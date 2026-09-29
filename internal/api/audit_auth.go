package api

import (
	"net/http"
	"strings"

	"github.com/prejudice-studio/twilight/internal/store"
)

// 本文件集中认证 / 会话 / 个人设置 / 设备相关的明确审计辅助。
//
// 这些写入原先只有 fallback 审计（只有路由模板、没有改了什么），或者根本没有审计
// （AuthPublic 路由、4xx 失败）。detail 只记录能回答“谁、对谁、改了什么”的字段；
// 键名避开审计脱敏规则（*code、*token、*password 等会被替换成 [REDACTED]）。

// auditFromTo 生成审计 detail 里的“改前 / 改后”子对象。
func auditFromTo(from, to any) map[string]any {
	return map[string]any{"from": from, "to": to}
}

// selfProfileChanges 对比用户自助修改资料前后的差异，只列出发生变化的字段。
// 邮箱只记遮蔽后的值；BGM token 只记是否已设置。
func selfProfileChanges(before, after store.User) map[string]any {
	changes := map[string]any{}
	if before.Username != after.Username {
		changes["username"] = auditFromTo(before.Username, after.Username)
	}
	if !strings.EqualFold(before.Email, after.Email) {
		changes["email"] = auditFromTo(maskEmail(before.Email), maskEmail(after.Email))
	}
	if before.EmailVerified != after.EmailVerified {
		changes["email_verified"] = auditFromTo(before.EmailVerified, after.EmailVerified)
	}
	boolFields := []struct {
		name   string
		before bool
		after  bool
	}{
		{"bgm_mode", before.BGMMode, after.BGMMode},
		{"bgm_manage_mode", before.BGMManageMode, after.BGMManageMode},
		{"bgm_bound", before.BGMToken != "", after.BGMToken != ""},
		{"notify_ticket_email", before.NotifyOnTicketEmail, after.NotifyOnTicketEmail},
		{"notify_expiry_email", before.NotifyOnExpiryEmail, after.NotifyOnExpiryEmail},
		{"notify_expiry_telegram", before.ExpiryTelegramNotificationsEnabled(), after.ExpiryTelegramNotificationsEnabled()},
		{"notify_login_telegram", before.NotifyOnLoginTelegram, after.NotifyOnLoginTelegram},
		{"notify_login_email", before.NotifyOnLoginEmail, after.NotifyOnLoginEmail},
		{"notify_ticket_telegram", before.NotifyOnTicketTelegram, after.NotifyOnTicketTelegram},
		{"signin_auto_renewal", before.SigninAutoRenewal, after.SigninAutoRenewal},
		// 改密保护开关：键名避开 “password” 以免被审计脱敏。
		{"email_guard_web_pwd_change", before.RequireEmailForPasswordChange, after.RequireEmailForPasswordChange},
		{"email_guard_emby_pwd_change", before.RequireEmailForEmbyPasswordChange, after.RequireEmailForEmbyPasswordChange},
		{"old_pwd_guard_emby_pwd_change", before.RequireOldPasswordForEmbyPasswordChange, after.RequireOldPasswordForEmbyPasswordChange},
	}
	for _, field := range boolFields {
		if field.before != field.after {
			changes[field.name] = auditFromTo(field.before, field.after)
		}
	}
	return changes
}

// auditLoginFailure 记录一次失败的密码登录。
//
// 只记账号存在时的目标 uid 与遮蔽后的标识；账号不存在时不记原始输入——用户偶尔会
// 把密码误填进用户名框，原样落库会泄露密码。限流（429）与参数缺失（400）不记，
// 避免被刷爆审计表。
func (a *App) auditLoginFailure(r *http.Request, input loginInput, failure *loginFailure) {
	if failure == nil || failure.Status == http.StatusTooManyRequests || failure.Status == http.StatusBadRequest {
		return
	}
	identifier := input.Username
	if input.Email != "" {
		identifier = input.Email
	}
	var target store.User
	var found bool
	if input.Email != "" || strings.Contains(identifier, "@") {
		target, found = a.store().FindUserByEmail(identifier)
	} else {
		target, found = a.store().FindUserByUsername(identifier)
	}
	detail := map[string]any{
		"error_code": string(failure.Code),
		"ip":         input.IP,
		"device":     loginDeviceID(input.DeviceID, input.UserAgent, input.IP),
		"known_user": found,
	}
	if found {
		detail["username"] = target.Username
	}
	a.auditWithUser(r, 0, "", "login_failed", "user", target.UID, detail)
}

// auditLogout 记录登出（all=true 为“登出全部设备”）。须在吊销会话之前调用，
// 以便读到当前会话所属设备。
func (a *App) auditLogout(r *http.Request, p principal, all bool) {
	record, _ := a.sessions().GetRecord(r.Context(), p.Token)
	action := "logout"
	if all {
		action = "logout_all"
	}
	a.audit(r, action, "user", p.User.UID, map[string]any{"device": record.DeviceID})
}
