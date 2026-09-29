package api

import (
	"fmt"
	"strings"

	"github.com/prejudice-studio/twilight/internal/store"
)

type notificationPreferences map[string]bool

func parseNotificationPreferences(payload map[string]any) (notificationPreferences, error) {
	out := notificationPreferences{}
	for _, key := range []string{"notify_on_ticket_email", "notify_on_expiry_email", "notify_on_expiry_telegram"} {
		if raw, exists := payload[key]; exists {
			value, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("%s 必须是布尔值", key)
			}
			out[key] = value
		}
	}
	return out, nil
}

func (p notificationPreferences) apply(u *store.User) {
	if value, ok := p["notify_on_ticket_email"]; ok {
		u.NotifyOnTicketEmail = value
	}
	if value, ok := p["notify_on_expiry_email"]; ok {
		u.NotifyOnExpiryEmail = value
	}
	if value, ok := p["notify_on_expiry_telegram"]; ok {
		u.NotifyOnExpiryTelegram = &value
	}
}

func (a *App) notificationEmailAvailable(u store.User) bool {
	return emailConfigured(a.cfg()) && u.Active && u.EmailVerified && strings.TrimSpace(u.Email) != ""
}

func (a *App) notificationChannels() map[string]bool {
	cfg := a.cfg()
	tg, email := a.telegramAvailable(), emailConfigured(cfg)
	return map[string]bool{
		"notify_on_login_telegram":  cfg.LoginNotifyTelegramEnabled && tg,
		"notify_on_login_email":     cfg.LoginNotifyEmailEnabled && email,
		"notify_on_ticket_telegram": cfg.TicketNotifyTelegramEnabled && tg,
		"notify_on_ticket_email":    cfg.TicketNotifyEmailEnabled && email,
		"notify_on_expiry_telegram": cfg.NotificationEnabled && cfg.ExpiryNotifyTelegramEnabled && tg,
		"notify_on_expiry_email":    cfg.NotificationEnabled && cfg.ExpiryNotifyEmailEnabled && email,
	}
}
