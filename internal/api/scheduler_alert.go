package api

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
	"go.uber.org/zap"
)

// schedulerRetryableJobs 是可以在失败后自动重试一次的每日任务：重复执行不会产生额外
// 副作用（不会重发通知、不会重复封禁）。expiry_reminders 会重发提醒、
// enforce_group_membership 的失败（群组层级错误 / 熔断）需要人工判断、
// system_auto_update 与删除远端账号的任务都不在此列。
var schedulerRetryableJobs = map[string]bool{
	"check_expired":                     true,
	"check_expiring":                    true,
	"daily_stats":                       true,
	"cleanup_no_emby":                   true,
	"cleanup_pending_emby_entitlements": true,
	"check_telegram_bindings":           true,
	"cleanup_unused_uploads":            true,
	"cleanup_audit_logs":                true,
	"cleanup_ticket_images":             true,
	"auto_backup_database":              true,
}

const (
	schedulerRetryTrigger = "scheduler_retry"
	// schedulerAlertMinInterval：同一任务两次通知之间至少间隔这么久，防止间隔型任务抖动刷屏。
	schedulerAlertMinInterval = 10 * time.Minute
)

// schedulerRetryDue 判断每日任务是否该补一次失败重试：本日这轮自动执行失败、还没重试过、
// 距失败已超过 Scheduler.retry_failed_after_minutes。每个每日周期最多重试一次。
func (a *App) schedulerRetryDue(jobID string, spec map[string]any, now time.Time, snapshot store.SchedulerRunSnapshot) bool {
	retryAt := a.schedulerRetryAt(jobID, spec, now, snapshot)
	return retryAt > 0 && retryAt <= now.Unix()
}

// schedulerRetryAt shares retry eligibility between the worker and the admin list.
func (a *App) schedulerRetryAt(jobID string, spec map[string]any, now time.Time, snapshot store.SchedulerRunSnapshot) int64 {
	minutes := a.cfg().SchedulerRetryFailedAfterMinutes
	if minutes <= 0 || !schedulerRetryableJobs[jobID] || !snapshot.HasLatestAuto {
		return 0
	}
	if t := strings.ToLower(asString(spec["type"])); t != "cron_daily" && t != "daily" {
		return 0
	}
	if schedulerSnapshotRecentlyRunning(snapshot, now) {
		return 0
	}
	last := snapshot.LatestAuto
	if last.Status != "failed" || last.Trigger == schedulerRetryTrigger {
		return 0
	}
	// 只重试「今天这一轮」的失败，昨天遗留的失败不在今天补。
	hour := clamp(int(numeric(spec["hour"])), 0, 23)
	minute := clamp(int(numeric(spec["minute"])), 0, 59)
	dueToday := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location()).Unix()
	if last.StartedAt < dueToday {
		return 0
	}
	finished := last.FinishedAt
	if finished == 0 {
		finished = last.StartedAt
	}
	if finished <= 0 {
		return 0
	}
	retryAt := finished + int64(minutes)*60
	// Eligibility expires at the end of this local daily cycle. Do not advertise
	// a delayed retry that tomorrow's scheduler would deliberately skip.
	endOfDay := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location()).Unix()
	if retryAt >= endOfDay {
		return 0
	}
	return retryAt
}

// notifySchedulerOutcome 按配置通过 Telegram／邮件通知管理员任务失败与恢复。
// 节流：同一任务连续失败只在第一次通知（上一轮不是失败才发），且两次通知至少间隔
// schedulerAlertMinInterval。管理员主动取消的运行不通知。
func (a *App) notifySchedulerOutcome(st *store.Store, result store.SchedulerRun) {
	telegramEnabled := a.cfg().SchedulerNotifyTelegramEnabled && a.telegramAvailable()
	emailEnabled := a.cfg().SchedulerNotifyEmailEnabled && emailConfigured(a.cfg())
	if !a.cfg().SchedulerFailureNotify || (!telegramEnabled && !emailEnabled) || st == nil {
		return
	}
	failed := result.Status == "failed" || result.Status == "interrupted"
	if !failed && result.Status != "success" {
		return
	}
	previousFailed := false
	for _, run := range st.SchedulerRuns(result.JobID, 5) {
		if run.ID == result.ID || run.Status == "queued" || run.Status == "running" || run.Status == "cancel_requested" {
			continue
		}
		previousFailed = run.Status == "failed" || run.Status == "interrupted"
		break
	}
	var text string
	switch {
	case failed && !previousFailed:
		text = fmt.Sprintf("[失败] Twilight 定时任务\n任务：%s\n状态：%s\n原因：%s\n运行 ID：%d", schedulerJobName(result.JobID), result.Status, truncateString(firstNonEmpty(result.Error, result.Message), 300), result.ID)
	case !failed && previousFailed:
		text = fmt.Sprintf("[恢复] Twilight 定时任务已恢复正常\n任务：%s\n运行 ID：%d", schedulerJobName(result.JobID), result.ID)
	default:
		return
	}
	a.schedulerAlertMu.Lock()
	if a.schedulerAlertAt == nil {
		a.schedulerAlertAt = map[string]time.Time{}
	}
	// 只节流失败通知；恢复通知本身就只会在「上一轮失败」后出现一次。
	if last, ok := a.schedulerAlertAt[result.JobID]; failed && ok && time.Since(last) < schedulerAlertMinInterval {
		a.schedulerAlertMu.Unlock()
		return
	}
	a.schedulerAlertAt[result.JobID] = time.Now()
	a.schedulerAlertMu.Unlock()
	if emailEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		seen := map[string]bool{}
		adminUIDs, _ := st.UserUIDsMatching(0, func(u store.User) bool {
			return u.Role == store.RoleAdmin && a.notificationEmailAvailable(u)
		})
		admins := st.UsersByUIDs(adminUIDs)
		for _, uid := range adminUIDs {
			u, exists := admins[uid]
			if !exists {
				continue
			}
			if ctx.Err() != nil {
				break
			}
			if u.Role != store.RoleAdmin || !a.notificationEmailAvailable(u) {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(u.Email))
			if seen[key] {
				continue
			}
			seen[key] = true
			if err := smtpDeliver(ctx, *a.cfg(), u.Email, "Twilight 定时任务通知", redactSensitiveText(text)); err != nil {
				zap.L().Warn("failed to send scheduler email", zap.String("job_id", result.JobID), zap.Int64("uid", u.UID), zap.Error(err))
			}
		}
		cancel()
	}
	if !telegramEnabled {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, chatID := range a.schedulerAlertRecipients(st) {
		if err := a.telegramSendMessage(ctx, chatID, redactSensitiveText(text)); err != nil {
			zap.L().Warn("failed to send scheduler alert", zap.String("job_id", result.JobID), zap.String("error", a.telegramSanitizeError(err)))
		}
	}
}

// schedulerAlertRecipients 是配置里的 Telegram 管理员 ID，加上绑定了 Telegram 的管理员账号。
func (a *App) schedulerAlertRecipients(st *store.Store) []int64 {
	seen := map[int64]bool{}
	out := []int64{}
	for _, id := range a.cfg().TelegramAdminIDs {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, u := range st.ListUsers() {
		if u.Role == store.RoleAdmin && u.Active && u.TelegramID != 0 && !seen[u.TelegramID] {
			seen[u.TelegramID] = true
			out = append(out, u.TelegramID)
		}
	}
	return out
}

func schedulerJobName(jobID string) string {
	for _, job := range schedulerJobs {
		if asString(job["id"]) == jobID {
			return fmt.Sprintf("%s（%s）", asString(job["name"]), jobID)
		}
	}
	return jobID
}
