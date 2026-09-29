package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
	"go.uber.org/zap"
)

var schedulerJobs = []map[string]any{
	{"id": "check_expired", "name": "检查已过期用户", "description": "扫描已过期账号，先按签到配置尝试自动积分续期，再禁用未续期账号并清除过期会话。", "manual_only": false, "enabled": true},
	{"id": "check_expiring", "name": "检查即将到期用户", "description": "统计近期即将到期的用户数量，供管理员评估续期风险。", "manual_only": false, "enabled": true},
	{"id": "expiry_reminders", "name": "发送到期提醒", "description": "向即将到期且已绑定 Telegram 的用户发送续期通知。", "manual_only": false, "enabled": true},
	{"id": "daily_stats", "name": "每日统计", "description": "记录每日用户总数与活跃用户数。", "manual_only": false, "enabled": true},
	{"id": "cleanup_sessions", "name": "会话巡检与清理", "description": "清理过期会话与邮箱验证码，并读取 Emby 当前活跃会话数。", "manual_only": false, "enabled": true},
	{"id": "emby_sync", "name": "同步 Emby 用户", "description": "将本地用户与 Emby 远程用户的 ID、名称、禁用状态同步，修复占位 ID。用户多于 max_users 时分批执行，下一次手动执行会接着上一批继续。", "manual_only": true, "enabled": true, "runtime_params": []string{"max_users", "after_uid"}},
	{"id": "emby_state_reconcile", "name": "Emby 状态对账", "description": "按 Web 账号状态收敛 Emby 启停：停用应停用却仍启用的账号，重新启用本系统自动停用、Web 已恢复的账号；不做名称认领，不启用管理员单独封禁的 Emby。", "manual_only": false, "enabled": true, "runtime_params": []string{"dry_run", "max_changes"}},
	{"id": "cleanup_no_emby", "name": "清理无 Emby 账号", "description": "删除注册后长期未绑定 Emby 且无开通资格的 Web 账号。", "manual_only": false, "enabled": true},
	{"id": "cleanup_pending_emby_entitlements", "name": "清理未使用的 Emby 开通资格", "description": "收回发放超过指定天数仍未创建 Emby 的开通资格，保留 Web 账号。", "manual_only": false, "enabled": true, "runtime_params": []string{"enabled", "days", "dry_run"}},
	{"id": "enforce_group_membership", "name": "Telegram 群成员校验", "description": "校验用户是否仍在要求的群组内，按配置处理退群（禁用/封禁/自动解禁）。群组层级错误或拟停用人数超过熔断阈值时整轮中止；支持仅预览。", "manual_only": false, "enabled": true, "runtime_params": []string{"dry_run", "auto_enable_rejoined", "breaker_percent", "breaker_max"}},
	{"id": "check_telegram_bindings", "name": "Telegram 绑定检查", "description": "扫描重复或异常的 Telegram 绑定关系。", "manual_only": false, "enabled": true},
	{"id": "system_auto_update", "name": "系统自动更新", "description": "从 Git 拉取更新并选择性重启服务。", "manual_only": false, "enabled": false},
	{"id": "auto_backup_database", "name": "定期数据库备份", "description": "每天备份一次数据库，只保留最近 N 份自动备份（手动备份不受影响）。需在配置中开启或保存运行参数启用。", "manual_only": false, "enabled": true, "runtime_params": []string{"enabled", "keep"}},
	{"id": "cleanup_unused_uploads", "name": "清理未使用上传文件", "description": "删除未被引用的过期间接上传文件。", "manual_only": false, "enabled": true},
	{"id": "cleanup_audit_logs", "name": "审计日志自动清理", "description": "按保留天数/条数策略清理过期操作日志，可保留管理员记录。", "manual_only": false, "enabled": true},
	{"id": "cleanup_ticket_images", "name": "清理过期工单图片", "description": "按保留天数清理已关闭工单的图片附件及元数据。", "manual_only": false, "enabled": true},
	{"id": "refresh_bangumi_collections", "name": "刷新 Bangumi 收藏缓存", "description": "每小时为开启 BGM 管理且配置 Token 的用户缓存在看、想看、看过收藏列表。", "manual_only": false, "enabled": true},
	{"id": "sync_emby_activity_logs", "name": "同步 Emby 活动日志", "description": "每 10 分钟从 Emby 拉取活动日志并存入本地，用于活动审计与播放记录入库。", "manual_only": false, "enabled": true, "runtime_params": []string{"since_hours"}},
	{"id": "cleanup_unlinked_emby", "name": "清理孤立 Emby 账号", "description": "扫描 Emby 中未绑定任何 Web 账号的孤立用户，支持仅扫描与删除模式。默认不自动执行，保存自定义排程后才会自动运行。", "manual_only": false, "enabled": true, "runtime_params": []string{"dry_run", "delete"}},
	{"id": "cleanup_emby_devices", "name": "清理 Emby 设备记录", "description": "通过 Emby 管理员接口删除历史设备记录，自动跳过 Twilight 自身设备与受保护用户。", "manual_only": true, "enabled": true, "runtime_params": []string{"dry_run", "max_workers", "skip_usernames"}},
	{"id": "kick_unknown_group_members", "name": "踢出未知 Telegram 群成员", "description": "根据观察到的群成员名册，踢出无账号/未绑定 Emby/已禁用的成员。", "manual_only": true, "enabled": true, "runtime_params": []string{"dry_run", "max_per_run"}},
}

func (a *App) handleSchedulerJobs(w http.ResponseWriter, r *http.Request, _ Params) {
	jobs := make([]map[string]any, 0, len(schedulerJobs))
	loc := a.schedulerLocation()
	now := time.Now().In(loc)

	// Batch-fetch all snapshots in a single lock acquisition instead of
	// N separate SchedulerRunSnapshot calls (one per job). This reduces
	// lock contention from ~26 RLock/RUnlock cycles to 1 per request.
	jobIDs := make([]string, 0, len(schedulerJobs))
	for _, job := range schedulerJobs {
		jobIDs = append(jobIDs, fmt.Sprint(job["id"]))
	}
	activeJobIDs := a.schedulerActiveJobIDs(jobIDs)
	overview, err := a.store().ReadSchedulerOverview(r.Context(), jobIDs, 20)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "调度状态暂不可用")
		return
	}

	for i, job := range schedulerJobs {
		item := cloneMap(job)
		jobID := jobIDs[i]
		var spec map[string]any
		if schedulerJobManualOnly(jobID) {
			// manual_only is a backend invariant. A stale or directly injected
			// persisted schedule must not make a maintenance job look automatic.
			spec = map[string]any{"type": "manual"}
			item["is_custom"] = false
			item["runtime_params"] = a.schedulerDefaultRuntimeParams(jobID)
			if schedule := overview.Schedules[jobID]; schedule.IsCustom {
				item["is_custom"] = true
				item["runtime_params"] = a.schedulerRuntimeParamsFromSchedule(jobID, schedule.RuntimeParams)
			}
		} else if schedule, okSchedule := overview.Schedules[jobID]; okSchedule && schedule.IsCustom {
			spec = schedule.TriggerSpec
			item["is_custom"] = schedule.IsCustom
			item["runtime_params"] = a.schedulerRuntimeParamsFromSchedule(jobID, schedule.RuntimeParams)
		} else {
			spec = a.schedulerDefaultTriggerSpec(jobID)
			item["is_custom"] = false
			item["runtime_params"] = a.schedulerDefaultRuntimeParams(jobID)
		}
		item["schedule_revision"] = overview.Schedules[jobID].Revision
		item["trigger_spec"] = spec
		item["default_trigger_spec"] = a.schedulerDefaultTriggerSpec(jobID)
		item["last_run"] = nil
		snapshot := overview.Runs[jobID]
		running := activeJobIDs[jobID] || schedulerSnapshotRecentlyRunning(snapshot, now)
		// 被配置关闭的任务（如未开启的 system_auto_update）永远不会自动入队，
		// 不能再显示「下次运行时间」误导管理员。
		enabledByConfig := a.cfg().SchedulerEnabled && schedulerJobEnabledByConfig(a.cfg().SystemUpdateEnabled, job)
		if enabledByConfig && !running {
			item["next_run_at"] = zeroNil(a.schedulerNextAutomaticRunAt(jobID, spec, now, snapshot))
		} else {
			item["next_run_at"] = nil
		}
		item["auto_disabled"] = schedulerTriggerDisabled(spec) || !enabledByConfig
		if runs := snapshot.Runs; len(runs) > 0 {
			item["last_run"] = schedulerRunListView(runs[0])
			if snapshot.HasLatestAuto {
				item["last_auto_run_at"] = zeroNil(snapshot.LatestAuto.StartedAt)
			}
			if snapshot.HasLatestManual {
				item["last_manual_run_at"] = zeroNil(snapshot.LatestManual.StartedAt)
			}
		}
		item["is_running"] = running
		jobs = append(jobs, item)
	}
	_, offset := now.Zone()
	ok(w, "OK", map[string]any{"jobs": jobs, "timezone": loc.String(), "utc_offset_seconds": offset})
}

func (a *App) handleSchedulerTerminate(w http.ResponseWriter, r *http.Request, params Params) {
	jobID := params["job_id"]
	if !schedulerJobExists(jobID) {
		failWithCode(w, http.StatusNotFound, ErrSchedulerJobNotFound, "调度任务不存在")
		return
	}
	run, found, err := a.store().RequestSchedulerCancellation(r.Context(), jobID)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "取消请求暂不可用")
		return
	}
	// The owner observes the persisted request on its heartbeat. Cancelling by
	// job ID here could accidentally cancel a newer run after this one finishes.
	a.audit(r, "scheduler_terminate", "admin", 0, map[string]any{"job_id": jobID, "cancel_requested": found})
	ok(w, "cancellation request processed", map[string]any{"job_id": jobID, "cancel_requested": found, "terminated": found && run.Status == "cancelled", "already_stopped": !found, "last_run": run})
}
func (a *App) handleSchedulerLastRun(w http.ResponseWriter, r *http.Request, params Params) {
	runs, err := a.schedulerRunsForRead(r, params["job_id"], 1)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "调度历史暂不可用")
		return
	}
	var last any
	if len(runs) > 0 {
		last = schedulerRunListView(runs[0])
	}
	ok(w, "OK", map[string]any{"job_id": params["job_id"], "last_run": last})
}
func (a *App) handleSchedulerHistory(w http.ResponseWriter, r *http.Request, params Params) {
	// The UI only needs a short, bounded history. Do not let an arbitrary
	// query value turn persisted log output into a large response.
	limit := clamp(queryInt(r, "limit", 20), 1, 20)
	runs, err := a.schedulerRunsForRead(r, params["job_id"], limit)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "调度历史暂不可用")
		return
	}
	ok(w, "OK", map[string]any{"job_id": params["job_id"], "history": runs, "total": len(runs)})
}
func (a *App) handleSchedulerSchedule(w http.ResponseWriter, r *http.Request, params Params) {
	jobID := params["job_id"]
	if !schedulerJobExists(jobID) {
		failWithCode(w, http.StatusNotFound, ErrSchedulerJobNotFound, "调度任务不存在")
		return
	}
	var expected *int64
	if raw := r.URL.Query().Get("expected_revision"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 || value > 9007199254740991 {
			fail(w, http.StatusBadRequest, "调度版本无效")
			return
		}
		expected = &value
	}
	if r.Method == http.MethodDelete {
		schedule, err := a.store().SetSchedulerScheduleRevision(jobID, a.schedulerDefaultTriggerSpec(jobID), nil, false, expected)
		if errors.Is(err, store.ErrConflict) {
			failWithCode(w, http.StatusConflict, ErrSchedulerRevisionConflict, "调度配置已被修改，请重新加载")
			return
		}
		if statusFromError(w, err) {
			return
		}
		a.audit(r, "scheduler_reset_schedule", "admin", 0, map[string]any{"job_id": jobID})
		ok(w, "schedule reset", map[string]any{"job_id": jobID, "trigger_spec": schedule.TriggerSpec, "runtime_params": a.schedulerDefaultRuntimeParams(jobID), "is_custom": false, "schedule_revision": schedule.Revision})
		return
	}
	payload := decodeMap(r)
	if value, ok := payload["expected_revision"]; ok {
		number, valid := value.(float64)
		if !valid || number < 0 || number > 9007199254740991 || number != float64(int64(number)) {
			fail(w, http.StatusBadRequest, "调度版本无效")
			return
		}
		revision := int64(number)
		expected = &revision
	}
	spec := map[string]any{"type": firstNonEmpty(stringValue(payload, "type"), "interval")}
	if schedulerJobManualOnly(jobID) {
		// Do not allow a crafted PUT to opt manual maintenance jobs into the
		// automatic scheduler. Runtime parameters may still be customized.
		spec = map[string]any{"type": "manual"}
	} else if spec["type"] == "manual" {
		spec = map[string]any{"type": "manual"}
	} else if spec["type"] == "cron_daily" {
		spec["hour"] = clamp(intValue(payload, "hour", 0), 0, 23)
		spec["minute"] = clamp(intValue(payload, "minute", 0), 0, 59)
	} else {
		spec["type"] = "interval"
		spec["seconds"] = clamp(intValue(payload, "seconds", 3600), 60, 604800)
	}
	runtimeParams := a.schedulerRuntimeParamsFromPayload(jobID, payload)
	schedule, err := a.store().SetSchedulerScheduleRevision(jobID, spec, runtimeParams, true, expected)
	if errors.Is(err, store.ErrConflict) {
		failWithCode(w, http.StatusConflict, ErrSchedulerRevisionConflict, "调度配置已被修改，请重新加载")
		return
	}
	if statusFromError(w, err) {
		return
	}
	a.audit(r, "scheduler_update_schedule", "admin", 0, map[string]any{"job_id": jobID, "trigger_type": spec["type"]})
	ok(w, "schedule updated", map[string]any{"job_id": jobID, "trigger_spec": schedule.TriggerSpec, "runtime_params": a.schedulerRuntimeParamsFromSchedule(jobID, schedule.RuntimeParams), "is_custom": true, "schedule_revision": schedule.Revision})
}

func schedulerJobManualOnly(jobID string) bool {
	for _, job := range schedulerJobs {
		if fmt.Sprint(job["id"]) == jobID {
			return boolish(job["manual_only"])
		}
	}
	return false
}

func (a *App) schedulerDefaultRuntimeParams(jobID string) map[string]any {
	switch jobID {
	case "cleanup_no_emby":
		days := a.cfg().AutoCleanupNoEmbyDays
		if days <= 0 {
			days = 7
		}
		return map[string]any{"enabled": a.cfg().AutoCleanupNoEmby, "auto_enabled": a.cfg().AutoCleanupNoEmby, "days": days, "preserve_tg_bound": a.cfg().EmbyDirectRegisterEnabled}
	case "cleanup_pending_emby_entitlements":
		return map[string]any{"enabled": a.cfg().AutoCleanupPendingEmby, "auto_enabled": a.cfg().AutoCleanupPendingEmby, "days": pendingEmbyCleanupDays(a.cfg().AutoCleanupPendingEmbyDays)}
	case "cleanup_audit_logs":
		return map[string]any{"enabled": a.cfg().AuditLogAutoCleanupEnabled, "auto_enabled": a.cfg().AuditLogAutoCleanupEnabled, "retention_days": a.cfg().AuditLogRetentionDays, "max_entries": a.cfg().AuditLogMaxEntries, "preserve_admin": a.cfg().AuditLogPreserveAdmin}
	case "cleanup_ticket_images":
		return map[string]any{"retention_days": a.cfg().TicketImageRetentionDays}
	case "sync_emby_activity_logs":
		return map[string]any{"since_hours": 24}
	case "cleanup_unlinked_emby":
		return map[string]any{"dry_run": true, "delete": false}
	case "cleanup_emby_devices":
		return map[string]any{"dry_run": true, "max_workers": embyDeviceCleanupDefaultWorkers, "skip_usernames": []string{}}
	case "kick_unknown_group_members":
		return map[string]any{"dry_run": true, "max_per_run": 200}
	case "emby_state_reconcile":
		return map[string]any{"dry_run": false, "max_changes": embyReconcileDefaultMaxChanges}
	case "auto_backup_database":
		return map[string]any{"enabled": a.cfg().SchedulerAutoBackupEnabled, "keep": autoBackupKeep(a.cfg().SchedulerAutoBackupKeep)}
	case "enforce_group_membership":
		return map[string]any{"auto_enable_rejoined": a.cfg().TelegramAutoEnableRejoined, "breaker_percent": a.cfg().TelegramMembershipBreakerPercent, "breaker_max": a.cfg().TelegramMembershipBreakerMax}
	default:
		return nil
	}
}

func (a *App) schedulerRuntimeParamsFromSchedule(jobID string, stored map[string]any) map[string]any {
	defaults := a.schedulerDefaultRuntimeParams(jobID)
	if len(defaults) == 0 {
		return nil
	}
	out := cloneMap(defaults)
	for key, value := range stored {
		out[key] = value
	}
	return a.normalizeSchedulerRuntimeParams(jobID, out)
}

func (a *App) schedulerRuntimeParamsFromPayload(jobID string, payload map[string]any) map[string]any {
	params := schedulerRuntimeParamsMap(payload["runtime_params"])
	if len(params) == 0 {
		params = payload
	}
	defaults := a.schedulerDefaultRuntimeParams(jobID)
	if len(defaults) == 0 {
		return nil
	}
	out := cloneMap(defaults)
	for key, value := range params {
		out[key] = value
	}
	return a.normalizeSchedulerRuntimeParams(jobID, out)
}

func (a *App) normalizeSchedulerRuntimeParams(jobID string, params map[string]any) map[string]any {
	switch jobID {
	case "cleanup_no_emby":
		enabled := boolValue(params, "enabled", boolValue(params, "auto_enabled", a.cfg().AutoCleanupNoEmby))
		days := clamp(intValue(params, "days", a.cfg().AutoCleanupNoEmbyDays), 1, 3650)
		return map[string]any{"enabled": enabled, "auto_enabled": enabled, "days": days, "preserve_tg_bound": boolValue(params, "preserve_tg_bound", a.cfg().EmbyDirectRegisterEnabled)}
	case "cleanup_pending_emby_entitlements":
		enabled := boolValue(params, "enabled", boolValue(params, "auto_enabled", a.cfg().AutoCleanupPendingEmby))
		return map[string]any{"enabled": enabled, "auto_enabled": enabled, "days": clamp(intValue(params, "days", pendingEmbyCleanupDays(a.cfg().AutoCleanupPendingEmbyDays)), 1, 3650)}
	case "cleanup_audit_logs":
		enabled := boolValue(params, "enabled", boolValue(params, "auto_enabled", a.cfg().AuditLogAutoCleanupEnabled))
		// 前端可能发送 "days" 作为 "retention_days" 的别名
		retentionDays := clamp(intValue(params, "retention_days", intValue(params, "days", a.cfg().AuditLogRetentionDays)), 0, 3650)
		maxEntries := clamp(intValue(params, "max_entries", a.cfg().AuditLogMaxEntries), 0, 100000)
		return map[string]any{"enabled": enabled, "auto_enabled": enabled, "retention_days": retentionDays, "max_entries": maxEntries, "preserve_admin": boolValue(params, "preserve_admin", a.cfg().AuditLogPreserveAdmin)}
	case "cleanup_ticket_images":
		retentionDays := clamp(intValue(params, "retention_days", intValue(params, "days", a.cfg().TicketImageRetentionDays)), 0, 3650)
		return map[string]any{"retention_days": retentionDays}
	case "kick_unknown_group_members":
		return map[string]any{"dry_run": boolValue(params, "dry_run", true), "max_per_run": clamp(intValue(params, "max_per_run", 200), 1, 500)}
	case "enforce_group_membership":
		return map[string]any{
			"auto_enable_rejoined": boolValue(params, "auto_enable_rejoined", a.cfg().TelegramAutoEnableRejoined),
			"breaker_percent":      clamp(intValue(params, "breaker_percent", a.cfg().TelegramMembershipBreakerPercent), 0, 100),
			"breaker_max":          clamp(intValue(params, "breaker_max", a.cfg().TelegramMembershipBreakerMax), 0, 1000000),
		}
	case "auto_backup_database":
		return map[string]any{"enabled": boolValue(params, "enabled", a.cfg().SchedulerAutoBackupEnabled), "keep": clamp(intValue(params, "keep", autoBackupKeep(a.cfg().SchedulerAutoBackupKeep)), 1, 365)}
	case "emby_state_reconcile":
		return map[string]any{"dry_run": boolValue(params, "dry_run", false), "max_changes": clamp(intValue(params, "max_changes", embyReconcileDefaultMaxChanges), 0, 100000)}
	case "emby_sync":
		out := map[string]any{"max_users": clamp(intValue(params, "max_users", 1000), 1, 50000)}
		// after_uid 只在显式传入时保留；缺省时任务会接着上一轮的游标继续。
		if _, ok := params["after_uid"]; ok {
			out["after_uid"] = max(intValue(params, "after_uid", 0), 0)
		}
		return out
	case "sync_emby_activity_logs":
		return map[string]any{"since_hours": clamp(intValue(params, "since_hours", 24), 1, 720)}
	case "cleanup_unlinked_emby":
		return map[string]any{"dry_run": boolValue(params, "dry_run", true), "delete": boolValue(params, "delete", false)}
	case "cleanup_emby_devices":
		return map[string]any{
			"dry_run":        boolValue(params, "dry_run", true),
			"max_workers":    clamp(intValue(params, "max_workers", embyDeviceCleanupDefaultWorkers), 1, embyDeviceCleanupMaxWorkers),
			"skip_usernames": embyDeviceCleanupSkipList(params["skip_usernames"]),
		}
	default:
		return nil
	}
}

func schedulerRuntimeParamsMap(value any) map[string]any {
	params, _ := value.(map[string]any)
	return params
}

func (a *App) reconcileSchedulerRunState(jobID string, running bool, now time.Time) {
	if running {
		return
	}
	if _, err := a.store().SchedulerStateOverview([]string{jobID}, 1, nil, now.Unix()-schedulerRunningWindowSeconds, now.Unix()); err != nil {
		zap.L().Warn("scheduler run reconciliation failed", zap.String("job_id", jobID), zap.Error(err))
	}
}

func (a *App) schedulerActiveJobIDs(jobIDs []string) map[string]bool {
	active := make(map[string]bool)
	for _, jobID := range jobIDs {
		if a.schedulerJobRunning(jobID) {
			active[jobID] = true
		}
	}
	return active
}

func (a *App) schedulerRunsForRead(r *http.Request, jobID string, limit int) ([]store.SchedulerRun, error) {
	overview, err := a.store().ReadSchedulerHistory(r.Context(), jobID, limit)
	return overview.Runs[jobID].Runs, err
}

func schedulerRunListView(run store.SchedulerRun) store.SchedulerRun {
	run.Logs = nil
	return run
}

// pendingEmbyCleanupDays 给未使用开通资格清理一个下限，配置为 0 或负数时回退 7 天。
func pendingEmbyCleanupDays(days int) int {
	if days <= 0 {
		return 7
	}
	return days
}

func autoBackupKeep(keep int) int {
	if keep <= 0 {
		return 7
	}
	return keep
}
