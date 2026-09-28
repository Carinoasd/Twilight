package api

import (
	"net/http"
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
