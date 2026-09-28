package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// 生成随机密码与手动改密同一道门：只拿到会话不能把密码换掉。
func TestGeneratedPasswordRequiresOldPassword(t *testing.T) {
	app := newTestApp(t)
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	cookies := registerAndLogin(t, app, "member", "User123456")
	headers := map[string]string{"X-Twilight-Client": "webui"}
	for _, path := range []string{"/api/v1/users/me/password", "/api/v2/settings/password/generate"} {
		if rr := doJSONWithHeaders(app, http.MethodPut, path, `{}`, cookies, headers); rr.Code != http.StatusForbidden {
			t.Fatalf("%s without old password: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	rr := doJSONWithHeaders(app, http.MethodPut, "/api/v2/settings/password/generate", `{"old_password":"User123456"}`, cookies, headers)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "new_password") {
		t.Fatalf("with old password: %d %s", rr.Code, rr.Body.String())
	}
}

// TMDB 失败时回给前端的只能是固定文案，不能带出含 api_key 的上游 URL。
func TestMediaSearchFailureHidesUpstreamDetail(t *testing.T) {
	err := errors.New(`Get "https://api.themoviedb.org/3/search/multi?api_key=SECRETKEY123&query=x": dial tcp: i/o timeout`)
	got := mediaSearchFailure("tmdb", err)
	if strings.Contains(got, "SECRETKEY123") || strings.Contains(got, "themoviedb") {
		t.Fatalf("upstream detail leaked: %q", got)
	}
	if strings.Contains(redactSensitiveText(err.Error()), "SECRETKEY123") {
		t.Fatalf("log redaction does not strip api_key: %q", redactSensitiveText(err.Error()))
	}
}
