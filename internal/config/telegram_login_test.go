package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTelegramLoginExplicitOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[Telegram]\nlogin_enabled = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWILIGHT_TELEGRAM_LOGIN_ENABLED", "")
	cfg, err := Load(path)
	if err != nil || !cfg.TelegramLoginEnabled {
		t.Fatalf("TOML not loaded: %v", err)
	}
	t.Setenv("TWILIGHT_TELEGRAM_LOGIN_ENABLED", "false")
	cfg, err = Load(path)
	if err != nil || cfg.TelegramLoginEnabled {
		t.Fatalf("env not applied: %v", err)
	}
	t.Setenv("TWILIGHT_TELEGRAM_LOGIN_ENABLED", "")
	if err = os.WriteFile(path, []byte("[Telegram]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil || cfg.TelegramLoginEnabled {
		t.Fatalf("default not closed: %v", err)
	}
}
