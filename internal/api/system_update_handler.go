package api

import (
	"net/http"
)

func (a *App) handleSystemUpdate(w http.ResponseWriter, r *http.Request, _ Params) {
	payload := decodeMap(r)
	// 仓库 URL 不接受请求体覆盖：自助更新会把 git origin 指向该 URL，拉取后重启
	// 服务执行新代码，等价于"面板管理员 → 主机 RCE"。若允许每次请求传入任意
	// HTTPS 仓库，任何被盗的 admin 会话 / Bearer Token 都能把服务器指向攻击者的
	// fork（fork = 官方仓库 + 一条恶意 commit，--ff-only 拦不住）。源仓库只能由
	// 运维通过 config 的 SystemUpdateRepoURL 设定，且该字段改动需重启才生效，
	// 形成可审计、不可被单个 HTTP 请求篡改的边界。分支名仍可覆盖（经白名单
	// 校验，且只作用于这个可信仓库）。SystemUpdateRepoURL 为空时更新会安全失败。
	repoURL := a.cfg().SystemUpdateRepoURL
	branch := firstNonEmpty(stringValue(payload, "branch"), a.cfg().SystemUpdateBranch, "main")
	restart := boolValue(payload, "restart_services", a.cfg().SystemUpdateRestartServices)
	dryRun := boolValue(payload, "dry_run", false)
	allowDirty := boolValue(payload, "allow_dirty", false)
	result := applyGitUpdate(r.Context(), repoURL, branch, restart, dryRun, allowDirty)
	if dryRun {
		skipAuditForDryRun(r)
	} else {
		// 成功与失败都记：失败响应是 4xx/5xx，fallback 不会记录。
		a.audit(r, "system_update", "admin", 0, systemUpdateAuditDetail(result, branch))
	}
	if !boolish(result["success"]) {
		// 走标准 envelope 而非裸 map：保留 result 作为 data 字段下发，
		// error_code 由 result["error_code"] 透出，前端基于稳定码做分支
		status := int(numeric(result["code"]))
		if status < 400 {
			status = http.StatusInternalServerError
		}
		errCode, _ := result["error_code"].(ErrCode)
		if errCode == "" {
			errCode = ErrInternal
		}
		failWithCodeData(w, status, errCode, asString(result["message"]), result)
		return
	}
	ok(w, asString(result["message"]), result)
}

// systemUpdateAuditDetail 从 applyGitUpdate 的结果里摘出审计所需字段（不含命令输出）。
func systemUpdateAuditDetail(result map[string]any, branch string) map[string]any {
	commitOf := func(key string) string {
		if state, ok := result[key].(map[string]any); ok {
			return asString(state["commit"])
		}
		return ""
	}
	detail := map[string]any{
		"branch":        branch,
		"dry_run":       boolish(result["dry_run"]),
		"success":       boolish(result["success"]),
		"updated":       boolish(result["updated"]),
		"restarted":     boolish(result["restart_scheduled"]),
		"before_commit": commitOf("before"),
		"after_commit":  commitOf("after"),
	}
	if code, ok := result["error_code"].(ErrCode); ok && code != "" {
		detail["error_code"] = string(code)
	}
	return detail
}
