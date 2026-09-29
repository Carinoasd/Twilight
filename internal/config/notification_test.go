package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNotificationChannelDefaultsAndOverrides(t *testing.T) {
	cfg := defaults()
	if !cfg.LoginNotifyTelegramEnabled || !cfg.LoginNotifyEmailEnabled || !cfg.ExpiryNotifyTelegramEnabled || !cfg.TicketNotifyTelegramEnabled || !cfg.SchedulerNotifyTelegramEnabled {
		t.Fatal("legacy channels must stay enabled")
	}
	if cfg.ExpiryNotifyEmailEnabled || cfg.TicketNotifyEmailEnabled || cfg.SchedulerNotifyEmailEnabled {
		t.Fatal("new email channels must be opt-in")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[Notification]\nlogin_telegram_enabled=false\nlogin_email_enabled=false\nexpiry_telegram_enabled=false\nexpiry_email_enabled=true\nticket_telegram_enabled=false\nticket_email_enabled=true\nscheduler_telegram_enabled=false\nscheduler_email_enabled=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFileOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoginNotifyTelegramEnabled || cfg.LoginNotifyEmailEnabled || cfg.ExpiryNotifyTelegramEnabled || cfg.TicketNotifyTelegramEnabled || cfg.SchedulerNotifyTelegramEnabled || !cfg.ExpiryNotifyEmailEnabled || !cfg.TicketNotifyEmailEnabled || !cfg.SchedulerNotifyEmailEnabled {
		t.Fatal("explicit configuration lost")
	}
	t.Setenv("TWILIGHT_NOTIFICATION_TICKET_EMAIL_ENABLED", "false")
	t.Setenv("TWILIGHT_NOTIFICATION_EXPIRY_TELEGRAM_ENABLED", "true")
	applyEnv(&cfg)
	if cfg.TicketNotifyEmailEnabled || !cfg.ExpiryNotifyTelegramEnabled {
		t.Fatal("environment overrides lost")
	}
}
