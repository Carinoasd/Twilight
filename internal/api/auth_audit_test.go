package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/security"
	"github.com/prejudice-studio/twilight/internal/store"
)

func TestAuditAPIKeyCannotBecomeWebSession(t *testing.T) {
	for _, role := range []int{store.RoleNormal, store.RoleAdmin} {
		for _, kind := range []string{"read-only", "default", "legacy"} {
			t.Run(fmt.Sprintf("role-%d/%s", role, kind), func(t *testing.T) {
				app := newTestApp(t)
				const key = "key-audit-local-fixture"
				user, err := app.store().CreateUser(store.User{Username: "audit-owner", Role: role, Active: true})
				if err != nil {
					t.Fatal(err)
				}
				permissions := defaultPermissions()
				if kind == "read-only" {
					permissions = []string{apiKeyPermissionAccountRead}
				}
				if kind == "legacy" {
					_, err = app.store().UpdateUser(user.UID, func(u *store.User) error {
						u.LegacyAPIKeyHash = hashAPIKey(key)
						u.LegacyAPIKeyStatus = true
						u.LegacyPermissions = permissions
						return nil
					})
				} else {
					_, err = app.store().CreateAPIKey(store.APIKey{UID: user.UID, Hash: hashAPIKey(key), Permissions: permissions})
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, version := range []string{"v1", "v2"} {
					prefix := "/api/" + version
					read := doJSONWithHeaders(app, http.MethodGet, prefix+"/apikey/info", "", nil, map[string]string{"X-API-Key": key})
					if read.Code != http.StatusOK {
						t.Fatalf("scoped read failed: %d %s", read.Code, read.Body.String())
					}
					before := app.sessions().ActiveCount(context.Background())
					response := doJSON(app, http.MethodPost, prefix+"/auth/login/apikey", `{"apikey":"`+key+`"}`, nil)
					if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), string(ErrAPIKeyPermissionDenied)) {
						t.Errorf("%s key exchanged for web session: status=%d", version, response.Code)
					}
					if len(response.Result().Cookies()) != 0 || strings.Contains(response.Body.String(), `"token":`) {
						t.Error("API key login issued session credentials")
					}
					if after := app.sessions().ActiveCount(context.Background()); after != before {
						t.Errorf("session count changed: %d -> %d", before, after)
					}
				}
			})
		}
	}
}

func TestAuditLoginAccountLimitPrecedesPasswordVerification(t *testing.T) {
	app := newTestApp(t)
	const password = "AuditPassword123456"
	hash, err := security.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.store().CreateUser(store.User{Username: "audit-login", Email: "audit@example.test", PasswordHash: hash, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	app.cfg().RateLimitEnabled = true
	app.cfg().RateLimitLoginPerMinute = 100
	app.cfg().RateLimitLoginUserPer5m = 1
	first := doJSON(app, http.MethodPost, "/api/v1/auth/login", `{"username":"audit-login","password":"incorrect"}`, nil)
	if first.Code != http.StatusUnauthorized {
		t.Fatalf("first attempt: %d", first.Code)
	}
	for _, payload := range []map[string]string{
		{"username": "audit-login", "password": password},
		{"email": "audit@example.test", "username": "unrelated-name", "password": password},
		{"username": "AUDIT@example.test", "password": password},
	} {
		body, _ := json.Marshal(payload)
		response := doJSON(app, http.MethodPost, "/api/v2/auth/login", string(body), nil)
		if response.Code != http.StatusTooManyRequests {
			t.Errorf("exhausted account budget allowed login: status=%d", response.Code)
		}
		if len(response.Result().Cookies()) != 0 {
			t.Error("throttled login issued a cookie")
		}
	}
	if count := app.sessions().ActiveCount(context.Background()); count != 0 {
		t.Errorf("throttled login created %d sessions", count)
	}
}

func TestAuditExpiredAPIKeyRejectedAtHTTPBoundary(t *testing.T) {
	app := newTestApp(t)
	user, err := app.store().CreateUser(store.User{Username: "expired-key-owner", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	const key = "key-expired-local-fixture"
	_, err = app.store().CreateAPIKey(store.APIKey{UID: user.UID, Hash: hashAPIKey(key), Permissions: defaultPermissions(), AllowQuery: true, ExpiredAt: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v1", "v2"} {
		prefix := "/api/" + version
		for _, headers := range []map[string]string{{"X-API-Key": key}, {"Authorization": "Bearer " + key}, {"Authorization": "ApiKey " + key}} {
			response := doJSONWithHeaders(app, http.MethodGet, prefix+"/apikey/info", "", nil, headers)
			if response.Code != http.StatusUnauthorized {
				t.Errorf("expired key accepted: %d", response.Code)
			}
		}
		query := doJSON(app, http.MethodGet, prefix+"/apikey/info?apikey="+key, "", nil)
		if query.Code != http.StatusUnauthorized {
			t.Errorf("expired query key accepted: %d", query.Code)
		}
		login := doJSON(app, http.MethodPost, prefix+"/auth/login/apikey", `{"apikey":"`+key+`"}`, nil)
		if login.Code != http.StatusUnauthorized {
			t.Errorf("expired login key accepted: %d", login.Code)
		}
	}
}
