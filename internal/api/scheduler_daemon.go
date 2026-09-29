package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	// 内嵌时区数据库：精简镜像可能没有 /usr/share/zoneinfo，Scheduler.timezone 仍要能解析。
	_ "time/tzdata"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/prejudice-studio/twilight/internal/store"
)

const (
	schedulerRunningWindowSeconds      = int64(30 * 60)
	schedulerMaxPersistedLogLines      = 100
	schedulerMaxPersistedTextRunes     = 1024
	schedulerMaxPersistedErrorRunes    = 2048
	schedulerMaxPersistedSummaryItems  = 100
	schedulerMaxPersistedSummaryDepth  = 8
	schedulerSummaryTruncatedIndicator = "details_truncated"
)

func (a *App) RunScheduler(ctx context.Context) error {
	// The CLI closes shared stores only after this method returns. Keep leases
	// and persistence available until cancelled executions finish unwinding.
	defer a.schedulerWorkers.Wait()
	zap.L().Info("scheduler runner started")
	// 主循环 panic 兜底：reloadConfigIfChanged / runDueSchedulerJobs 调用栈深，
	// 一处空指针或 map race 会让整个 RunScheduler 协程退出，所有定时任务静默
	// 失效；之前只在 runScheduledJob / startManualSchedulerJob 协程入口加了
	// recover，daemon 主循环本身没保护。这里 recover 后退避重启循环，
	// 直到外层 ctx 真的 Done 才退出。
	for {
		err := a.runSchedulerLoop(ctx)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		zap.L().Error("scheduler loop crashed, restarting after backoff", zap.Error(err))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

// runSchedulerLoop 是单次主循环；panic 经 recover 后转 error 由 RunScheduler 重启。
func (a *App) runSchedulerLoop(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// panic value 可能携带敏感字段，强制走 redactSensitiveText 字符串路径，
			// 不走 zap.Any 的反射 dump。
			zap.L().Error("scheduler loop panic", zap.String("panic", redactSensitiveText(fmt.Sprintf("%v", r))))
			err = fmt.Errorf("scheduler loop panic")
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var nextDue time.Time
	var lastInterval int
	for {
		if ctx.Err() != nil {
			return nil
		}
		a.reloadConfigIfChanged()
		now := time.Now().In(a.schedulerLocation())
		interval := clamp(a.cfg().SchedulerTickIntervalSeconds, 10, 300)
		if interval != lastInterval {
			nextDue = time.Time{}
			lastInterval = interval
		}
		if !now.Before(nextDue) {
			a.runDueSchedulerJobs(ctx)
			nextDue = now.Add(time.Duration(clamp(a.cfg().SchedulerTickIntervalSeconds, 10, 300)) * time.Second)
		}
		a.claimSchedulerJobs(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (a *App) runDueSchedulerJobs(ctx context.Context) {
	if !a.cfg().SchedulerEnabled {
		return
	}
	if err := a.store().Refresh(); err != nil {
		zap.L().Warn("scheduler state unavailable")
		return
	}
	ids := make([]string, 0, len(schedulerJobs))
	for _, job := range schedulerJobs {
		if !boolish(job["manual_only"]) && schedulerJobEnabledByConfig(a.cfg().SystemUpdateEnabled, job) {
			ids = append(ids, fmt.Sprint(job["id"]))
		}
	}
	overview, err := a.store().ReadSchedulerOverview(ctx, ids, 20)
	if err != nil {
		zap.L().Warn("scheduler history unavailable")
		return
	}
	// cron_daily 的「几点」按 Scheduler.timezone 解释（schedulerJobDueFromSnapshot 用 now.Location()）。
	now := time.Now().In(a.schedulerLocation())
	for _, id := range ids {
		spec := a.schedulerDefaultTriggerSpec(id)
		schedule := overview.Schedules[id]
		if schedule.IsCustom {
			spec = schedule.TriggerSpec
		}
		if schedulerTriggerDisabled(spec) {
			continue
		}
		trigger := "scheduler"
		if !schedulerJobDueFromSnapshot(spec, now, overview.Runs[id]) {
			// 每日任务失败后不必等 24 小时：可安全重放的任务补一次重试。
			if !a.schedulerRetryDue(id, spec, now, overview.Runs[id]) {
				continue
			}
			trigger = schedulerRetryTrigger
		}
		last := overview.Runs[id].LatestAuto.ID
		params := a.schedulerRuntimeParamsFromSchedule(id, schedule.RuntimeParams)
		_, _, err = a.store().EnqueueSchedulerRun(ctx, store.SchedulerRun{JobID: id, Type: "auto", Trigger: trigger, Params: params, ScheduleRevision: schedule.Revision}, &last)
		if err != nil {
			zap.L().Warn("scheduler enqueue failed", zap.String("job_id", id))
		}
	}
}

// schedulerLocation 返回调度使用的时区。Scheduler.timezone 为空时用进程本地时区；
// 名字无效时同样回退本地时区并告警（只在值变化时告警一次）。
func (a *App) schedulerLocation() *time.Location {
	name := strings.TrimSpace(a.cfg().SchedulerTimezone)
	if name == "" {
		return time.Local
	}
	if cached := a.schedulerTZCache.Load(); cached != nil && cached.name == name {
		return cached.loc
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		zap.L().Warn("invalid Scheduler.timezone, falling back to local time", zap.String("timezone", name), zap.Error(err))
		loc = time.Local
	}
	a.schedulerTZCache.Store(&schedulerTZEntry{name: name, loc: loc})
	return loc
}

type schedulerTZEntry struct {
	name string
	loc  *time.Location
}

func schedulerJobEnabledByConfig(systemUpdateEnabled bool, job map[string]any) bool {
	if enabled, ok := job["enabled"].(bool); ok && !enabled {
		if fmt.Sprint(job["id"]) != "system_auto_update" || !systemUpdateEnabled {
			return false
		}
	}
	return true
}

func (a *App) schedulerJobDue(jobID string, spec map[string]any, now time.Time) bool {
	snapshot := a.store().SchedulerRunSnapshot(jobID, 20)
	return schedulerJobDueFromSnapshot(spec, now, snapshot)
}

// schedulerJobDueFromSnapshot 仅基于已取得的 snapshot 做 due 判定，不再触碰
// store，便于 daemon 单次 batch 拉取后对多个 job 复用。
func schedulerJobDueFromSnapshot(spec map[string]any, now time.Time, snapshot store.SchedulerRunSnapshot) bool {
	if schedulerSnapshotRecentlyRunning(snapshot, now) {
		return false
	}
	// 关键：last 必须从全量历史里取最新 auto run，否则 admin 在窗口内对同
	// job 手动重跑 21 次会把 auto 记录挤出 SchedulerRuns(20)，进而把 last
	// 退化为 0，cron_daily 路径会判定"今天还没跑过 auto"再起一次。
	last := int64(0)
	if snapshot.HasLatestAuto {
		last = snapshot.LatestAuto.StartedAt
		if last == 0 {
			last = snapshot.LatestAuto.CreatedAt
		}
	}
	switch strings.ToLower(asString(spec["type"])) {
	case "cron_daily", "daily":
		hour := clamp(int(numeric(spec["hour"])), 0, 23)
		minute := clamp(int(numeric(spec["minute"])), 0, 59)
		due := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		return !now.Before(due) && last < due.Unix()
	case "interval":
		seconds := clamp(int(numeric(spec["seconds"])), 60, 604800)
		return last == 0 || now.Unix()-last >= int64(seconds)
	default:
		return false
	}
}

func schedulerSnapshotRecentlyRunning(snapshot store.SchedulerRunSnapshot, now time.Time) bool {
	return snapshot.HasLatestRunning && (snapshot.LatestRunning.Status == "queued" || snapshot.LatestRunning.LeaseUntil > now.Unix() || (snapshot.LatestRunning.LeaseUntil == 0 && now.Unix()-snapshot.LatestRunning.StartedAt < schedulerRunningWindowSeconds))
}

// Compatibility entry points enqueue only. A running scheduler/all worker owns execution.
func (a *App) runScheduledJob(ctx context.Context, jobID string) {
	_, _, err := a.enqueueSchedulerJob(ctx, jobID, nil, "auto")
	if err != nil {
		zap.L().Warn("scheduler enqueue failed", zap.String("job_id", jobID))
	}
}

func (a *App) startManualSchedulerJob(ctx context.Context, jobID string, params map[string]any) (store.SchedulerRun, bool) {
	run, created, err := a.enqueueSchedulerJob(ctx, jobID, params, "manual")
	if err != nil {
		zap.L().Warn("scheduler enqueue failed", zap.String("job_id", jobID))
	}
	return run, created && err == nil
}

func schedulerFinishedRun(jobID, runType, trigger string, started int64, summary map[string]any, logs []string, err error) store.SchedulerRun {
	status := "success"
	message := "job completed"
	errText := ""
	if err != nil {
		status = "failed"
		// SchedulerRun.Message / Error 会被 PG INSERT 持久化，admin 后台直接
		// 显示。job 内部错误链常包含 git remote URL（含明文 PAT）、emby
		// /Auth 响应（含 password fragment）、telegram API 错误（含 bot
		// token URL）等敏感字段。统一走 redactSensitiveText。
		message, _ = sanitizeSchedulerText(err.Error(), schedulerMaxPersistedErrorRunes)
		errText = message
		if errors.Is(err, context.Canceled) {
			message = "job terminated by administrator"
			errText = message
			if summary == nil {
				summary = map[string]any{}
			}
			summary["success"] = false
			summary["terminated"] = true
		}
	}
	// 任务没有返回 error、但摘要明确报告 success=false（例如部分用户处理失败、
	// Emby 停用失败）时，旧实现仍显示「成功」，管理员看不到问题。这里一律按失败处理。
	if err == nil && schedulerSummaryReportsFailure(summary) {
		status = "failed"
		message, _ = sanitizeSchedulerText(firstNonEmpty(asString(summary["error"]), "job reported failure (success=false)"), schedulerMaxPersistedErrorRunes)
		errText = message
	}
	finished := time.Now().Unix()
	return store.SchedulerRun{
		JobID:      jobID,
		Type:       runType,
		Trigger:    trigger,
		Status:     status,
		Message:    message,
		StartedAt:  started,
		FinishedAt: finished,
		EndedAt:    finished,
		Summary:    sanitizeSchedulerSummary(summary),
		Logs:       sanitizeSchedulerLogs(logs),
		Error:      errText,
	}
}

// schedulerSummaryReportsFailure 只认显式的 success=false；没有这个键的摘要不算失败。
func schedulerSummaryReportsFailure(summary map[string]any) bool {
	if summary == nil {
		return false
	}
	value, ok := summary["success"]
	return ok && !boolish(value)
}

func sanitizeSchedulerSummary(summary map[string]any) map[string]any {
	if summary == nil {
		return nil
	}
	sanitizedValue, truncated := sanitizeSchedulerValue(summary, 0)
	sanitized, _ := sanitizedValue.(map[string]any)
	if truncated {
		sanitized[schedulerSummaryTruncatedIndicator] = true
	}
	return sanitized
}

func sanitizeSchedulerLogs(logs []string) []string {
	if len(logs) == 0 {
		return nil
	}
	limit := len(logs)
	if limit > schedulerMaxPersistedLogLines {
		limit = schedulerMaxPersistedLogLines - 1
	}
	out := make([]string, 0, min(len(logs), schedulerMaxPersistedLogLines))
	for _, logLine := range logs[:limit] {
		line, _ := sanitizeSchedulerText(logLine, schedulerMaxPersistedTextRunes)
		out = append(out, line)
	}
	if len(logs) > limit {
		out = append(out, fmt.Sprintf("[truncated %d additional scheduler log lines]", len(logs)-limit))
	}
	return out
}

func sanitizeSchedulerText(value string, limit int) (string, bool) {
	value = redactSensitiveText(value)
	// 与 truncateString 同口径的快路径：字节长度已 <= limit 时 rune 数必然 <= limit，
	// 避免每次日志行都分配 rune 切片计数（这里每轮调度逐行调用，分配压力放大）。
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	return truncateString(value, limit) + " [truncated]", true
}

func sanitizeSchedulerValue(value any, depth int) (any, bool) {
	if depth >= schedulerMaxPersistedSummaryDepth {
		switch value.(type) {
		case []string:
			return []string{"[truncated]"}, true
		case []any:
			return []any{"[truncated]"}, true
		case []map[string]any:
			return []map[string]any{{schedulerSummaryTruncatedIndicator: true}}, true
		case map[string]any:
			return map[string]any{schedulerSummaryTruncatedIndicator: true}, true
		case map[string]string:
			return map[string]string{schedulerSummaryTruncatedIndicator: "true"}, true
		}
	}
	switch v := value.(type) {
	case string:
		return sanitizeSchedulerText(v, schedulerMaxPersistedTextRunes)
	case []string:
		limit := min(len(v), schedulerMaxPersistedSummaryItems)
		out := make([]string, 0, limit)
		truncated := len(v) > limit
		for _, item := range v[:limit] {
			sanitized, itemTruncated := sanitizeSchedulerText(item, schedulerMaxPersistedTextRunes)
			out = append(out, sanitized)
			truncated = truncated || itemTruncated
		}
		return out, truncated
	case []any:
		limit := min(len(v), schedulerMaxPersistedSummaryItems)
		out := make([]any, 0, limit)
		truncated := len(v) > limit
		for _, item := range v[:limit] {
			sanitized, itemTruncated := sanitizeSchedulerValue(item, depth+1)
			out = append(out, sanitized)
			truncated = truncated || itemTruncated
		}
		return out, truncated
	case []map[string]any:
		limit := min(len(v), schedulerMaxPersistedSummaryItems)
		out := make([]map[string]any, 0, limit)
		truncated := len(v) > limit
		for _, item := range v[:limit] {
			sanitized, itemTruncated := sanitizeSchedulerValue(item, depth+1)
			mapped, _ := sanitized.(map[string]any)
			out = append(out, mapped)
			truncated = truncated || itemTruncated
		}
		return out, truncated
	case map[string]any:
		out := make(map[string]any, len(v))
		truncated := false
		for key, item := range v {
			sanitized, itemTruncated := sanitizeSchedulerValue(item, depth+1)
			out[key] = sanitized
			truncated = truncated || itemTruncated
		}
		return out, truncated
	case map[string]string:
		out := make(map[string]string, len(v))
		truncated := false
		for key, item := range v {
			sanitized, itemTruncated := sanitizeSchedulerText(item, schedulerMaxPersistedTextRunes)
			out[key] = sanitized
			truncated = truncated || itemTruncated
		}
		return out, truncated
	default:
		return value, false
	}
}

func (a *App) schedulerTriggerSpec(jobID string) map[string]any {
	if schedule, ok := a.store().SchedulerSchedule(jobID); ok && len(schedule.TriggerSpec) > 0 {
		return schedule.TriggerSpec
	}
	return a.schedulerDefaultTriggerSpec(jobID)
}

func schedulerTriggerDisabled(spec map[string]any) bool {
	return strings.EqualFold(asString(spec["type"]), "manual")
}

func (a *App) schedulerNextRunAt(jobID string, spec map[string]any, now time.Time) int64 {
	// 与 schedulerJobDue 对齐：从全量历史里取最新 auto run，避免被 manual
	// 重跑挤出 SchedulerRuns(20) 时把 last 退化成 0，让前端"下次自动运行"
	// 时间显示成 now / 当天而不是真正的次日。
	snapshot := a.store().SchedulerRunSnapshot(jobID, 1)
	return a.schedulerNextAutomaticRunAt(jobID, spec, now, snapshot)
}

func (a *App) schedulerNextAutomaticRunAt(jobID string, spec map[string]any, now time.Time, snapshot store.SchedulerRunSnapshot) int64 {
	if !a.cfg().SchedulerEnabled {
		return 0
	}
	next := schedulerNextRunAtFromSnapshot(spec, now, snapshot)
	if next == 0 {
		return 0
	}
	if retryAt := a.schedulerRetryAt(jobID, spec, now, snapshot); retryAt > 0 && retryAt < next {
		next = retryAt
		if next < now.Unix() {
			next = now.Unix()
		}
	}
	return next
}

func schedulerNextRunAtFromSnapshot(spec map[string]any, now time.Time, snapshot store.SchedulerRunSnapshot) int64 {
	if schedulerTriggerDisabled(spec) || schedulerSnapshotRecentlyRunning(snapshot, now) {
		return 0
	}
	last := int64(0)
	if snapshot.HasLatestAuto {
		last = snapshot.LatestAuto.StartedAt
		if last == 0 {
			last = snapshot.LatestAuto.CreatedAt
		}
	}
	switch strings.ToLower(asString(spec["type"])) {
	case "cron_daily", "daily":
		hour := clamp(int(numeric(spec["hour"])), 0, 23)
		minute := clamp(int(numeric(spec["minute"])), 0, 59)
		due := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if last >= due.Unix() {
			due = time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, now.Location())
		}
		// An overdue cycle is queued on the next scheduler tick, not tomorrow.
		if now.After(due) {
			return now.Unix()
		}
		return due.Unix()
	case "interval":
		seconds := clamp(int(numeric(spec["seconds"])), 60, 604800)
		if last == 0 || last+int64(seconds) < now.Unix() {
			return now.Unix()
		}
		return last + int64(seconds)
	default:
		return 0
	}
}

func (a *App) schedulerDefaultTriggerSpec(jobID string) map[string]any {
	switch jobID {
	case "check_expired":
		return dailySpec(a.cfg().SchedulerExpiredCheckTime, 3, 0)
	case "check_expiring", "expiry_reminders":
		return dailySpec(a.cfg().SchedulerExpiringCheckTime, 9, 0)
	case "daily_stats":
		return dailySpec(a.cfg().SchedulerDailyStatsTime, 0, 5)
	case "cleanup_sessions":
		hours := a.cfg().SchedulerSessionCleanupInterval
		if hours <= 0 {
			hours = 6
		}
		return map[string]any{"type": "interval", "seconds": hours * 3600}
	case "emby_state_reconcile":
		hours := a.cfg().SchedulerEmbyReconcileInterval
		if hours <= 0 {
			hours = 6
		}
		return map[string]any{"type": "interval", "seconds": hours * 3600}
	case "cleanup_no_emby":
		return dailySpec(a.cfg().SchedulerCleanupNoEmbyTime, 3, 30)
	case "cleanup_pending_emby_entitlements":
		return dailySpec(a.cfg().SchedulerCleanupPendingEmbyTime, 3, 45)
	case "system_auto_update":
		switch strings.ToLower(strings.TrimSpace(a.cfg().SystemUpdateTriggerType)) {
		case "daily", "cron_daily":
			return dailySpec(a.cfg().SystemUpdateTime, 4, 0)
		case "manual":
			return map[string]any{"type": "manual"}
		default:
			hours := a.cfg().SystemUpdateIntervalHours
			if hours <= 0 {
				hours = 24
			}
			return map[string]any{"type": "interval", "seconds": hours * 3600}
		}
	case "cleanup_unlinked_emby":
		// 删除远端账号的维护任务默认不自动执行（旧实现列表却显示 05:00 的下次时间）。
		// 管理员在后台保存自定义排程即表示启用自动执行（仍默认仅扫描不删除）。
		return map[string]any{"type": "manual"}
	case "auto_backup_database":
		return dailySpec(a.cfg().SchedulerAutoBackupTime, 4, 15)
	case "enforce_group_membership":
		return dailySpec(a.cfg().SchedulerGroupMembershipCheckTime, 3, 10)
	case "check_telegram_bindings":
		return dailySpec(a.cfg().SchedulerTelegramBindingsCheckTime, 3, 20)
	case "emby_sync", "cleanup_emby_devices", "kick_unknown_group_members":
		return map[string]any{"type": "manual"}
	case "cleanup_unused_uploads":
		return dailySpec(a.cfg().SchedulerCleanupUnusedUploadsTime, 2, 20)
	case "cleanup_audit_logs":
		return dailySpec(a.cfg().SchedulerCleanupAuditLogsTime, 4, 30)
	case "cleanup_ticket_images":
		return dailySpec(a.cfg().SchedulerCleanupTicketImagesTime, 4, 45)
	case "refresh_bangumi_collections":
		return map[string]any{"type": "interval", "seconds": 3600}
	case "sync_emby_activity_logs":
		return map[string]any{"type": "interval", "seconds": 600}
	default:
		return dailySpec("03:00", 3, 0)
	}
}

func dailySpec(value string, fallbackHour, fallbackMinute int) map[string]any {
	hour, minute := parseClock(value, fallbackHour, fallbackMinute)
	return map[string]any{"type": "cron_daily", "hour": hour, "minute": minute}
}

func parseClock(value string, fallbackHour, fallbackMinute int) (int, int) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return fallbackHour, fallbackMinute
	}
	hour, errH := strconv.Atoi(strings.TrimSpace(parts[0]))
	minute, errM := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errH != nil || errM != nil {
		return fallbackHour, fallbackMinute
	}
	return clamp(hour, 0, 23), clamp(minute, 0, 59)
}

type schedulerProcessRun struct {
	cancel     context.CancelFunc
	started    int64
	runID      atomic.Int64
	terminated atomic.Bool
}

// schedulerProcessLocks 已迁移到 App.schedulerLocks（app.go）。原本是 package
// 级 sync.Map，单进程 prod 没问题，但测试 setup 反复 New() 出多个 App 时
// 该表跨实例共享，会让一个 case 的 cancel 影响另一 case 的 LoadOrStore，
// 偶发 flake。改为 instance 字段后每个 App 自带独立锁。

func (a *App) startSchedulerRun(ctx context.Context, jobID string) (context.Context, *schedulerProcessRun, func(), bool) {
	runCtx, cancel := context.WithCancel(ctx)
	run := &schedulerProcessRun{cancel: cancel, started: time.Now().Unix()}
	actual, loaded := a.schedulerLocks.LoadOrStore(jobID, run)
	if loaded {
		cancel()
		_ = actual
		return ctx, nil, func() {}, false
	}
	finish := func() {
		cancel()
		if current, ok := a.schedulerLocks.Load(jobID); ok && current == run {
			a.schedulerLocks.Delete(jobID)
		}
	}
	return runCtx, run, finish, true
}

func (a *App) schedulerJobRunning(jobID string) bool {
	_, ok := a.schedulerLocks.Load(jobID)
	return ok
}

func (a *App) terminateSchedulerJob(jobID string) bool {
	value, ok := a.schedulerLocks.Load(jobID)
	if !ok {
		return false
	}
	run, ok := value.(*schedulerProcessRun)
	if !ok || run.cancel == nil {
		return false
	}
	run.terminated.Store(true)
	run.cancel()

	return true
}
