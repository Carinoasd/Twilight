package api

// 修复（操作日志缺口）：Telegram 各写操作以前只记成功，失败与验证失败完全无痕，
// 例如 /delAccount 的密码验证失败可被用来暴力试密码。这里只是调用现有的
// auditEntryIP / auditTelegramAction 写入额外的 *_failed 记录，不改稽核机制。

// telegramAuditError 把错误转成可安全写入审计 detail 的短文本。
func telegramAuditError(err error) string {
	if err == nil {
		return ""
	}
	return truncateString(redactSensitiveText(err.Error()), 200)
}

// auditTelegramFailure 记录 Telegram 管理操作失败（action 自动追加 _failed）。
func (a *App) auditTelegramFailure(actorTelegramID int64, action string, targetUID int64, err error, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["source"] = "telegram"
	if msg := telegramAuditError(err); msg != "" {
		detail["error"] = msg
	}
	a.auditTelegramAction(actorTelegramID, action+"_failed", "admin", targetUID, detail)
}

// auditSelfDeleteVerifyFailed 记录 /delAccount 验证失败（stage: web_password /
// emby_password / email_code）。
func (a *App) auditSelfDeleteVerifyFailed(uid int64, username, stage string, telegramID int64) {
	a.auditEntryIP("telegram", uid, username, "self_delete_verify_failed", "user", uid, map[string]any{
		"source":      "telegram",
		"stage":       stage,
		"telegram_id": telegramID,
	})
}
