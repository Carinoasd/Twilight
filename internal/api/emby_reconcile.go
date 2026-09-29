package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// embyReconcileDefaultMaxChanges 是 Emby 状态对账单轮最多改动的账号数。计划改动超过
// 这个数时整轮中止、不改任何账号：通常意味着本地数据被回滚或 Emby 指向了别的服务器，
// 需要管理员先确认，而不是让对账任务一口气启停大量账号。
const embyReconcileDefaultMaxChanges = 200

// runEmbyStateReconcile 是 emby_state_reconcile 调度任务：把远端 Emby 的启停状态收敛到
// 本地应有的状态。它只修启停，不做名称认领或 ID 回填（那些仍归手动的 emby_sync），
// 只处理两种情况：
//
//  1. 本地应停用（Web 已停用或已过期）但远端仍启用 → 停用。补上 check_expired /
//     群成员巡检里 Emby 调用失败被漏掉的账号。
//  2. 远端停用是本系统按 Web 状态做的（EmbyAutoDisabled），而 Web 现在已恢复
//     （启用且未过期）→ 重新启用。补上续期、回群启用时 Emby 调用失败的账号。
//
// 管理员单独封禁的 Emby（没有 EmbyAutoDisabled 标记）永远不会被启用；远端 Emby
// 管理员账号一律跳过。
func (a *App) runEmbyStateReconcile(ctx context.Context, dryRun bool, maxChanges int) (map[string]any, []string, error) {
	result := map[string]any{"configured": a.embyConfigured(), "dry_run": dryRun, "max_changes": maxChanges,
		"checked": 0, "disabled": 0, "enabled": 0, "failed": 0, "mirror_fixed": 0, "missing_remote": 0,
		"skipped_emby_admin": 0, "skipped_manual_ban": 0}
	logs := []string{}
	if !a.embyConfigured() {
		result["success"] = true
		return result, []string{"Emby not configured"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	var remote []map[string]any
	if err := embyRetryOn5xx(ctx, func(c context.Context) error { return a.embyGet(c, "/Users", &remote) }); err != nil {
		result["success"] = false
		return result, nil, err
	}
	remoteByID := make(map[string]map[string]any, len(remote))
	for _, user := range remote {
		if id := embyRemoteID(user); id != "" {
			remoteByID[id] = user
		}
	}
	type reconcilePlan struct {
		user   store.User
		enable bool
	}
	plans := []reconcilePlan{}
	for _, u := range a.store().ListUsers() {
		if err := ctx.Err(); err != nil {
			result["success"] = false
			return result, append(logs, "job terminated"), err
		}
		if u.EmbyID == "" || isSyntheticEmbyID(u.EmbyID, u.UID) {
			continue
		}
		remoteUser, ok := remoteByID[u.EmbyID]
		if !ok {
			result["missing_remote"] = int(numeric(result["missing_remote"])) + 1
			continue
		}
		remoteDisabled, known := embyRemoteDisabled(remoteUser)
		if !known {
			continue
		}
		result["checked"] = int(numeric(result["checked"])) + 1
		if embyRemoteIsAdministrator(remoteUser) {
			result["skipped_emby_admin"] = int(numeric(result["skipped_emby_admin"])) + 1
			continue
		}
		switch {
		case embyShouldDisableForWebState(u) && !remoteDisabled:
			plans = append(plans, reconcilePlan{user: u, enable: false})
		case remoteDisabled && a.embyShouldEnableUser(u) && u.EmbyDisabled && u.EmbyAutoDisabled:
			plans = append(plans, reconcilePlan{user: u, enable: true})
		case remoteDisabled && a.embyShouldEnableUser(u):
			// 远端停用、Web 正常、但不是系统停用的 → 管理员单独封禁，保持不动。
			result["skipped_manual_ban"] = int(numeric(result["skipped_manual_ban"])) + 1
			if !dryRun && !u.EmbyDisabled {
				a.mirrorEmbyDisabled(u.UID, true)
				result["mirror_fixed"] = int(numeric(result["mirror_fixed"])) + 1
			}
		default:
			// 远端状态已符合预期，只修正本地镜像漂移。
			if !dryRun && u.EmbyDisabled != remoteDisabled {
				a.mirrorEmbyDisabled(u.UID, remoteDisabled)
				result["mirror_fixed"] = int(numeric(result["mirror_fixed"])) + 1
			}
		}
	}
	result["planned"] = len(plans)
	if maxChanges > 0 && len(plans) > maxChanges {
		result["success"] = false
		result["aborted"] = true
		msg := fmt.Sprintf("Emby 状态对账计划改动 %d 个账号，超过单轮上限 %d，已中止且未改动任何账号；请先确认本地数据与 Emby 服务器是否对应，再调高 max_changes 手动执行", len(plans), maxChanges)
		return result, append(logs, msg), errors.New(msg)
	}
	disabledUIDs, enabledUIDs, failedUIDs := []int64{}, []int64{}, []int64{}
	for _, plan := range plans {
		u := plan.user
		action := map[bool]string{true: "enable", false: "disable"}[plan.enable]
		if dryRun {
			if len(logs) < 100 {
				logs = append(logs, fmt.Sprintf("dry-run: would %s Emby uid=%d username=%s", action, u.UID, u.Username))
			}
			if plan.enable {
				enabledUIDs = appendLimitedUID(enabledUIDs, u.UID)
			} else {
				disabledUIDs = appendLimitedUID(disabledUIDs, u.UID)
			}
			continue
		}
		err := embyRetryOn5xx(ctx, func(c context.Context) error {
			if plan.enable {
				return a.embyApplyEnabledState(c, u.UID, u.EmbyID, true)
			}
			_, err := a.disableRemoteEmbyForWebState(c, u)
			return err
		})
		if err != nil {
			result["failed"] = int(numeric(result["failed"])) + 1
			failedUIDs = appendLimitedUID(failedUIDs, u.UID)
			if len(logs) < 100 {
				logs = append(logs, fmt.Sprintf("failed to %s Emby uid=%d: %s", action, u.UID, redactSensitiveText(err.Error())))
			}
			continue
		}
		if plan.enable {
			result["enabled"] = int(numeric(result["enabled"])) + 1
			enabledUIDs = appendLimitedUID(enabledUIDs, u.UID)
		} else {
			result["disabled"] = int(numeric(result["disabled"])) + 1
			disabledUIDs = appendLimitedUID(disabledUIDs, u.UID)
		}
		if len(logs) < 100 {
			logs = append(logs, fmt.Sprintf("%sd Emby uid=%d username=%s", action, u.UID, u.Username))
		}
	}
	result["disabled_uids"] = disabledUIDs
	result["enabled_uids"] = enabledUIDs
	result["failed_uids"] = failedUIDs
	if !dryRun && (len(disabledUIDs) > 0 || len(enabledUIDs) > 0 || len(failedUIDs) > 0) {
		a.auditSystem("scheduler", "emby_state_reconcile", 0, map[string]any{
			"disabled":      int(numeric(result["disabled"])),
			"enabled":       int(numeric(result["enabled"])),
			"failed":        int(numeric(result["failed"])),
			"disabled_uids": disabledUIDs,
			"enabled_uids":  enabledUIDs,
			"failed_uids":   failedUIDs,
		})
	}
	result["success"] = int(numeric(result["failed"])) == 0
	logs = append(logs, fmt.Sprintf("checked %d Emby accounts: disabled %d, enabled %d, failed %d, mirror fixed %d", int(numeric(result["checked"])), int(numeric(result["disabled"])), int(numeric(result["enabled"])), int(numeric(result["failed"])), int(numeric(result["mirror_fixed"]))))
	return result, logs, nil
}
