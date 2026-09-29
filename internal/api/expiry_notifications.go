package api

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (a *App) sendExpiryReminders(ctx context.Context, days int) map[string]any {
	now := time.Now().Unix()
	deadline := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	users := []map[string]any{}
	failed := []map[string]any{}
	sent, telegramSent, emailSent := 0, 0, 0
	consecutiveRateLimited, telegramSkipped := 0, 0
	firstTelegram, telegramAborted := true, false
	aborted := ""
	for _, u := range a.store().ListUsers() {
		if ctx.Err() != nil {
			aborted = "context_canceled"
			break
		}
		if !u.Active || u.ExpiredAt <= now || u.ExpiredAt > deadline {
			continue
		}
		remaining := u.ExpiredAt - now
		users = append(users, map[string]any{"uid": u.UID, "username": u.Username, "telegram_id": nullableInt(u.TelegramID), "expired_at": u.ExpiredAt, "remaining_seconds": remaining, "remaining_str": formatSeconds(remaining)})
		if !a.cfg().NotificationEnabled {
			continue
		}
		text := fmt.Sprintf("%s，您的账号将在 %s 后到期，请及时续期。", u.Username, formatSeconds(remaining))
		delivered := false
		var telegramErr error
		if a.cfg().ExpiryNotifyTelegramEnabled && a.telegramAvailable() && u.TelegramID != 0 && u.ExpiryTelegramNotificationsEnabled() {
			if telegramAborted {
				telegramSkipped++
			} else {
				if !firstTelegram {
					select {
					case <-ctx.Done():
						aborted = "context_canceled"
					case <-time.After(35 * time.Millisecond):
					}
					if ctx.Err() != nil {
						break
					}
				}
				firstTelegram = false
				telegramErr = a.telegramSendMessage(ctx, u.TelegramID, text)
				if telegramErr != nil {
					failed = append(failed, map[string]any{"uid": u.UID, "username": u.Username, "telegram_id": u.TelegramID, "channel": "telegram", "error": a.telegramSanitizeError(telegramErr)})
					if _, rateLimited := telegramRetryAfterFromError(telegramErr); rateLimited || strings.Contains(strings.ToLower(telegramErr.Error()), "too many requests") {
						consecutiveRateLimited++
						if consecutiveRateLimited >= 5 {
							telegramAborted = true
							aborted = "rate_limited"
						}
					} else {
						consecutiveRateLimited = 0
					}
				} else {
					telegramSent++
					delivered = true
					consecutiveRateLimited = 0
				}
			}
		}
		// An unavailable or rate-limited Telegram channel must not suppress email.
		if ctx.Err() == nil && a.cfg().ExpiryNotifyEmailEnabled && u.NotifyOnExpiryEmail && a.notificationEmailAvailable(u) {
			if err := smtpDeliver(ctx, *a.cfg(), u.Email, "账号到期提醒", text); err != nil {
				failed = append(failed, map[string]any{"uid": u.UID, "username": u.Username, "channel": "email", "error": redactSensitiveText(err.Error())})
			} else {
				emailSent++
				delivered = true
			}
		}
		if delivered {
			sent++
		}
		if !telegramAborted && !telegramRateLimitPauseContext(ctx, telegramErr) {
			aborted = "context_canceled"
			break
		}
	}
	if ctx.Err() != nil {
		aborted = "context_canceled"
	}
	success := aborted == "" && len(failed) == 0
	result := map[string]any{"success": success, "partial": !success && sent > 0, "sent": sent, "telegram_sent": telegramSent, "email_sent": emailSent, "telegram_skipped": telegramSkipped, "total": len(users), "count": len(users), "users": users, "failed": failed, "failed_count": len(failed), "telegram_enabled": a.telegramAvailable() && a.cfg().ExpiryNotifyTelegramEnabled, "email_enabled": emailConfigured(a.cfg()) && a.cfg().ExpiryNotifyEmailEnabled, "notification_enabled": a.cfg().NotificationEnabled, "days": days}
	if aborted != "" {
		result["aborted"] = aborted
	}
	if telegramAborted {
		result["consecutive_rate_limited"] = consecutiveRateLimited
	}
	if !success {
		result["error"] = fmt.Sprintf("reminder delivery incomplete: %d recipients reached, %d channel failures, %d Telegram deliveries skipped", sent, len(failed), telegramSkipped)
	}
	return result
}
