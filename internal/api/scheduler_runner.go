package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
	"go.uber.org/zap"
)

func (a *App) handleSendReminders(w http.ResponseWriter, r *http.Request, _ Params) {
	payload := decodeMap(r)
	defaultDays := a.cfg().NotificationExpiryRemindDays
	if defaultDays <= 0 {
		defaultDays = 3
	}
	days := clamp(intValue(payload, "days", defaultDays), 1, 365)
	result := a.sendExpiryReminders(r.Context(), days)
	ok(w, "reminders sent", result)
}

func (a *App) handleSchedulerRunV2(w http.ResponseWriter, r *http.Request, params Params) {
	jobID := params["job_id"]
	if !schedulerJobExists(jobID) {
		failWithCode(w, http.StatusNotFound, ErrSchedulerJobNotFound, "调度任务不存在")
		return
	}
	run, okRun, err := a.enqueueSchedulerJob(r.Context(), jobID, schedulerRequestParams(r), "manual")
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "任务入队失败，请稍后重试")
		return
	}
	if !okRun {
		failWithCode(w, http.StatusConflict, ErrSchedulerJobRunning, "调度任务正在运行中")
		return
	}
	a.audit(r, "scheduler_run", "admin", 0, map[string]any{"job_id": jobID, "run_id": run.ID, "type": "manual"})
	ok(w, "job queued", map[string]any{"job_id": run.JobID, "last_run": run})
}

func (a *App) runSchedulerJob(r *http.Request, jobID string) (map[string]any, []string, error) {
	if !schedulerJobExists(jobID) {
		return map[string]any{"success": false}, nil, fmt.Errorf("job not found")
	}
	// Scheduler may run in a dedicated process. Refresh once before every job so
	// all job reads share the same persisted view as HTTP and Telegram flows.
	if err := a.store().Refresh(); err != nil {
		return map[string]any{"success": false}, []string{"failed to refresh persisted state"}, err
	}
	if r != nil {
		r = withRequestStoreRefreshState(r)
		if state, _ := r.Context().Value(requestStoreRefreshKey{}).(*requestStoreRefreshState); state != nil {
			state.completed = true
		}
	}
	params := a.schedulerEffectiveParams(r, jobID)
	if err := r.Context().Err(); err != nil {
		return map[string]any{"success": false, "terminated": true}, []string{"job terminated before execution"}, err
	}
	now := time.Now().Unix()
	switch jobID {
	case "check_expired":
		disabled := 0
		embyDisabled := 0
		skippedProtected := 0
		autoRenewed := 0
		autoRenewalInsufficient := 0
		autoRenewalIneligible := 0
		autoRenewalFailed := 0
		autoRenewalPointsSpent := 0
		autoRenewalEmbyEnabled := 0
		autoRenewalEmbyEnableFailed := 0
		// Emby 停用失败必须计数并记录：本地已停用，下一轮 check_expired 会跳过这个人，
		// 只能靠 emby_state_reconcile 收敛。uid 清单写进稽核与摘要，方便管理员追查。
		embyDisableFailed := 0
		disabledUIDs := []int64{}
		embyDisableFailedUIDs := []int64{}
		renewedUIDs := []int64{}
		expiredLogs := []string{}
		disableEmbyWithRetry := func(u store.User) {
			sideCtx, sideCancel := schedulerSideEffectContext(r.Context())
			defer sideCancel()
			disabledRemote := false
			err := embyRetryOn5xx(sideCtx, func(ctx context.Context) error {
				var err error
				disabledRemote, err = a.disableRemoteEmbyForWebState(ctx, u)
				return err
			})
			if err != nil {
				embyDisableFailed++
				embyDisableFailedUIDs = appendLimitedUID(embyDisableFailedUIDs, u.UID)
				zap.L().Warn("failed to disable Emby for expired user", zap.Int64("uid", u.UID), zap.Error(err))
				if len(expiredLogs) < 50 {
					expiredLogs = append(expiredLogs, fmt.Sprintf("failed to disable Emby uid=%d: %s", u.UID, redactSensitiveText(err.Error())))
				}
				return
			}
			if disabledRemote {
				embyDisabled++
			}
		}
		cfg := *a.cfg()
		autoRenewalActive := signinAutoRenewalEnabled(cfg)
		users := a.store().ListUsers()
		invitedUIDs := map[int64]bool{}
		for _, rel := range a.store().InviteRelations() {
			invitedUIDs[rel.ChildUID] = true
		}
		for _, u := range users {
			if r.Context().Err() != nil {
				break // Keep the audit entries for users already processed.
			}
			// 守护管理员 / 白名单不被自动禁用：运维约定"绝不会给 admin 设
			// finite ExpiredAt"，但 demote-then-repromote 路径 / 手动 SQL /
			// 旧迁移可能在 admin 上留下 ExpiredAt > 0；一旦 check_expired 命中
			// 就会把 admin Active=false 并 DeleteUser session——管理员从此登
			// 不上自己的 panel，且只有数据库直改才能解锁。这里统一走保护用
			// 户口径（角色 + 配置管理员），并 emit `skipped_protected` 计数让
			// admin 在调度报告里看到守护命中（区别于"无人需要禁用"的 0 值）。
			if a.userIsProtected(u) {
				if u.Active && u.ExpiredAt > 0 && u.ExpiredAt < now {
					skippedProtected++
				}
				continue
			}
			if u.Active && u.ExpiredAt > 0 && u.ExpiredAt < now {
				if autoRenewalActive && u.SigninAutoRenewal {
					renewed, _, renewErr := a.spendSigninRenewal(u.UID, cfg.SigninRenewalCost, cfg.SigninRenewalDays, time.Unix(now, 0), true)
					if renewErr == nil {
						autoRenewed++
						renewedUIDs = appendLimitedUID(renewedUIDs, renewed.UID)
						autoRenewalPointsSpent += cfg.SigninRenewalCost
						if renewed.EmbyDisabled && a.embyConfigured() {
							sideCtx, sideCancel := schedulerSideEffectContext(r.Context())
							if err := embyRetryOn5xx(sideCtx, func(ctx context.Context) error {
								return a.embyApplyEnabledState(ctx, renewed.UID, renewed.EmbyID, true)
							}); err != nil {
								autoRenewalEmbyEnableFailed++
								zap.L().Warn("failed to re-enable Emby after automatic sign-in renewal", zap.Int64("uid", renewed.UID), zap.Error(err))
							} else {
								autoRenewalEmbyEnabled++
							}
							sideCancel()
						}
						continue
					}
					switch {
					case errors.Is(renewErr, store.ErrInsufficientPoints):
						autoRenewalInsufficient++
					case errors.Is(renewErr, store.ErrEmbyRequired), errors.Is(renewErr, store.ErrConflict):
						autoRenewalIneligible++
					default:
						autoRenewalFailed++
						zap.L().Warn("automatic sign-in renewal failed", zap.Int64("uid", u.UID), zap.Error(renewErr))
					}
					// SpendSigninPointsAndUpdateUser refreshes under the store lock. Re-read
					// before expiry handling so a concurrent renewal is never disabled from
					// the stale ListUsers snapshot.
					latest, ok := a.store().User(u.UID)
					if !ok || !latest.Active || latest.ExpiredAt <= 0 || latest.ExpiredAt >= now {
						continue
					}
					u = latest
				}
				// For invited users (have invite relation), only disable Emby access
				// but keep the account active so they can still log in and renew
				isInvited := invitedUIDs[u.UID]
				if isInvited {
					// Only disable Emby, keep account active so the user
					// can re-login (or the inviter can renew on their behalf)
					disableEmbyWithRetry(u)
					sideCtx, sideCancel := schedulerSideEffectContext(r.Context())
					// 即便保留 Active=true 让用户能重新登录续期，已经过期的
					// 时刻必须立刻让现有会话失效——否则 stale cookie 在
					// SessionTTL 内仍能访问受保护接口（包括非续期接口），
					// 与 authenticateAPIKey 的 `!u.Active` / 过期兜底语义
					// 不一致。续期成功后用户重新登录即可拿新 session。
					a.sessions().DeleteUser(sideCtx, u.UID)
					sideCancel()
					disabled++
					disabledUIDs = appendLimitedUID(disabledUIDs, u.UID)
				} else {
					// Non-invited users: disable the whole account
					updated, err := a.store().SetUserActiveAtomic(u.UID, false)
					if err == nil {
						disableEmbyWithRetry(updated)
						sideCtx, sideCancel := schedulerSideEffectContext(r.Context())
						// 立即清除该用户的所有会话。否则 stale
						// token 仍可访问受保护接口直到 SessionTTL 自然到期。
						a.sessions().DeleteUser(sideCtx, updated.UID)
						disabled++
						disabledUIDs = appendLimitedUID(disabledUIDs, updated.UID)
						sideCancel()
					}
				}
			}
		}
		if autoRenewed > 0 {
			a.auditSystem("scheduler", "auto_renew_expired_users", 0, map[string]any{
				"auto_renewed":                    autoRenewed,
				"auto_renewal_points_spent":       autoRenewalPointsSpent,
				"auto_renewal_emby_enabled":       autoRenewalEmbyEnabled,
				"auto_renewal_emby_enable_failed": autoRenewalEmbyEnableFailed,
				"uids":                            renewedUIDs,
			})
		}
		if disabled > 0 || embyDisabled > 0 || embyDisableFailed > 0 {
			a.auditSystem("scheduler", "disable_expired_users", 0, map[string]any{
				"disabled":                 disabled,
				"emby_disabled":            embyDisabled,
				"emby_disable_failed":      embyDisableFailed,
				"skipped_protected":        skippedProtected,
				"uids":                     disabledUIDs,
				"emby_disable_failed_uids": embyDisableFailedUIDs,
			})
		}
		expiredLogs = append(expiredLogs, fmt.Sprintf("auto-renewed %d and disabled %d expired users", autoRenewed, disabled))
		if embyDisableFailed > 0 {
			// Emby 停用失败要让本轮显示为失败（并触发失败通知），漏掉的由 emby_state_reconcile 收敛。
			expiredLogs = append(expiredLogs, fmt.Sprintf("%d Emby accounts could not be disabled; emby_state_reconcile will retry", embyDisableFailed))
		}
		return map[string]any{
			"success":                         embyDisableFailed == 0 && autoRenewalFailed == 0 && autoRenewalEmbyEnableFailed == 0 && r.Context().Err() == nil,
			"emby_disable_failed":             embyDisableFailed,
			"emby_disable_failed_uids":        embyDisableFailedUIDs,
			"disabled_uids":                   disabledUIDs,
			"disabled":                        disabled,
			"emby_disabled":                   embyDisabled,
			"skipped_protected":               skippedProtected,
			"auto_renewed":                    autoRenewed,
			"auto_renewal_insufficient":       autoRenewalInsufficient,
			"auto_renewal_ineligible":         autoRenewalIneligible,
			"auto_renewal_failed":             autoRenewalFailed,
			"auto_renewal_points_spent":       autoRenewalPointsSpent,
			"auto_renewal_emby_enabled":       autoRenewalEmbyEnabled,
			"auto_renewal_emby_enable_failed": autoRenewalEmbyEnableFailed,
		}, expiredLogs, r.Context().Err()
	case "check_expiring", "expiry_reminders":
		defaultDays := a.cfg().NotificationExpiryRemindDays
		if defaultDays <= 0 {
			defaultDays = 3
		}
		days := clamp(jobParamInt(params, "days", queryInt(r, "days", defaultDays)), 1, 365)
		if jobID == "expiry_reminders" {
			result := a.sendExpiryReminders(r.Context(), days)
			return result, []string{fmt.Sprintf("sent %d reminders for %d expiring users; %d failed", int(numeric(result["sent"])), int(numeric(result["count"])), int(numeric(result["failed_count"])))}, r.Context().Err()
		}
		deadline := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
		count := 0
		for _, u := range a.store().ListUsers() {
			if err := r.Context().Err(); err != nil {
				return map[string]any{"success": false, "terminated": true, "expiring": count, "days": days}, []string{"job terminated"}, err
			}
			if u.Active && u.ExpiredAt > now && u.ExpiredAt <= deadline {
				count++
			}
		}
		return map[string]any{"success": true, "expiring": count, "days": days}, []string{fmt.Sprintf("found %d expiring users", count)}, nil
	case "daily_stats":
		totalUsers, activeUsers := a.store().UserCounts()
		return map[string]any{"success": true, "users": totalUsers, "active": activeUsers}, []string{"daily stats generated"}, nil
	case "sync_emby_activity_logs":
		if !a.embyConfigured() {
			return map[string]any{"success": true, "configured": false, "new_entries": 0}, []string{"Emby not configured"}, nil
		}
		sinceHours := clamp(jobParamInt(params, "since_hours", 24), 1, 720)
		count, err := a.fetchAndStoreEmbyActivityLogsSince(r.Context(), time.Now().Add(-time.Duration(sinceHours)*time.Hour))
		if err != nil {
			return map[string]any{"success": false, "since_hours": sinceHours}, nil, err
		}
		return map[string]any{"success": true, "configured": true, "new_entries": count, "since_hours": sinceHours}, []string{fmt.Sprintf("synced %d Emby activity log entries from the last %d hours", count, sinceHours)}, nil
	case "cleanup_sessions":
		if err := a.store().CleanupTwoFactorRequests(r.Context()); err != nil {
			return nil, nil, err
		}
		expiredSessions := a.sessions().CleanupExpired(r.Context())
		cfg := a.cfg()
		expiredEmailCodes := 0
		if cfg.EmailAutoCleanupExpiredVerifications {
			// 顺带回收过期邮箱验证码：短 TTL，借会话清理槽周期回收，避免 state 里
			// 堆积已失效记录。失败不阻断主流程（下一轮再清）。
			expiredEmailCodes, _ = a.store().CleanupExpiredEmailVerifications(time.Now().Unix())
		}
		staleCleared := 0
		staleClearedUIDs := []int64{}
		if cfg.EmailAutoCleanupUnverified {
			// 定期清理已绑定但长期未验证的邮箱，释放邮箱地址供其他用户使用。
			// 使用基于 CreatedAt 的年龄门限而非 ClearUnverifiedEmails 的全量清理，
			// 避免刚注册几分钟的用户还没查收验证码就被清掉邮箱。
			hours := cfg.EmailAutoCleanupUnverifiedHours
			if hours <= 0 {
				hours = 24
			}
			staleClearedUIDs = a.unverifiedEmailUIDsBefore(time.Now().Add(-time.Duration(hours) * time.Hour).Unix())
			_, staleCleared, _ = a.store().CleanupUnverifiedEmailsByAge(time.Now().Add(-time.Duration(hours) * time.Hour).Unix())
		}
		// Telegram 绑定链接旧实现只在启动 / 配置重载时清理，这里并入例行清理。
		now := time.Now().Unix()
		expiredTelegramLinks := a.cleanupExpiredTelegramLinks(now)
		orphanedTelegramLinks := a.cleanupOrphanedTelegramLinks()
		summary := map[string]any{"success": true, "configured": a.embyConfigured(), "active": 0, "total": 0,
			"expired_sessions": expiredSessions, "expired_email_codes": expiredEmailCodes, "cleared_unverified_emails": staleCleared,
			"expired_telegram_links": expiredTelegramLinks, "orphaned_telegram_links": orphanedTelegramLinks}
		logs := []string{fmt.Sprintf("cleaned up %d expired sessions", expiredSessions), fmt.Sprintf("cleaned up %d expired email codes", expiredEmailCodes), fmt.Sprintf("cleared %d stale unverified emails", staleCleared), fmt.Sprintf("cleaned up %d expired and %d orphaned Telegram bind links", expiredTelegramLinks, orphanedTelegramLinks)}
		if len(staleClearedUIDs) > 0 {
			// 清掉用户邮箱属于改用户资料，写系统稽核并附 uid 清单。
			a.auditSystem("scheduler", "clear_stale_unverified_emails", 0, map[string]any{"cleared": staleCleared, "uids": staleClearedUIDs})
		}
		if !a.embyConfigured() {
			return summary, append([]string{"Emby not configured"}, logs...), nil
		}
		// 读 Emby 会话数只是巡检附带的观测项。前面的清理已经完成，读不到 Emby 时不能
		// 把整轮标成失败——记为部分完成并附上原因。
		sessions, err := a.embySessionsSnapshot(r.Context(), false)
		if err != nil {
			summary["partial"] = true
			summary["emby_error"] = redactSensitiveText(err.Error())
			return summary, append(logs, "failed to read Emby sessions: "+redactSensitiveText(err.Error())), nil
		}
		summary["active"] = countEmbyPlayingSessions(sessions)
		summary["total"] = len(sessions)
		return summary, append([]string{fmt.Sprintf("read %d Emby sessions", len(sessions))}, logs...), nil
	case "emby_sync":
		if !a.embyConfigured() {
			return map[string]any{"success": true, "configured": false}, []string{"Emby not configured"}, nil
		}
		// 给 emby_sync 加 30 分钟硬上限，避免 emby 反代僵死时整个调度槽被占住——
		// 调度器是单 goroutine 串行的，emby_sync hang 住会让 check_expired /
		// expiry_reminders 这些下游任务推迟整轮。即便 500 个用户每人 5s，仍不
		// 到 45 分钟；超过 30min 几乎一定是 emby 不健康，让本轮 fail-fast 优于
		// 拖到管理员手动 cancel。
		syncCtx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		logs := []string{}
		var remote []map[string]any
		// /Users 列表是幂等 GET，遇 5xx / 连接抖动重试 2 次更划算（详见
		// embyRetryOn5xx 的注释）。一开局拉用户列表如果直接挂掉，整轮 sync 全
		// 部 user 都被记成 missing，下一轮还要再炸一遍。
		if err := embyRetryOn5xx(syncCtx, func(ctx context.Context) error {
			return a.embyGet(ctx, "/Users", &remote)
		}); err != nil {
			return map[string]any{"success": false}, nil, err
		}
		remoteByID := map[string]map[string]any{}
		remoteByName := map[string]map[string]any{}
		duplicateRemoteNames := map[string]bool{}
		for _, user := range remote {
			if id := embyRemoteID(user); id != "" {
				remoteByID[id] = user
			}
			if name := normalizeEmbyName(embyRemoteName(user)); name != "" {
				if _, exists := remoteByName[name]; exists {
					duplicateRemoteNames[name] = true
				}
				remoteByName[name] = user
			}
		}
		for name := range duplicateRemoteNames {
			delete(remoteByName, name)
		}
		allUsers := a.store().ListUsers()
		maxUsers := clamp(jobParamInt(params, "max_users", 1000), 1, 50000)
		// 占用关系必须看全部用户，不能只看本批，否则批外用户已占用的远端 ID 会被误判为空闲。
		claimedRemoteIDs := map[string]int64{}
		for _, u := range allUsers {
			if u.EmbyID != "" && !isSyntheticEmbyID(u.EmbyID, u.UID) {
				claimedRemoteIDs[u.EmbyID] = u.UID
			}
		}
		// max_users 截断改成游标分批：用户按 UID 升序，本批从 after_uid 之后开始；
		// 未显式指定时接着上一轮留下的 next_after_uid 继续，跑完一圈后回到开头。
		afterUID := int64(jobParamInt(params, "after_uid", -1))
		if afterUID < 0 {
			afterUID = a.embySyncLastCursor()
		}
		users := make([]store.User, 0, min(len(allUsers), maxUsers))
		truncated := false
		for _, u := range allUsers {
			if u.UID <= afterUID {
				continue
			}
			if len(users) >= maxUsers {
				truncated = true
				break
			}
			users = append(users, u)
		}
		nextAfterUID := int64(0)
		if truncated && len(users) > 0 {
			nextAfterUID = users[len(users)-1].UID
		}
		logs = append(logs, fmt.Sprintf("batch: after_uid=%d, %d users, next_after_uid=%d", afterUID, len(users), nextAfterUID))
		updatedNames, syncedState, stateUnchanged, missing, filledIDs, repairedPlaceholders, conflicts := 0, 0, 0, 0, 0, 0, 0
		filledUIDs, embyDisabledUIDs := []int64{}, []int64{}
		nameCandidates := 0
		for _, u := range users {
			if err := syncCtx.Err(); err != nil {
				return map[string]any{"success": false, "terminated": true, "updated_names": updatedNames, "synced_state": syncedState, "state_unchanged": stateUnchanged, "missing": missing, "filled_emby_ids": filledIDs, "repaired_placeholders": repairedPlaceholders, "conflicts": conflicts, "name_candidates": nameCandidates}, []string{"job terminated"}, err
			}
			placeholder := isSyntheticEmbyID(u.EmbyID, u.UID)
			remoteUser, okRemote := remoteByID[u.EmbyID]
			// 按名称认领远端账号只用于修复“面板自己开通、但 EmbyID 还是占位值”的账号。
			// 没有占位 ID 的账号绝不按名称认领：注册码注册时 EmbyUsername 就是用户自己
			// 填的 Web 用户名，注册一个与他人 Emby 同名的账号，等管理员跑一次同步就能
			// 接管对方的 Emby（随后改密码、解绑、删号）。远端管理员账号同样不自动认领。
			// 这些同名情况只记为候选，交给管理员手动绑定（手动绑定需要 Emby 密码）。
			if !okRemote {
				for _, name := range []string{u.EmbyUsername, u.Username} {
					candidate, okByName := remoteByName[normalizeEmbyName(name)]
					if strings.TrimSpace(name) == "" || !okByName {
						continue
					}
					if placeholder && !embyRemoteIsAdministrator(candidate) {
						remoteUser = candidate
						okRemote = true
					} else {
						nameCandidates++
						if len(logs) < 200 {
							logs = append(logs, fmt.Sprintf("user #%d (%s): same-name Emby account found, not auto-linked (needs manual bind)", u.UID, u.Username))
						}
					}
					break
				}
			}
			if !okRemote {
				if u.EmbyID != "" {
					missing++
				}
				continue
			}
			remoteID := embyRemoteID(remoteUser)
			if remoteID == "" {
				missing++
				continue
			}
			if ownerUID, claimed := claimedRemoteIDs[remoteID]; claimed && ownerUID != u.UID {
				conflicts++
				continue
			}
			name := embyRemoteName(remoteUser)
			updatedUser := u
			changes := []string{}
			if remoteID != u.EmbyID {
				changes = append(changes, "emby_id: "+u.EmbyID+"→"+remoteID)
			}
			if name != "" && name != u.EmbyUsername {
				changes = append(changes, "username: "+u.EmbyUsername+"→"+name)
			}
			if u.PendingEmby {
				changes = append(changes, "pending_emby: cleared")
			}
			if remoteID != u.EmbyID || (name != "" && name != u.EmbyUsername) || u.PendingEmby {
				var err error
				updatedUser, err = a.store().UpdateUser(u.UID, func(u *store.User) error {
					if remoteID != u.EmbyID {
						u.EmbyID = remoteID
					}
					if name != "" {
						u.EmbyUsername = name
					}
					u.PendingEmby = false
					u.PendingEmbyDays = nil
					return nil
				})
				if err == nil {
					if remoteID != u.EmbyID {
						filledIDs++
						filledUIDs = appendLimitedUID(filledUIDs, u.UID)
						if placeholder {
							repairedPlaceholders++
						}
						logs = append(logs, "user #"+fmt.Sprintf("%d", u.UID)+" ("+u.Username+"): "+strings.Join(changes, ", "))
					}
					updatedNames++
					claimedRemoteIDs[remoteID] = u.UID
				} else {
					conflicts++
					logs = append(logs, "user #"+fmt.Sprintf("%d", u.UID)+" ("+u.Username+"): sync conflict - "+truncateString(err.Error(), 120))
					continue
				}
			}
			shouldDisable := embyShouldDisableForWebState(updatedUser)
			if !shouldDisable {
				syncedState++
				stateUnchanged++
				continue
			}
			if remoteDisabled, ok := embyRemoteDisabled(remoteUser); ok && remoteDisabled {
				syncedState++
				stateUnchanged++
				continue
			}
			if embyRetryOn5xx(syncCtx, func(ctx context.Context) error {
				_, err := a.disableRemoteEmbyForWebState(ctx, updatedUser)
				return err
			}) == nil {
				syncedState++
				embyDisabledUIDs = appendLimitedUID(embyDisabledUIDs, updatedUser.UID)
				logs = append(logs, "user #"+fmt.Sprintf("%d", updatedUser.UID)+" ("+updatedUser.Username+"): emby disabled by policy")
			}
		}
		logs = append(logs, fmt.Sprintf("read %d Emby users, %d synced, %d unchanged, %d missing, %d conflicts", len(remote), syncedState, stateUnchanged, missing, conflicts))
		if syncedState > 0 || filledIDs > 0 {
			a.auditSystem("scheduler", "emby_sync", 0, map[string]any{
				"remote_users":          len(remote),
				"updated_names":         updatedNames,
				"synced_state":          syncedState,
				"state_unchanged":       stateUnchanged,
				"filled_emby_ids":       filledIDs,
				"repaired_placeholders": repairedPlaceholders,
				"missing":               missing,
				"conflicts":             conflicts,
				"filled_uids":           filledUIDs,
				"emby_disabled_uids":    embyDisabledUIDs,
			})
		}
		return map[string]any{"success": true, "after_uid": afterUID, "next_after_uid": nextAfterUID, "truncated": truncated, "batch_users": len(users), "remote_users": len(remote), "updated_names": updatedNames, "synced_state": syncedState, "state_unchanged": stateUnchanged, "missing": missing, "filled_emby_ids": filledIDs, "repaired_placeholders": repairedPlaceholders, "conflicts": conflicts, "name_candidates": nameCandidates}, logs, nil
	case "auto_backup_database":
		// 未开启时自动排程空转；管理员手动「立即执行」总是会备份。
		if !jobParamBool(params, "enabled", a.cfg().SchedulerAutoBackupEnabled) && !schedulerManualRun(r) {
			return map[string]any{"success": true, "skipped": true, "enabled": false}, []string{"auto backup disabled"}, nil
		}
		return a.runAutoBackupDatabase(jobParamInt(params, "keep", autoBackupKeep(a.cfg().SchedulerAutoBackupKeep)))
	case "emby_state_reconcile":
		return a.runEmbyStateReconcile(r.Context(), jobParamBool(params, "dry_run", false), max(jobParamInt(params, "max_changes", embyReconcileDefaultMaxChanges), 0))
	case "cleanup_no_emby":
		ignoreEnabled := jobParamBool(params, "ignore_enabled_flag", false)
		enabled := jobParamBool(params, "enabled", jobParamBool(params, "auto_enabled", a.cfg().AutoCleanupNoEmby))
		if !enabled && !ignoreEnabled {
			return map[string]any{"success": true, "enabled": false, "deleted": 0}, []string{"auto cleanup no-Emby disabled"}, nil
		}
		days := jobParamInt(params, "days", queryInt(r, "days", a.cfg().AutoCleanupNoEmbyDays))
		if days <= 0 {
			days = 7
		}
		threshold := int64(0)
		if days > 0 {
			threshold = time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
		}
		preserveTG := jobParamBool(params, "preserve_tg_bound", a.cfg().EmbyDirectRegisterEnabled)
		dryRun := jobParamBool(params, "dry_run", false)
		candidates := 0
		deleted := 0
		failed := 0
		skippedPending := 0
		deletedUsers := []map[string]any{}
		for _, u := range a.store().ListUsers() {
			if r.Context().Err() != nil {
				break // Audit completed deletions before returning cancellation.
			}
			if a.userIsProtected(u) || u.EmbyID != "" {
				continue
			}
			if u.PendingEmby {
				skippedPending++
				continue
			}
			if preserveTG && u.TelegramID != 0 {
				continue
			}
			registered := u.RegisterTime
			if registered == 0 {
				registered = u.CreatedAt
			}
			// 「多久没有 Emby」要从最近一次解绑算起，不能只看注册时间：注册三年、昨天
			// 刚解绑 Emby 的老用户不应该被当成「注册后长期未开通」直接删掉。
			if u.EmbyUnboundAt > registered {
				registered = u.EmbyUnboundAt
			}
			if threshold > 0 && registered > threshold {
				continue
			}
			candidates++
			if dryRun {
				continue
			}
			sideCtx, cancel := schedulerSideEffectContext(r.Context())
			if err := a.deleteLocalUser(sideCtx, u); err != nil {
				cancel()
				failed++
			} else {
				cancel()
				deleted++
				if len(deletedUsers) < auditUIDListLimit {
					deletedUsers = append(deletedUsers, map[string]any{"uid": u.UID, "username": u.Username})
				}
			}
		}
		if deleted > 0 {
			a.auditSystem("scheduler", "delete_no_emby_users", 0, map[string]any{
				"deleted":    deleted,
				"candidates": candidates,
				"failed":     failed,
				"days":       days,
				"users":      deletedUsers,
			})
		}
		return map[string]any{"success": failed == 0 && r.Context().Err() == nil, "enabled": true, "candidates": candidates, "deleted": deleted, "failed": failed, "dry_run": dryRun, "days": days, "days_threshold": days, "preserve_tg_bound": preserveTG, "skipped_pending_emby": skippedPending}, []string{fmt.Sprintf("processed %d no-Emby web users; %d deleted, %d failed", candidates, deleted, failed)}, r.Context().Err()
	case "cleanup_pending_emby_entitlements":
		ignoreEnabled := jobParamBool(params, "ignore_enabled_flag", false)
		enabled := jobParamBool(params, "enabled", jobParamBool(params, "auto_enabled", a.cfg().AutoCleanupPendingEmby))
		if !enabled && !ignoreEnabled {
			return map[string]any{"success": true, "enabled": false, "cleared": 0}, []string{"auto cleanup pending-Emby entitlement disabled"}, nil
		}
		dryRun := jobParamBool(params, "dry_run", false)
		// 旧实现没有年龄门槛，scope=all 会把刚发放的资格也一次收回。现在只收回发放
		// 超过 days 天仍未开通的资格（SAR.auto_cleanup_pending_emby_days，默认 7）。
		days := jobParamInt(params, "days", a.cfg().AutoCleanupPendingEmbyDays)
		if days <= 0 {
			days = 7
		}
		threshold := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
		candidates := 0
		cleared := 0
		failed := 0
		skippedRecent := 0
		clearedUIDs := []int64{}
		for _, u := range a.store().ListUsers() {
			if err := r.Context().Err(); err != nil {
				return map[string]any{"success": false, "terminated": true, "candidates": candidates, "cleared": cleared, "failed": failed, "dry_run": dryRun}, []string{"job terminated"}, err
			}
			if a.userIsProtected(u) || u.EmbyID != "" || !u.PendingEmby {
				continue
			}
			grantedAt := u.PendingEmbyGrantedAt
			if grantedAt == 0 {
				grantedAt = u.RegisterTime
				if u.CreatedAt > grantedAt {
					grantedAt = u.CreatedAt
				}
			}
			if grantedAt > threshold {
				skippedRecent++
				continue
			}
			candidates++
			if dryRun {
				continue
			}
			if _, err := a.store().UpdateUser(u.UID, func(u *store.User) error {
				u.PendingEmby = false
				u.PendingEmbyDays = nil
				return nil
			}); err != nil {
				failed++
			} else {
				cleared++
				clearedUIDs = appendLimitedUID(clearedUIDs, u.UID)
			}
		}
		if cleared > 0 {
			a.auditSystem("scheduler", "clear_pending_emby_entitlements", 0, map[string]any{"cleared": cleared, "failed": failed, "days": days, "uids": clearedUIDs})
		}
		return map[string]any{"success": failed == 0, "enabled": true, "candidates": candidates, "cleared": cleared, "failed": failed, "dry_run": dryRun, "days": days, "skipped_recent": skippedRecent, "cleared_uids": clearedUIDs}, []string{fmt.Sprintf("cleared %d pending Emby entitlements older than %d days", cleared, days)}, nil
	case "enforce_group_membership":
		// dry_run 只列出会停用 / 会启用的名单；breaker_* 允许管理员临时调整熔断阈值。
		result, logs, err := a.enforceTelegramMembershipWithOptions(r.Context(), telegramMembershipOptions{
			AutoEnableRejoined: jobParamBool(params, "auto_enable_rejoined", a.cfg().TelegramAutoEnableRejoined),
			DryRun:             jobParamBool(params, "dry_run", false),
			BreakerPercent:     clamp(jobParamInt(params, "breaker_percent", a.cfg().TelegramMembershipBreakerPercent), 0, 100),
			BreakerMax:         max(jobParamInt(params, "breaker_max", a.cfg().TelegramMembershipBreakerMax), 0),
		})
		result["success"] = err == nil
		return result, logs, err
	case "check_telegram_bindings":
		seen := map[int64]int64{}
		duplicates := 0
		for _, u := range a.store().ListUsers() {
			if err := r.Context().Err(); err != nil {
				return map[string]any{"success": false, "terminated": true, "duplicates": duplicates, "bound": len(seen)}, []string{"job terminated"}, err
			}
			if u.TelegramID == 0 {
				continue
			}
			if seen[u.TelegramID] != 0 {
				duplicates++
			}
			seen[u.TelegramID] = u.UID
		}
		return map[string]any{"success": true, "duplicates": duplicates, "bound": len(seen)}, []string{fmt.Sprintf("found %d duplicate telegram bindings", duplicates)}, nil
	case "kick_unknown_group_members":
		dryRun := jobParamBool(params, "dry_run", true)
		maxPerRun := clamp(jobParamInt(params, "max_per_run", 200), 1, 500)
		chats := telegramChatIDs(a.cfg().TelegramGroupIDs)
		if len(chats) == 0 {
			return map[string]any{"success": true, "enabled": false, "targets": 0, "dry_run": dryRun, "max_per_run": maxPerRun}, []string{"Telegram group not configured"}, nil
		}
		plan, err := a.telegramKickPlan(chats[0])
		if err != nil {
			return map[string]any{"success": false, "enabled": true}, []string{"读取 Telegram 花名册失败"}, err
		}
		targets := plan.Targets
		skippedByType := plan.Skipped
		preservedBound := plan.PreservedBound
		reasonCounts := map[string]int{"no_account": 0, "no_emby": 0, "disabled": 0}
		for _, target := range targets {
			reasonCounts[target.Reason]++
		}
		summary := map[string]any{
			"success":           true,
			"enabled":           true,
			"known_only":        plan.KnownOnly,
			"chat_id":           chats[0],
			"roster_size":       plan.RosterSize,
			"bots_in_roster":    plan.Bots,
			"preserved_bound":   preservedBound,
			"admins_excluded":   skippedByType["admin"],
			"excluded_total":    skippedByType["admin"] + skippedByType["whitelist"] + skippedByType["bound"],
			"targets":           len(targets),
			"reason_no_account": reasonCounts["no_account"],
			"reason_no_emby":    reasonCounts["no_emby"],
			"reason_disabled":   reasonCounts["disabled"],
			"dry_run":           dryRun,
			"max_per_run":       maxPerRun,
			"kicked":            0,
			"skipped":           0,
			"failed":            0,
			"not_in_group":      0,
			"scanned":           0,
			"skipped_no_tg":     skippedByType["no_telegram"],
			"skipped_whitelist": skippedByType["whitelist"],
			"skipped_bound":     skippedByType["bound"],
		}
		if dryRun || len(targets) == 0 {
			return summary, []string{fmt.Sprintf("found %d known Telegram kick candidates", len(targets))}, nil
		}
		if !a.telegramAvailable() {
			summary["success"] = false
			return summary, nil, fmt.Errorf("Telegram not configured")
		}
		adminSet := a.telegramAdminSet(r.Context(), chats[0])
		kicked, skipped, failedCount, notInGroup, scanned := 0, 0, 0, 0, 0
		kickedTargets := []map[string]any{}
		logs := []string{}
		for _, target := range targets {
			if err := r.Context().Err(); err != nil {
				summary["success"] = false
				summary["terminated"] = true
				return summary, append(logs, "job terminated"), err
			}
			if scanned >= maxPerRun {
				break
			}
			scanned++
			if adminSet[target.TelegramID] {
				skipped++
				continue
			}
			member, err := a.telegramGetChatMember(r.Context(), chats[0], target.TelegramID)
			if err != nil {
				msg := strings.ToLower(err.Error())
				if strings.Contains(msg, "not found") || strings.Contains(msg, "participant") {
					notInGroup++
					continue
				}
				failedCount++
				if len(logs) < 20 {
					// err 来自 telegram bot API，body 偶尔会带 bot token 反弹（API
					// 4xx 时 telegram 偶尔在 description 里回显请求 URL）。logs 落
					// 到 SchedulerRun.Logs 后被持久化到 PG，admin 后台可见，必须
					// 走 redactSensitiveText 脱敏。
					logs = append(logs, fmt.Sprintf("failed to inspect tg=%d uid=%d: %s", target.TelegramID, target.UID, redactSensitiveText(err.Error())))
				}
				telegramRateLimitPause(err)
				continue
			}
			if telegramMemberIsGone(member) {
				notInGroup++
				continue
			}
			if telegramMemberIsAdminOrBot(member) {
				skipped++
				continue
			}
			if err := a.telegramKickChatMember(r.Context(), chats[0], target.TelegramID); err != nil {
				failedCount++
				if len(logs) < 20 {
					// 同上：tg API 错误持久化前必须脱敏。
					logs = append(logs, fmt.Sprintf("failed to kick tg=%d uid=%d: %s", target.TelegramID, target.UID, redactSensitiveText(err.Error())))
				}
				telegramRateLimitPause(err)
				continue
			}
			kicked++
			if len(kickedTargets) < auditUIDListLimit {
				kickedTargets = append(kickedTargets, map[string]any{"uid": target.UID, "telegram_id": target.TelegramID, "reason": target.Reason})
			}
		}
		summary["kicked"] = kicked
		summary["skipped"] = skipped
		summary["failed"] = failedCount
		summary["not_in_group"] = notInGroup
		summary["scanned"] = scanned
		// 一键踢未绑（退群/无账号）同样属于封禁类副作用，计入系统日志供管理员复核。
		if kicked > 0 {
			a.auditSystem("scheduler", "kick_unbound_group_members", 0, map[string]any{
				"kicked":  kicked,
				"skipped": skipped,
				"failed":  failedCount,
				"chat_id": chats[0],
				"targets": kickedTargets,
			})
		}
		return summary, logs, nil
	case "cleanup_emby_devices":
		result, logs, err := a.cleanupEmbyDevices(r.Context(), embyDeviceCleanupOptions{
			DryRun:        jobParamBool(params, "dry_run", true),
			MaxWorkers:    jobParamInt(params, "max_workers", embyDeviceCleanupDefaultWorkers),
			SkipUsernames: embyDeviceCleanupSkipList(params["skip_usernames"]),
		})
		return result, logs, err
	case "cleanup_unused_uploads":
		result := a.cleanupUnusedUploadAssets(24 * time.Hour)
		result["success"] = true
		return result, []string{fmt.Sprintf("scanned %d upload files, deleted %d", int(numeric(result["scanned"])), int(numeric(result["deleted"])))}, nil
	case "system_auto_update":
		if !a.cfg().SystemUpdateEnabled && !schedulerManualRun(r) {
			return map[string]any{"success": true, "skipped": true, "enabled": false}, []string{"system auto update disabled"}, nil
		}
		result := applyGitUpdate(r.Context(), a.cfg().SystemUpdateRepoURL, a.cfg().SystemUpdateBranch, a.cfg().SystemUpdateRestartServices, false, false)
		a.auditSystem("scheduler", "system_update", 0, systemUpdateAuditDetail(result, a.cfg().SystemUpdateBranch))
		if !boolish(result["success"]) {
			return result, nil, fmt.Errorf("%s", asString(result["message"]))
		}
		return result, []string{asString(result["message"])}, nil
	case "cleanup_audit_logs":
		enabled := jobParamBool(params, "enabled", true)
		if !enabled {
			return map[string]any{"success": true, "skipped": true, "reason": "auto cleanup disabled"}, nil, nil
		}
		logs := []string{}
		preserveAdmin := jobParamBool(params, "preserve_admin", true)
		detail := map[string]any{"preserve_admin": preserveAdmin}
		// 按条数裁剪（保留最新 N 条）；preserve_admin 同样作用于条数裁剪，错误不再吞掉。
		if maxEntries := jobParamInt(params, "max_entries", 0); maxEntries > 0 {
			removed, err := a.store().PruneAuditLogs(maxEntries, preserveAdmin)
			if err != nil {
				return map[string]any{"success": false}, logs, fmt.Errorf("prune audit logs by count: %w", err)
			}
			detail["max_entries"] = maxEntries
			detail["removed_by_limit"] = removed
			logs = append(logs, fmt.Sprintf("enforced max %d entries, removed %d (preserve_admin=%v, current: %d)", maxEntries, removed, preserveAdmin, a.store().AuditLogCount()))
		}
		// 按天数裁剪
		if retentionDays := jobParamInt(params, "retention_days", 0); retentionDays > 0 {
			cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
			removed, err := a.store().PruneAuditLogsByAge(cutoff, preserveAdmin)
			if err != nil {
				return map[string]any{"success": false}, logs, fmt.Errorf("prune audit logs by age: %w", err)
			}
			detail["retention_days"] = retentionDays
			detail["removed_by_age"] = removed
			logs = append(logs, fmt.Sprintf("removed %d entries older than %d days (preserve_admin=%v)", removed, retentionDays, preserveAdmin))
		}
		// 排程裁剪同样写一条不可删除的自保记录。
		a.auditSystem("scheduler", "cleanup_audit_logs", 0, detail)
		return map[string]any{"success": true, "current": a.store().AuditLogCount()}, logs, nil
	case "cleanup_unlinked_emby":
		if !a.embyConfigured() {
			return map[string]any{"success": true, "configured": false}, []string{"Emby not configured"}, nil
		}
		logs := []string{}
		dryRun := jobParamBool(params, "dry_run", true)
		delete := jobParamBool(params, "delete", false)
		if dryRun && !delete {
			logs = append(logs, "dry-run mode: scanning only, no deletions")
		}
		var remote []map[string]any
		syncCtx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		if err := embyRetryOn5xx(syncCtx, func(ctx context.Context) error {
			return a.embyGet(ctx, "/Users", &remote)
		}); err != nil {
			return map[string]any{"success": false}, nil, err
		}
		localEmbyIDs := map[string]bool{}
		for _, u := range a.store().ListUsers() {
			if u.EmbyID != "" {
				localEmbyIDs[u.EmbyID] = true
			}
		}
		unlinked := []map[string]any{}
		for _, user := range remote {
			id := embyRemoteID(user)
			// Emby 服务器管理员（通常是站长自己的账号）从来不归面板管理，不能当孤儿删掉。
			if id == "" || localEmbyIDs[id] || embyRemoteIsAdministrator(user) {
				continue
			}
			name := embyRemoteName(user)
			if name == "" {
				name = "(no name)"
			}
			unlinked = append(unlinked, user)
			logs = append(logs, "unlinked Emby user: "+id+" ("+name+")")
		}
		deleted := 0
		deletedIDs := []string{}
		failed := 0
		if !dryRun && delete {
			for _, user := range unlinked {
				if syncCtx.Err() != nil {
					break
				}
				id := embyRemoteID(user)
				if id == "" {
					continue
				}
				if err := a.embyDeleteUser(syncCtx, id); err != nil {
					failed++
					logs = append(logs, "delete failed for "+id+": "+truncateString(err.Error(), 120))
					continue
				}
				deleted++
				if len(deletedIDs) < auditUIDListLimit {
					deletedIDs = append(deletedIDs, id)
				}
				logs = append(logs, "deleted Emby user: "+id)
			}
		}
		if deleted > 0 {
			a.auditSystem("scheduler", "delete_unlinked_emby", 0, map[string]any{
				"unlinked":      len(unlinked),
				"deleted":       deleted,
				"failed":        failed,
				"dry_run":       dryRun || !delete,
				"emby_user_ids": deletedIDs,
			})
		}
		return map[string]any{"success": failed == 0 && syncCtx.Err() == nil, "unlinked": len(unlinked), "deleted": deleted, "failed": failed, "dry_run": dryRun || !delete}, logs, syncCtx.Err()
	case "cleanup_ticket_images":
		retentionDays := jobParamInt(params, "retention_days", a.cfg().TicketImageRetentionDays)
		if retentionDays <= 0 {
			return map[string]any{"success": true, "skipped": true, "reason": "retention disabled"}, []string{"ticket image retention disabled"}, nil
		}
		cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
		tickets := a.store().ClosedTicketsWithAttachmentsBefore(cutoff)
		cleanedTickets := 0
		removedImages := 0
		failed := 0
		logs := []string{}
		recordFailure := func(ticketID int64, operation string) {
			failed++
			if len(logs) < 50 {
				logs = append(logs, fmt.Sprintf("ticket %d: %s failed", ticketID, operation))
			}
		}
		for _, ticket := range tickets {
			// Finish files already detached for one ticket, then honor cancellation.
			if r.Context().Err() != nil {
				break
			}
			removed, err := a.store().DetachExpiredTicketAttachments(ticket.ID, store.TicketRevision(ticket), cutoff)
			if err != nil {
				recordFailure(ticket.ID, "detach metadata")
				zap.L().Warn("清空工单图片元数据失败", zap.Int64("ticket_id", ticket.ID), zap.Error(err))
				continue
			}
			if len(removed) == 0 {
				continue
			}
			cleanedTickets++
			removedImages += len(removed)
			dir, err := a.ticketAttachmentDir(ticket.ID)
			if err != nil {
				recordFailure(ticket.ID, "resolve directory")
				zap.L().Warn("解析工单图片清理目录失败", zap.Int64("ticket_id", ticket.ID))
				continue
			}
			for _, attachment := range removed {
				if !ticketImageFilenamePattern.MatchString(attachment.Filename) {
					recordFailure(ticket.ID, "validate filename")
					continue
				}
				target, err := ResolveWithinRoot(dir, attachment.Filename)
				if err != nil {
					recordFailure(ticket.ID, "resolve file")
					continue
				}
				if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
					recordFailure(ticket.ID, "remove file")
					zap.L().Warn("清理工单图片文件失败", zap.Int64("ticket_id", ticket.ID), zap.Error(err))
				}
			}
		}
		if cleanedTickets > 0 || removedImages > 0 {
			a.auditSystem("scheduler", "cleanup_ticket_images", 0, map[string]any{
				"tickets": cleanedTickets,
				"images":  removedImages,
				"failed":  failed,
			})
		}
		logs = append(logs, fmt.Sprintf("cleaned %d tickets, detached %d images older than %d days; %d failures", cleanedTickets, removedImages, retentionDays, failed))
		return map[string]any{"success": failed == 0 && r.Context().Err() == nil, "tickets": cleanedTickets, "images": removedImages, "failed": failed}, logs, r.Context().Err()
	case "sync_bangumi_watching":
		return a.runBangumiWatchSync(r.Context())
	case "refresh_bangumi_collections":
		if !a.cfg().BangumiManageEnabled {
			return map[string]any{"success": true, "enabled": false, "refreshed_users": 0}, []string{"Bangumi manage disabled"}, nil
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		scanned := 0
		eligible := 0
		refreshedUsers := 0
		refreshedLists := 0
		cachedEntries := 0
		failed := 0
		logs := []string{}
		for _, u := range a.store().ListUsers() {
			if err := ctx.Err(); err != nil {
				return map[string]any{"success": false, "terminated": true, "scanned": scanned, "eligible": eligible, "refreshed_users": refreshedUsers, "failed": failed}, logs, err
			}
			scanned++
			if u.BGMToken == "" || !u.BGMManageMode {
				continue
			}
			eligible++
			lists, entries, err := a.refreshBangumiCollectionCacheForUser(ctx, u)
			if err != nil {
				failed++
				logs = append(logs, fmt.Sprintf("user #%d (%s): %s", u.UID, u.Username, truncateString(err.Error(), 120)))
				continue
			}
			if lists > 0 {
				refreshedUsers++
				refreshedLists += lists
				cachedEntries += entries
			}
		}
		return map[string]any{"success": failed == 0, "enabled": true, "scanned": scanned, "eligible": eligible, "refreshed_users": refreshedUsers, "refreshed_lists": refreshedLists, "cached_entries": cachedEntries, "failed": failed},
			append(logs, fmt.Sprintf("refreshed %d users, %d lists, %d entries", refreshedUsers, refreshedLists, cachedEntries)), nil
	default:
		return map[string]any{"success": false}, nil, fmt.Errorf("unknown scheduler job: %s", jobID)
	}
}

func schedulerRequestParams(r *http.Request) map[string]any {
	if params, ok := r.Context().Value(schedulerParamsContextKey).(map[string]any); ok {
		return params
	}
	payload := decodeMap(r)
	if params, ok := payload["params"].(map[string]any); ok {
		return params
	}
	if params, ok := payload["runtime_params"].(map[string]any); ok {
		return params
	}
	return nil
}

func (a *App) schedulerEffectiveParams(r *http.Request, jobID string) map[string]any {
	if params, ok := r.Context().Value(schedulerFrozenParamsKey{}).(map[string]any); ok {
		return params
	}
	var stored map[string]any
	if schedule, ok := a.store().SchedulerSchedule(jobID); ok {
		stored = schedule.RuntimeParams
	}
	params := a.schedulerRuntimeParamsFromSchedule(jobID, stored)
	requestParams := schedulerRequestParams(r)
	if len(params) == 0 {
		return requestParams
	}
	for key, value := range requestParams {
		params[key] = value
	}
	return params
}

func embyRemoteID(user map[string]any) string {
	return strings.TrimSpace(firstNonEmpty(asString(user["Id"]), asString(user["ID"]), asString(user["id"])))
}

func embyRemoteName(user map[string]any) string {
	return strings.TrimSpace(firstNonEmpty(asString(user["Name"]), asString(user["name"]), asString(user["UserName"]), asString(user["Username"])))
}

func embyRemoteDisabled(user map[string]any) (bool, bool) {
	policy, _ := user["Policy"].(map[string]any)
	if policy == nil {
		policy, _ = user["policy"].(map[string]any)
	}
	if policy == nil {
		return false, false
	}
	value, ok := policy["IsDisabled"]
	if !ok {
		value, ok = policy["is_disabled"]
	}
	if !ok {
		return false, false
	}
	return boolish(value), true
}

func normalizeEmbyName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func isSyntheticEmbyID(id string, uid int64) bool {
	value := strings.TrimSpace(id)
	if value == "" || uid == 0 {
		return false
	}
	return strings.EqualFold(value, fmt.Sprintf("emby_%d", uid))
}

type schedulerParamsKey struct{}
type schedulerManualKey struct{}

var schedulerParamsContextKey schedulerParamsKey
var schedulerManualContextKey schedulerManualKey

func schedulerManualRun(r *http.Request) bool {
	manual, _ := r.Context().Value(schedulerManualContextKey).(bool)
	return manual
}

func schedulerSideEffectContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
}

// unverifiedEmailUIDsBefore 在清理前取出将被清掉邮箱的 uid（与 CleanupUnverifiedEmailsByAge
// 同口径），只用于稽核名单，最多 auditUIDListLimit 个。
func (a *App) unverifiedEmailUIDsBefore(cutoff int64) []int64 {
	uids := []int64{}
	for _, u := range a.store().ListUsers() {
		if u.Email != "" && !u.EmailVerified && u.CreatedAt > 0 && u.CreatedAt < cutoff {
			uids = appendLimitedUID(uids, u.UID)
		}
	}
	return uids
}

// embySyncLastCursor 读取最近一轮完成的 emby_sync 留下的 next_after_uid；没有就从头开始。
func (a *App) embySyncLastCursor() int64 {
	for _, run := range a.store().SchedulerRuns("emby_sync", 10) {
		if run.Status != "success" || run.Summary == nil {
			continue
		}
		if value, ok := run.Summary["next_after_uid"]; ok {
			return int64(numeric(value))
		}
	}
	return 0
}

// appendLimitedUID 往稽核用的 uid 清单追加，最多 auditUIDListLimit 个。
func appendLimitedUID(list []int64, uid int64) []int64 {
	if len(list) >= auditUIDListLimit {
		return list
	}
	return append(list, uid)
}

func jobParamInt(params map[string]any, key string, fallback int) int {
	if params == nil {
		return fallback
	}
	return intValue(params, key, fallback)
}

func jobParamBool(params map[string]any, key string, fallback bool) bool {
	if params == nil {
		return fallback
	}
	return boolValue(params, key, fallback)
}
