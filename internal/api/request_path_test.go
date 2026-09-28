package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestPathCanonical(t *testing.T) {
	for path, want := range map[string]bool{
		"/":                                  true,
		"/api/v1/users/me/sessions":          true,
		"/api/v1/users/me/sessions/":         true,
		"/api/v1/users/admin/../me/sessions": false,
		"/api/v1/users/./me":                 false,
		"/api/v1//users/me":                  false,
		"/api/v1/users/me/..":                false,
		"api/v1/users/me":                    false,
		"":                                   false,
	} {
		if got := requestPathCanonical(path); got != want {
			t.Fatalf("requestPathCanonical(%q)=%v want %v", path, got, want)
		}
	}
}

// 普通用户不能用 “admin/..” 让会话接口进入管理员视图。
func TestNonCanonicalPathCannotReachAdminSessionView(t *testing.T) {
	app := newTestApp(t)
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	cookies := registerAndLogin(t, app, "member", "User123456")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me/sessions", nil)
	req.URL.Path = "/api/v1/users/admin/../me/sessions"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("non-canonical path status=%d body=%s", rr.Code, rr.Body.String())
	}
}
