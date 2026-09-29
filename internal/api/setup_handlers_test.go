package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/config"
	"github.com/prejudice-studio/twilight/internal/store"
)

func newSetupTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	st := newTestStore(t)
	cfg := config.Config{
		AppName:                  "Twilight Test",
		Version:                  "test",
		Host:                     "127.0.0.1",
		Port:                     0,
		ConfigFile:               filepath.Join(dir, "config.toml"),
		DatabaseDir:              dir,
		DatabaseDriver:           store.BackendPostgres,
		DatabaseBackupDir:        filepath.Join(dir, "backups"),
		StateFile:                stateFile,
		UploadDir:                filepath.Join(dir, "uploads"),
		MaxUploadSize:            1024 * 1024,
		SessionCookie:            "twilight_session",
		SessionTTL:               time.Hour,
		CookieSameSite:           "lax",
		RateLimitEnabled:         true,
		RateLimitGlobalPerMinute: 1200,
		AuditLogEnabled:          true,
		AuditLogMaxEntries:       1000,
		TicketTypes:              []string{"all"},
		SetupMode:                true,
	}
	app, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func setupIntentHeaders() map[string]string {
	return map[string]string{
		"X-Twilight-Client": "webui",
		"X-Twilight-Intent": "complete-setup",
	}
}

func setupStatusFromRecorder(t *testing.T, rrBody string) map[string]any {
	t.Helper()
	var env envelope
	if err := json.Unmarshal([]byte(rrBody), &env); err != nil {
		t.Fatal(err)
	}
	data, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected setup status payload: %#v", env.Data)
	}
	return data
}

func TestSetupStatusAvailableOnEmptySystem(t *testing.T) {
	app := newSetupTestApp(t)
	rr := doJSON(app, http.MethodGet, "/api/v1/setup/status", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	data := setupStatusFromRecorder(t, rr.Body.String())
	if data["available"] != true {
		t.Fatalf("expected setup available, got %#v", data)
	}
	if data["setup_mode"] != true {
		t.Fatalf("expected setup mode enabled, got %#v", data)
	}

	info := doJSON(app, http.MethodGet, "/api/v1/system/info", "", nil)
	if info.Code != http.StatusOK || !strings.Contains(info.Body.String(), `"setup"`) {
		t.Fatalf("system info missing setup status: status=%d body=%s", info.Code, info.Body.String())
	}
}

func TestSetupCompleteRequiresWebIntent(t *testing.T) {
	app := newSetupTestApp(t)
	body := `{"admin":{"username":"owner","password":"Owner123456"}}`
	rr := doJSON(app, http.MethodPost, "/api/v1/setup/complete", body, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected bad request without intent, status=%d body=%s", rr.Code, rr.Body.String())
	}
	if app.store().UserCount() != 0 {
		t.Fatalf("setup without intent created users")
	}
}

func TestSetupCompleteCreatesAdminWritesConfigAndCloses(t *testing.T) {
	app := newSetupTestApp(t)
	body := `{
		"admin":{"username":"owner","password":"Owner123456","email":"owner@example.com"},
		"global":{"server_name":"My Twilight"},
		"emby":{"emby_url":"http://emby:8096","emby_token":"secret-token","emby_url_list":[{"name":"LAN","url":"http://192.168.1.10:8096"}]},
		"policy":{"register_mode":false,"register_code_limit":true}
	}`
	rr := doJSONWithHeaders(app, http.MethodPost, "/api/v1/setup/complete", body, nil, setupIntentHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", rr.Code, rr.Body.String())
	}
	if findCookie(rr.Result().Cookies(), "twilight_session") == nil {
		t.Fatalf("setup did not issue session cookie")
	}
	u, ok := app.store().FindUserByUsername("owner")
	if !ok {
		t.Fatal("setup admin user not created")
	}
	if u.Role != store.RoleAdmin || !u.Active || u.ExpiredAt != -1 {
		t.Fatalf("setup user is not active admin: %#v", u)
	}
	if !u.EmailVerified {
		t.Fatalf("setup admin email should be marked verified")
	}
	content, err := os.ReadFile(app.cfg().ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if !strings.Contains(text, "[Admin]") || !strings.Contains(text, `usernames = ["owner"]`) {
		t.Fatalf("setup did not write admin username: %s", text)
	}
	if strings.Contains(strings.ToLower(text), "setupmode") || strings.Contains(strings.ToLower(text), "setup_mode") {
		t.Fatalf("setup mode marker was not removed after completion: %s", text)
	}
	if strings.Contains(rr.Body.String(), "secret-token") {
		t.Fatalf("setup response leaked emby token: %s", rr.Body.String())
	}

	status := doJSON(app, http.MethodGet, "/api/v1/setup/status", "", nil)
	data := setupStatusFromRecorder(t, status.Body.String())
	if data["available"] != false {
		t.Fatalf("setup still available after completion: %#v", data)
	}

	second := doJSONWithHeaders(app, http.MethodPost, "/api/v1/setup/complete", body, nil, setupIntentHeaders())
	if second.Code != http.StatusForbidden {
		t.Fatalf("second setup should be forbidden, status=%d body=%s", second.Code, second.Body.String())
	}
}

func TestSetupUnavailableWithoutSetupMode(t *testing.T) {
	app := newSetupTestApp(t)
	app.cfg().SetupMode = false
	rr := doJSON(app, http.MethodGet, "/api/v1/setup/status", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	data := setupStatusFromRecorder(t, rr.Body.String())
	if data["available"] != false || data["setup_mode"] != false {
		t.Fatalf("setup should be unavailable without setup mode, got %#v", data)
	}
	body := `{"admin":{"username":"owner","password":"Owner123456"}}`
	complete := doJSONWithHeaders(app, http.MethodPost, "/api/v1/setup/complete", body, nil, setupIntentHeaders())
	if complete.Code != http.StatusForbidden {
		t.Fatalf("setup complete should be forbidden without setup mode, status=%d body=%s", complete.Code, complete.Body.String())
	}
}

func TestFirstRegisterWithoutAdminConfigStaysNormalUser(t *testing.T) {
	app := newSetupTestApp(t)
	rr := doJSON(app, http.MethodPost, "/api/v1/users/register", `{"username":"first","password":"First123456"}`, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("register status=%d body=%s", rr.Code, rr.Body.String())
	}
	u, ok := app.store().FindUserByUsername("first")
	if !ok {
		t.Fatal("registered user missing")
	}
	if u.Role == store.RoleAdmin {
		t.Fatalf("first normal registration unexpectedly became admin: %#v", u)
	}
}

// TestSetupDoesNotPersistEnvOrLocalOverrides 防回归：setup 落盘只能以配置文件本身
// 为底稿，env 与 .local 覆盖层里的密钥不得被写进 config.toml。
func TestSetupDoesNotPersistEnvOrLocalOverrides(t *testing.T) {
	app := newSetupTestApp(t)
	t.Setenv("TWILIGHT_BOT_INTERNAL_SECRET", "ENV_INTERNAL_SECRET_VALUE")
	t.Setenv("TWILIGHT_TELEGRAM_BOT_TOKEN", "ENV_BOT_TOKEN_VALUE")
	// 模拟启动时 Load() 合并了 env / .local 覆盖层后的生效值（写 .local 文件会触发
	// 热重载改变 setup 状态，这里直接设置内存生效值）。
	app.cfg().BotInternalSecret = "ENV_INTERNAL_SECRET_VALUE"
	app.cfg().TelegramBotToken = "ENV_BOT_TOKEN_VALUE"
	app.cfg().SMTPPassword = "LOCAL_SMTP_SECRET"
	body := `{"admin":{"username":"owner","password":"Owner123456"},"global":{"server_name":"My Twilight"}}`
	rr := doJSONWithHeaders(app, http.MethodPost, "/api/v1/setup/complete", body, nil, setupIntentHeaders())
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup status=%d body=%s", rr.Code, rr.Body.String())
	}
	content, err := os.ReadFile(app.cfg().ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ENV_INTERNAL_SECRET_VALUE", "ENV_BOT_TOKEN_VALUE", "LOCAL_SMTP_SECRET"} {
		if strings.Contains(string(content), secret) {
			t.Fatalf("override secret %q persisted into config.toml:\n%s", secret, content)
		}
	}
	if !strings.Contains(string(content), "My Twilight") {
		t.Fatalf("setup payload not written:\n%s", content)
	}
}
