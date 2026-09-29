package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

const (
	telegramMembershipSchedulerCheckTimeout = 8 * time.Second
	telegramMembershipMaxCheckConcurrency   = 64
)

type telegramMembershipCheckResult struct {
	telegramID int64
	missing    []string
	updates    []store.TelegramRosterUpdate
	err        error
}

// telegramMembershipOptions 是群成员巡检的运行参数。
type telegramMembershipOptions struct {
	AutoEnableRejoined bool
	// DryRun 只计算会停用 / 会启用的名单，不做任何写入。
	DryRun bool
	// BreakerPercent / BreakerMax 是熔断阈值，0 表示关闭该项。
	BreakerPercent int
	BreakerMax     int
}

// telegramMembershipBreakerMinCount：拟停用人数少于该值时不按百分比熔断，避免
// 小站 3 个用户走 1 个（33%）就误触发。绝对人数阈值不受此限制。
const telegramMembershipBreakerMinCount = 3

func (a *App) enforceTelegramMembership(ctx context.Context, autoEnableRejoined bool) (map[string]any, []string, error) {
	return a.enforceTelegramMembershipWithOptions(ctx, telegramMembershipOptions{
		AutoEnableRejoined: autoEnableRejoined,
		BreakerPercent:     a.cfg().TelegramMembershipBreakerPercent,
		BreakerMax:         a.cfg().TelegramMembershipBreakerMax,
	})
}

// telegramMembershipBreakerTripped 判断本轮拟停用人数是否触发熔断。
func telegramMembershipBreakerTripped(toDisable, scanned, percent, max int) bool {
	if toDisable <= 0 {
		return false
	}
	if max > 0 && toDisable > max {
		return true
	}
	if percent > 0 && scanned > 0 && toDisable >= telegramMembershipBreakerMinCount && toDisable*100 > percent*scanned {
		return true
	}
	return false
}

func (a *App) enforceTelegramMembershipWithOptions(ctx context.Context, opts telegramMembershipOptions) (map[string]any, []string, error) {
	autoEnableRejoined := opts.AutoEnableRejoined
	chats := telegramChatIDs(a.cfg().TelegramGroupIDs)
	result := map[string]any{
		"enabled": false, "telegram_available": a.telegramAvailable(), "groups": chats,
		"scanned": 0, "disabled": 0, "emby_disabled": 0, "banned": 0, "rejoined_enabled": 0,
		"rejoined_pending_review": 0, "rejoin_candidates": 0, "skipped": 0, "failed": 0,
		"rebind_protected": 0, "rebind_pending": 0, "rebind_approved": 0, "rebinding_in_progress": 0,
		"auto_enable_rejoined": autoEnableRejoined, "dry_run": opts.DryRun,
		"breaker_percent": opts.BreakerPercent, "breaker_max": opts.BreakerMax, "circuit_breaker_tripped": false,
	}
	rejoinCandidates := []map[string]any{}
	logs := []string{}
	if !a.cfg().TelegramRequireMembership || len(chats) == 0 {
		logs = append(logs, "Telegram membership enforcement disabled")
		return result, logs, nil
	}
	result["enabled"] = true
	if !a.telegramAvailable() {
		logs = append(logs, "Telegram unavailable; membership enforcement skipped")
		return result, logs, nil
	}
	now := time.Now().Unix()
	candidates := []store.User{}
	uniqueTelegramIDs := []int64{}
	seenTelegramIDs := map[int64]bool{}
	bannedUsers := []map[string]any{}
	rebindProtections := a.store().TelegramMembershipRebindProtections()
	for _, u := range a.store().ListUsers() {
		if err := ctx.Err(); err != nil {
			result["terminated"] = true
			return result, append(logs, "job terminated"), err
		}
		if u.TelegramID == 0 || a.userIsProtected(u) {
			result["skipped"] = int(numeric(result["skipped"])) + 1
			continue
		}
		if reason := rebindProtections[u.UID]; reason != "" {
			recordTelegramMembershipRebindProtection(result, reason)
			if len(logs) < 50 {
				logs = append(logs, fmt.Sprintf("rebind-protected uid=%d username=%s state=%s", u.UID, u.Username, reason))
			}
			continue
		}
		result["scanned"] = int(numeric(result["scanned"])) + 1
		candidates = append(candidates, u)
		if !seenTelegramIDs[u.TelegramID] {
			seenTelegramIDs[u.TelegramID] = true
			uniqueTelegramIDs = append(uniqueTelegramIDs, u.TelegramID)
		}
	}
	concurrency := a.telegramMembershipCheckConcurrency(len(uniqueTelegramIDs))
	result["unique_telegram_ids"] = len(uniqueTelegramIDs)
	result["concurrency"] = concurrency
	checks, rosterUpdates, err := a.checkTelegramMemberships(ctx, uniqueTelegramIDs, chats, concurrency)
	if err != nil {
		result["terminated"] = true
		return result, append(logs, "job terminated"), err
	}
	// 群组层级错误（群 ID 配错、Bot 被踢或没有权限）：任何一个用户的检查命中就说明
	// 本轮结果不可信，整轮中止并以失败回报，不停用、不启用、也不写花名册。
	for _, check := range checks {
		var chatErr *telegramChatLevelError
		if errors.As(check.err, &chatErr) {
			result["aborted"] = true
			result["abort_reason"] = "chat_level_error"
			result["abort_chat_id"] = chatErr.ChatID
			return result, append(logs, "aborted: "+redactSensitiveText(chatErr.Error())), chatErr
		}
	}
	if !opts.DryRun {
		if err := a.store().ApplyTelegramRosterUpdates(rosterUpdates); err != nil {
			result["failed"] = int(numeric(result["failed"])) + 1
			if len(logs) < 50 {
				logs = append(logs, "failed to update telegram roster: "+err.Error())
			}
		}
	}
	// 先算出完整计划，再统一判断熔断，最后才动手。这样熔断触发时一个用户都不会被停用。
	type membershipDisablePlan struct {
		user    store.User
		missing []string
	}
	toDisable := []membershipDisablePlan{}
	toRejoin := []store.User{}
	for _, u := range candidates {
		if err := ctx.Err(); err != nil {
			result["terminated"] = true
			return result, append(logs, "job terminated"), err
		}
		check, ok := checks[u.TelegramID]
		if !ok {
			result["failed"] = int(numeric(result["failed"])) + 1
			if len(logs) < 50 {
				logs = append(logs, fmt.Sprintf("failed to check uid=%d tg=%d: missing check result", u.UID, u.TelegramID))
			}
			continue
		}
		missing, err := check.missing, check.err
		if err != nil {
			result["failed"] = int(numeric(result["failed"])) + 1
			if len(logs) < 50 {
				logs = append(logs, fmt.Sprintf("failed to check uid=%d tg=%d: %s", u.UID, u.TelegramID, redactSensitiveText(err.Error())))
			}
			continue
		}
		if u.Active && len(missing) > 0 {
			toDisable = append(toDisable, membershipDisablePlan{user: u, missing: missing})
			continue
		}
		if !u.Active && len(missing) == 0 && (u.ExpiredAt <= 0 || u.ExpiredAt > now) {
			// 只有「因退群被巡检停用」的账号才进入回群流程；管理员手动停权或其他原因
			// 停用的账号即使人在群里也不处理，避免被自动放出来。
			if u.DisabledReason != store.DisabledReasonTelegramMembership {
				result["skipped_other_disabled"] = int(numeric(result["skipped_other_disabled"])) + 1
				continue
			}
			toRejoin = append(toRejoin, u)
		}
	}
	scanned := int(numeric(result["scanned"]))
	result["would_disable"] = len(toDisable)
	if telegramMembershipBreakerTripped(len(toDisable), scanned, opts.BreakerPercent, opts.BreakerMax) {
		result["circuit_breaker_tripped"] = true
		result["aborted"] = true
		result["abort_reason"] = "circuit_breaker"
		result["would_disable_uids"] = membershipPlanUIDs(len(toDisable), func(i int) int64 { return toDisable[i].user.UID })
		msg := fmt.Sprintf("群成员巡检熔断：本轮拟停用 %d / %d 人，超过阈值（%d%% 或 %d 人），已中止且未停用任何用户；请检查群 ID 与 Bot 权限，确认无误后可调高阈值再手动执行", len(toDisable), scanned, opts.BreakerPercent, opts.BreakerMax)
		return result, append(logs, msg), errors.New(msg)
	}
	if opts.DryRun {
		result["would_disable_uids"] = membershipPlanUIDs(len(toDisable), func(i int) int64 { return toDisable[i].user.UID })
		result["would_review_rejoin"] = len(toRejoin)
		result["would_review_rejoin_uids"] = membershipPlanUIDs(len(toRejoin), func(i int) int64 { return toRejoin[i].UID })
		for _, plan := range toDisable {
			if len(logs) >= 50 {
				break
			}
			logs = append(logs, fmt.Sprintf("dry-run: would disable uid=%d username=%s missing=%s", plan.user.UID, plan.user.Username, strings.Join(plan.missing, ",")))
		}
		return result, append(logs, fmt.Sprintf("dry-run: would disable %d users", len(toDisable))), nil
	}
	disabledUsers := []map[string]any{}
	for _, plan := range toDisable {
		u, missing := plan.user, plan.missing
		if err := ctx.Err(); err != nil {
			result["terminated"] = true
			return result, append(logs, "job terminated"), err
		}
		updated, disabledNow, rebindReason, err := a.store().DisableUserForTelegramMembership(u.UID)
		if err != nil {
			result["failed"] = int(numeric(result["failed"])) + 1
			continue
		}
		if rebindReason != "" {
			recordTelegramMembershipRebindProtection(result, rebindReason)
			if len(logs) < 50 {
				logs = append(logs, fmt.Sprintf("rebind-protected before disable uid=%d username=%s state=%s", updated.UID, updated.Username, rebindReason))
			}
			continue
		}
		if !disabledNow {
			continue
		}
		// 立即清除该用户所有 session（redis + memory + PG）。否则 stale token
		// 在 SessionTTL 到期前都还能访问受保护接口。
		sideCtx, sideCancel := schedulerSideEffectContext(ctx)
		embyDisabledNow := false
		if disabledRemote, err := a.disableRemoteEmbyForWebState(sideCtx, updated); err == nil && disabledRemote {
			result["emby_disabled"] = int(numeric(result["emby_disabled"])) + 1
			embyDisabledNow = true
		}
		a.sessions().DeleteUser(sideCtx, updated.UID)
		result["disabled"] = int(numeric(result["disabled"])) + 1
		sideCancel()
		if len(disabledUsers) < auditUIDListLimit {
			disabledUsers = append(disabledUsers, map[string]any{"uid": updated.UID, "username": updated.Username, "telegram_id": updated.TelegramID, "missing": missing, "emby_disabled": embyDisabledNow})
		}
		if a.cfg().TelegramBanOnLeave {
			for _, chatID := range chats {
				if err := a.telegramBanChatMember(ctx, chatID, updated.TelegramID); err == nil {
					result["banned"] = int(numeric(result["banned"])) + 1
					bannedUsers = append(bannedUsers, map[string]any{"uid": updated.UID, "username": updated.Username, "telegram_id": updated.TelegramID, "chat_id": chatID})
				}
			}
		}
		if len(logs) < 50 {
			logs = append(logs, fmt.Sprintf("disabled uid=%d username=%s missing=%s", updated.UID, updated.Username, strings.Join(missing, ",")))
		}
	}
	rejoinedUsers := []map[string]any{}
	for _, u := range toRejoin {
		if autoEnableRejoined && !a.cfg().TelegramBanOnLeave {
			// 在写锁内再确认一次停用原因：巡检期间管理员可能已手动改过状态。
			updated, err := a.store().UpdateUser(u.UID, func(u *store.User) error {
				if u.Active || u.DisabledReason != store.DisabledReasonTelegramMembership {
					return errTelegramRejoinNoLongerEligible
				}
				u.Active = true
				return nil
			})
			if errors.Is(err, errTelegramRejoinNoLongerEligible) {
				result["skipped_other_disabled"] = int(numeric(result["skipped_other_disabled"])) + 1
				continue
			}
			if err != nil {
				result["failed"] = int(numeric(result["failed"])) + 1
				continue
			}
			result["rejoined_enabled"] = int(numeric(result["rejoined_enabled"])) + 1
			// 退群时 Emby 是随 Web 一起被系统停用的（EmbyAutoDisabled），回群时一并恢复；
			// 管理员单独封禁的 Emby（无该标记）保持不动。失败留给 Emby 状态对账任务收敛。
			embyEnabled := false
			if updated.EmbyID != "" && updated.EmbyDisabled && updated.EmbyAutoDisabled && a.embyConfigured() && a.embyShouldEnableUser(updated) {
				sideCtx, sideCancel := schedulerSideEffectContext(ctx)
				if err := embyRetryOn5xx(sideCtx, func(c context.Context) error {
					return a.embyApplyEnabledState(c, updated.UID, updated.EmbyID, true)
				}); err != nil {
					result["rejoined_emby_enable_failed"] = int(numeric(result["rejoined_emby_enable_failed"])) + 1
					if len(logs) < 50 {
						logs = append(logs, fmt.Sprintf("failed to re-enable Emby uid=%d: %s", updated.UID, redactSensitiveText(err.Error())))
					}
				} else {
					embyEnabled = true
					result["rejoined_emby_enabled"] = int(numeric(result["rejoined_emby_enabled"])) + 1
				}
				sideCancel()
			}
			if len(rejoinedUsers) < auditUIDListLimit {
				rejoinedUsers = append(rejoinedUsers, map[string]any{"uid": updated.UID, "username": updated.Username, "telegram_id": updated.TelegramID, "emby_enabled": embyEnabled})
			}
			if len(logs) < 50 {
				logs = append(logs, fmt.Sprintf("re-enabled uid=%d username=%s", updated.UID, updated.Username))
			}
			continue
		}
		result["rejoined_pending_review"] = int(numeric(result["rejoined_pending_review"])) + 1
		result["rejoin_candidates"] = int(numeric(result["rejoin_candidates"])) + 1
		if len(rejoinCandidates) < 200 {
			rejoinCandidates = append(rejoinCandidates, map[string]any{"uid": u.UID, "username": u.Username, "telegram_id": u.TelegramID, "emby_bound": u.EmbyID != "", "expired_at": zeroNil(u.ExpiredAt)})
		}
		if len(logs) < 50 {
			logs = append(logs, fmt.Sprintf("rejoin pending review uid=%d username=%s", u.UID, u.Username))
		}
	}
	if len(rejoinCandidates) > 0 {
		result["rejoin_candidate_users"] = rejoinCandidates
	}
	// 退群停用与回群启用都会改用户状态，必须写系统稽核并附 uid 清单，供管理员追查。
	if len(disabledUsers) > 0 {
		a.auditSystem("scheduler", "disable_telegram_users_on_leave", 0, map[string]any{
			"disabled":      int(numeric(result["disabled"])),
			"emby_disabled": int(numeric(result["emby_disabled"])),
			"users":         disabledUsers,
		})
	}
	if len(rejoinedUsers) > 0 {
		a.auditSystem("scheduler", "enable_telegram_users_on_rejoin", 0, map[string]any{
			"enabled": int(numeric(result["rejoined_enabled"])),
			"users":   rejoinedUsers,
		})
	}
	// 退群封禁属于安全敏感的系统副作用，必须计入系统日志（审计）供管理员追查。
	if len(bannedUsers) > 0 {
		a.auditSystem("scheduler", "ban_telegram_users_on_leave", 0, map[string]any{
			"banned": int(numeric(result["banned"])),
			"users":  bannedUsers,
		})
	}
	return result, logs, nil
}

// errTelegramRejoinNoLongerEligible 表示写锁内复查时账号已不再是「因退群停用」。
var errTelegramRejoinNoLongerEligible = errors.New("telegram rejoin no longer eligible")

// auditUIDListLimit 是系统稽核 detail 里名单的上限，避免超出 8KB detail 限制。
const auditUIDListLimit = 64

// membershipPlanUIDs 取前 200 个 uid 放进运行摘要。
func membershipPlanUIDs(n int, uidAt func(int) int64) []int64 {
	limit := min(n, 200)
	out := make([]int64, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, uidAt(i))
	}
	return out
}

func recordTelegramMembershipRebindProtection(result map[string]any, reason string) {
	result["skipped"] = int(numeric(result["skipped"])) + 1
	result["rebind_protected"] = int(numeric(result["rebind_protected"])) + 1
	switch reason {
	case "pending":
		result["rebind_pending"] = int(numeric(result["rebind_pending"])) + 1
	case "approved":
		result["rebind_approved"] = int(numeric(result["rebind_approved"])) + 1
	case "in_progress":
		result["rebinding_in_progress"] = int(numeric(result["rebinding_in_progress"])) + 1
	}
}

func (a *App) telegramMembershipCheckConcurrency(total int) int {
	if total <= 0 {
		return 0
	}
	concurrency := a.cfg().TelegramGroupCheckConcurrency
	if concurrency <= 0 {
		concurrency = 24
	}
	concurrency = clamp(concurrency, 1, telegramMembershipMaxCheckConcurrency)
	if concurrency > total {
		return total
	}
	return concurrency
}

func (a *App) checkTelegramMemberships(ctx context.Context, telegramIDs []int64, chats []string, concurrency int) (map[int64]telegramMembershipCheckResult, []store.TelegramRosterUpdate, error) {
	checks := make(map[int64]telegramMembershipCheckResult, len(telegramIDs))
	if len(telegramIDs) == 0 {
		return checks, nil, nil
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	jobs := make(chan int64)
	results := make(chan telegramMembershipCheckResult, len(telegramIDs))
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for telegramID := range jobs {
				if err := ctx.Err(); err != nil {
					results <- telegramMembershipCheckResult{telegramID: telegramID, err: err}
					continue
				}
				missing, updates, err := a.telegramMembershipMissingForScheduler(ctx, telegramID, chats)
				results <- telegramMembershipCheckResult{telegramID: telegramID, missing: missing, updates: updates, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, telegramID := range telegramIDs {
			select {
			case <-ctx.Done():
				return
			case jobs <- telegramID:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	rosterUpdates := []store.TelegramRosterUpdate{}
	for result := range results {
		checks[result.telegramID] = result
		rosterUpdates = append(rosterUpdates, result.updates...)
	}
	if err := ctx.Err(); err != nil {
		return checks, rosterUpdates, err
	}
	return checks, rosterUpdates, nil
}

func (a *App) telegramMembershipMissingForScheduler(ctx context.Context, telegramID int64, chats []string) ([]string, []store.TelegramRosterUpdate, error) {
	missing := []string{}
	updates := []store.TelegramRosterUpdate{}
	if len(chats) == 0 || telegramID == 0 {
		return missing, updates, nil
	}
	for _, chatID := range chats {
		if err := ctx.Err(); err != nil {
			return missing, updates, err
		}
		member, err := a.telegramGetChatMemberWithTimeout(ctx, chatID, telegramID, telegramMembershipSchedulerCheckTimeout)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return missing, updates, ctxErr
			}
			// 只把明确的用户层级错误当成不在群；群组层级错误（群 ID 错、Bot 被踢）
			// 原样上抛，由 enforceTelegramMembership 中止整轮。
			switch classifyTelegramMembershipError(err) {
			case telegramMembershipErrUserMissing:
				missing = append(missing, chatID)
				updates = append(updates, store.TelegramRosterUpdate{ChatID: chatID, TelegramID: telegramID, Status: "left"})
				continue
			case telegramMembershipErrChatLevel:
				return missing, updates, &telegramChatLevelError{ChatID: chatID, Err: err}
			}
			if !telegramRateLimitPauseContext(ctx, err) {
				return missing, updates, ctx.Err()
			}
			return missing, updates, err
		}
		status := strings.ToLower(member.Status)
		if status == "left" || status == "kicked" {
			missing = append(missing, chatID)
			updates = append(updates, store.TelegramRosterUpdate{ChatID: chatID, TelegramID: telegramID, Status: status})
			continue
		}
		updates = append(updates, store.TelegramRosterUpdate{ChatID: chatID, TelegramID: telegramID, Status: firstNonEmpty(status, "member"), IsBot: member.User.IsBot})
	}
	return missing, updates, nil
}

func (a *App) cleanupUnusedUploadAssets(maxAge time.Duration) map[string]any {
	result := map[string]any{"scanned": 0, "deleted": 0, "skipped_recent": 0, "failed": 0}
	root, err := filepath.Abs(a.cfg().UploadDir)
	if err != nil {
		result["failed"] = 1
		result["error"] = err.Error()
		return result
	}
	referenced := map[string]bool{}
	for _, u := range a.store().ListUsers() {
		addUploadReference(referenced, u.Avatar)
		addUploadReference(referenced, u.Background)
	}
	for _, kind := range []string{"avatar", "background", "avatars", "backgrounds"} {
		dir := filepath.Join(root, kind)
		absDir, err := filepath.Abs(dir)
		if err != nil || !isSubpath(root, absDir) {
			result["failed"] = int(numeric(result["failed"])) + 1
			continue
		}
		entries, err := os.ReadDir(absDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			result["failed"] = int(numeric(result["failed"])) + 1
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			result["scanned"] = int(numeric(result["scanned"])) + 1
			filename := entry.Name()
			if referenced[uploadRefKey(kind, filename)] {
				continue
			}
			path := filepath.Join(absDir, filename)
			info, err := entry.Info()
			if err != nil {
				result["failed"] = int(numeric(result["failed"])) + 1
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if time.Since(info.ModTime()) < maxAge {
				result["skipped_recent"] = int(numeric(result["skipped_recent"])) + 1
				continue
			}
			if err := os.Remove(path); err != nil {
				result["failed"] = int(numeric(result["failed"])) + 1
				continue
			}
			result["deleted"] = int(numeric(result["deleted"])) + 1
		}
	}
	return result
}

func addUploadReference(refs map[string]bool, raw string) {
	// 如果 Background 是 JSON 对象（用户背景配置），提取所有 url(...) 值
	if trimmed := strings.TrimSpace(raw); strings.HasPrefix(trimmed, "{") {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(trimmed), &cfg); err == nil {
			for _, value := range cfg {
				if str, ok := value.(string); ok {
					addUploadReference(refs, str)
				}
			}
		}
		return
	}
	kind, filename, ok := extractUploadReference(raw)
	if !ok {
		return
	}
	for _, alias := range uploadKindAliases(kind) {
		refs[uploadRefKey(alias, filename)] = true
	}
}

func extractUploadReference(raw string) (string, string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", "", false
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "url(") && strings.HasSuffix(value, ")") {
		value = strings.TrimSpace(value[4 : len(value)-1])
		value = strings.Trim(value, `"'`)
	}
	for _, prefix := range []string{"/api/v1/users/assets/", "/uploads/"} {
		if !strings.HasPrefix(value, prefix) {
			continue
		}
		rel := strings.TrimPrefix(value, prefix)
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) != 2 {
			return "", "", false
		}
		kind := strings.TrimSpace(parts[0])
		filename := filepath.Base(parts[1])
		if kind == "" || filename == "." || filename == string(filepath.Separator) || strings.Contains(parts[1], "..") {
			return "", "", false
		}
		return kind, filename, true
	}
	return "", "", false
}

func uploadKindAliases(kind string) []string {
	switch kind {
	case "avatar", "avatars":
		return []string{"avatar", "avatars"}
	case "background", "backgrounds":
		return []string{"background", "backgrounds"}
	default:
		return []string{kind}
	}
}

func uploadRefKey(kind, filename string) string {
	return kind + "/" + filename
}

func isSubpath(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}
