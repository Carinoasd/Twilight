package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

func TestV2InviteActionsUseAuthenticatedNoStoreResources(t *testing.T) {
	app := newTestApp(t)
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v2/invite/summary"},
		{http.MethodPost, "/api/v2/invite/codes"},
		{http.MethodPost, "/api/v2/invite/renew-codes"},
		{http.MethodDelete, "/api/v2/invite/codes/test"},
		{http.MethodPost, "/api/v2/invite/me/detach-expired"},
	} {
		response := doJSON(app, route.method, route.path, `{}`, nil)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s expected unauthenticated rejection, got %d body=%s", route.method, route.path, response.Code, response.Body.String())
		}
	}

	cookies := registerAndLogin(t, app, "v2-invite-owner", "InviteV2Owner123456")
	app.cfg().InviteEnabled = true
	app.cfg().InviteRequireEmby = false
	// 未开通 Emby 的用户只能在持有待开通资格时按资格天数发码（ExpiredAt=-1 不再视为永久）。
	pendingDays := 30
	if owner, ok := app.store().FindUserByUsername("v2-invite-owner"); ok {
		if _, err := app.store().UpdateUser(owner.UID, func(u *store.User) error { u.PendingEmby = true; u.PendingEmbyDays = &pendingDays; return nil }); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Fatal("owner missing")
	}

	read := doJSON(app, http.MethodGet, "/api/v2/invite/summary", "", cookies)
	if read.Code != http.StatusOK || read.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("summary status=%d cache=%q body=%s", read.Code, read.Header().Get("Cache-Control"), read.Body.String())
	}

	created := doJSON(app, http.MethodPost, "/api/v2/invite/codes", `{"days":7}`, cookies)
	if created.Code != http.StatusCreated || created.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(created.Body.String(), `"code"`) {
		t.Fatalf("create status=%d cache=%q body=%s", created.Code, created.Header().Get("Cache-Control"), created.Body.String())
	}
}
