package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

// TestConfiguredAdminUsernameCannotBeHijacked 防回归：Admin.usernames 按名字提权，
// 系统已有用户后，任何人都不能靠注册或自助改名占用名单里的名字。
func TestConfiguredAdminUsernameCannotBeHijacked(t *testing.T) {
	app := newTestApp(t) // AdminUsernames=["admin"]
	registerAndLogin(t, app, "owner", "Owner123456")

	// 非首位注册配置的管理员用户名：拒绝，且不建账号。
	resp := doJSON(app, http.MethodPost, "/api/v1/users/register", `{"username":"Admin","password":"Admin123456"}`, nil)
	if resp.Code != http.StatusConflict {
		t.Fatalf("register reserved admin name status=%d body=%s", resp.Code, resp.Body.String())
	}
	if _, ok := app.store().FindUserByUsername("admin"); ok {
		t.Fatal("reserved admin username must not be registered by a non-first user")
	}

	mallory := registerAndLogin(t, app, "mallory", "Mallory123456")
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/v2/settings/username", `{"new_username":"admin"}`},
		{http.MethodPut, "/api/v1/users/me/username", `{"new_username":"ADMIN"}`},
		{http.MethodPut, "/api/v1/users/me", `{"username":"admin"}`},
	} {
		resp := doJSON(app, tc.method, tc.path, tc.body, mallory)
		if resp.Code != http.StatusConflict {
			t.Fatalf("%s %s status=%d body=%s", tc.method, tc.path, resp.Code, resp.Body.String())
		}
	}
	app.applyConfiguredAdmins()
	u, _ := app.store().FindUserByUsername("mallory")
	if u.Role == store.RoleAdmin {
		t.Fatalf("mallory must not become admin: %#v", u)
	}

	// 普通改名不受影响。
	if resp := doJSON(app, http.MethodPut, "/api/v2/settings/username", `{"new_username":"mallory2"}`, mallory); resp.Code != http.StatusOK {
		t.Fatalf("ordinary rename status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func loginWithUA(t *testing.T, app *App, username, password, ua string) (*httptest.ResponseRecorder, []*http.Cookie) {
	t.Helper()
	resp := doJSONWithHeaders(app, http.MethodPost, "/api/v1/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, username, password), nil, map[string]string{"User-Agent": ua})
	if cookie := findCookie(resp.Result().Cookies(), "twilight_session"); cookie != nil {
		return resp, []*http.Cookie{cookie}
	}
	return resp, nil
}

// TestBlockedDeviceIsEnforced 防回归：封禁设备要吊销其会话、拒绝其再登录，
// 用户不能用 trust / delete 自行解封，trust 也不能凭空新建设备。
func TestBlockedDeviceIsEnforced(t *testing.T) {
	app := newTestApp(t)
	admin := registerAdmin(t, app, "admin", "Admin123456")
	registerAndLogin(t, app, "victim", "Victim123456")
	victim, _ := app.store().FindUserByUsername("victim")

	_, sessA := loginWithUA(t, app, "victim", "Victim123456", "DeviceA")
	if sessA == nil {
		t.Fatal("login on DeviceA failed")
	}
	block := doJSON(app, http.MethodPost, fmt.Sprintf("/api/v2/admin/security/users/%d/devices/DeviceA/block", victim.UID), "", admin)
	if block.Code != http.StatusOK {
		t.Fatalf("block status=%d body=%s", block.Code, block.Body.String())
	}
	if resp := doJSON(app, http.MethodGet, "/api/v1/users/me", "", sessA); resp.Code != http.StatusUnauthorized {
		t.Fatalf("session on blocked device must be revoked, status=%d", resp.Code)
	}
	resp, _ := loginWithUA(t, app, "victim", "Victim123456", "DeviceA")
	if resp.Code != http.StatusForbidden || !strings.Contains(resp.Body.String(), string(ErrDeviceBlocked)) {
		t.Fatalf("login from blocked device status=%d body=%s", resp.Code, resp.Body.String())
	}

	_, sessB := loginWithUA(t, app, "victim", "Victim123456", "DeviceB")
	if sessB == nil {
		t.Fatal("login from another device should still work")
	}
	if resp := doJSON(app, http.MethodPost, "/api/v2/security/devices/DeviceA/trust", "", sessB); resp.Code != http.StatusForbidden {
		t.Fatalf("self-unblock via trust status=%d body=%s", resp.Code, resp.Body.String())
	}
	if resp := doJSON(app, http.MethodPost, "/api/v1/security/devices/DeviceA/trust", "", sessB); resp.Code != http.StatusForbidden {
		t.Fatalf("v1 self-unblock via trust status=%d body=%s", resp.Code, resp.Body.String())
	}
	if resp := doJSON(app, http.MethodDelete, "/api/v2/security/devices/DeviceA", "", sessB); resp.Code != http.StatusForbidden {
		t.Fatalf("self-unblock via delete status=%d body=%s", resp.Code, resp.Body.String())
	}
	if d, ok := app.store().Device(victim.UID, "DeviceA"); !ok || !d.Blocked {
		t.Fatalf("DeviceA must stay blocked: ok=%v %+v", ok, d)
	}
	if resp := doJSON(app, http.MethodPost, "/api/v2/security/devices/ghost-device/trust", "", sessB); resp.Code != http.StatusNotFound {
		t.Fatalf("trust unknown device status=%d body=%s", resp.Code, resp.Body.String())
	}
	if _, ok := app.store().Device(victim.UID, "ghost-device"); ok {
		t.Fatal("trust must not create devices")
	}
	if resp := doJSON(app, http.MethodPost, "/api/v2/security/devices/DeviceB/trust", "", sessB); resp.Code != http.StatusOK {
		t.Fatalf("trust own device status=%d body=%s", resp.Code, resp.Body.String())
	}
}

// TestDeviceLimitRevokesEvictedSessions 防回归：设备数上限淘汰旧设备时要吊销其会话。
func TestDeviceLimitRevokesEvictedSessions(t *testing.T) {
	app := newTestApp(t)
	app.cfg().DeviceLimitEnabled = true
	app.cfg().MaxDevices = 1
	registerAndLogin(t, app, "limited", "Limited123456")

	_, sessA := loginWithUA(t, app, "limited", "Limited123456", "DeviceA")
	_, sessB := loginWithUA(t, app, "limited", "Limited123456", "DeviceB")
	if sessA == nil || sessB == nil {
		t.Fatal("logins failed")
	}
	if resp := doJSON(app, http.MethodGet, "/api/v1/users/me", "", sessA); resp.Code != http.StatusUnauthorized {
		t.Fatalf("evicted device session must be revoked, status=%d", resp.Code)
	}
	if resp := doJSON(app, http.MethodGet, "/api/v1/users/me", "", sessB); resp.Code != http.StatusOK {
		t.Fatalf("newest device session must stay valid, status=%d", resp.Code)
	}
}

func TestDeviceIDNormalization(t *testing.T) {
	if got := loginDeviceID("bad id\n", "UA", ""); got != "UA" {
		t.Fatalf("invalid header should fall back to UA, got %q", got)
	}
	if got := loginDeviceID("app-1234", "UA", ""); got != "app-1234" {
		t.Fatalf("valid header should be used, got %q", got)
	}
	long := strings.Repeat("x", 1000)
	if got := loginDeviceID("", long, ""); len(got) > maxDeviceIDLength || !strings.HasPrefix(got, "h:") {
		t.Fatalf("overlong UA must be digested, got %q", got)
	}
	if validDeviceID(strings.Repeat("a", maxDeviceIDLength+1)) || validDeviceID("a\x00b") {
		t.Fatal("validDeviceID accepted an invalid id")
	}
}
