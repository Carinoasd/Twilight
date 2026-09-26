package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

func TestTelegramBindingCannotBypassApprovedUnbind(t *testing.T) {
	app := newTestApp(t)
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	cookies := registerAndLogin(t, app, "bindinguser", "User123456")
	app.cfg().TelegramBotToken = "123:test-token"
	app.cfg().TelegramMode = true
	u, _ := app.store().FindUserByUsername("bindinguser")
	_, err := app.store().UpdateUser(u.UID, func(u *store.User) error { u.TelegramID = 111; return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/users/me/telegram/bind-code", "/api/v2/me/telegram/bind-code"} {
		rr := doJSONWithHeaders(app, http.MethodGet, path, "", cookies, bindCodeCreateTestHeaders())
		if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), ErrTGAlreadyBound) {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	// A previously issued code must not overwrite an identity bound in the meantime.
	code := "STALEBIND12"
	if err := app.upsertBindCode(store.BindCode{Code: code, UID: u.UID, Scene: "user", ExpiresAt: time.Now().Unix() + 60}); err != nil {
		t.Fatal(err)
	}
	rr := doLoopbackJSON(app, http.MethodPost, "/api/v1/users/me/telegram/bind-confirm", fmt.Sprintf(`{"code":%q,"telegram_id":222}`, code))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), ErrTGAlreadyBound) {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	got, _ := app.store().User(u.UID)
	if got.TelegramID != 111 {
		t.Fatal("existing binding overwritten")
	}
}

func TestTelegramUnbindV1V2ReturnCommittedStateAndHistory(t *testing.T) {
	for _, path := range []string{"/api/v1/users/me/telegram/unbind", "/api/v2/telegram/unbind"} {
		t.Run(path, func(t *testing.T) {
			app := newTestApp(t)
			_ = registerAndLogin(t, app, "admin", "Admin123456")
			cookies := registerAndLogin(t, app, "bindinguser", "User123456")
			u, _ := app.store().FindUserByUsername("bindinguser")
			_, err := app.store().UpdateUser(u.UID, func(u *store.User) error { u.TelegramID = 111; u.TelegramUsername = "old"; return nil })
			if err != nil {
				t.Fatal(err)
			}
			request, err := app.store().CreateRebindRequest(store.RebindRequest{UID: u.UID, OldTelegramID: 111})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.store().ReviewRebindRequest(request.ID, 1, "approved", "reviewed"); err != nil {
				t.Fatal(err)
			}
			rr := doJSONWithHeaders(app, http.MethodPost, path, "", cookies, map[string]string{"X-Twilight-Client": "webui"})
			if rr.Code != http.StatusOK {
				t.Fatalf("unbind: %d %s", rr.Code, rr.Body.String())
			}
			var response struct {
				Data struct {
					Rebinding bool `json:"rebinding_in_progress"`
				} `json:"data"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil || !response.Data.Rebinding {
				t.Fatalf("stale response: %s, %v", rr.Body.String(), err)
			}
			history, err := app.store().GetTelegramIdentityHistory(context.Background(), u.UID, 10)
			if err != nil || len(history) != 1 || history[0].ChangeType != "unbind" {
				t.Fatalf("history=%#v err=%v", history, err)
			}
		})
	}
}

func TestTelegramBindFallbackUsesSignedHTTPAndDeliversBusinessError(t *testing.T) {
	app := newTestApp(t)
	app.cfg().TelegramBotToken = "123:test-token"
	app.cfg().TelegramMode = true
	app.cfg().BotInternalSecret = "shared-test-key"
	var confirmed atomic.Bool
	confirmationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authenticateTelegramBindRequest(r, app.telegramBindSigningKey(), time.Now()) {
			t.Error("Bot did not authenticate its confirmation")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		confirmed.Store(true)
		failWithCode(w, http.StatusNotFound, ErrTGBindCodeExpired, "expired")
	}))
	defer confirmationServer.Close()
	parsed, _ := url.Parse(confirmationServer.URL)
	app.cfg().Port, _ = strconv.Atoi(parsed.Port())
	messages := make(chan string, 2)
	telegramServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		messages <- payload.Text
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer telegramServer.Close()
	app.cfg().TelegramAPIURL = telegramServer.URL
	app.confirmBindCodeViaHTTP(context.Background(), 42, "ABCDEF12", 42, "user")
	if !confirmed.Load() {
		t.Fatal("confirmation endpoint not called")
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "无效或已过期") || strings.Contains(message, "ABCDEF12") {
			t.Fatalf("message=%q", message)
		}
	default:
		t.Fatal("no Telegram reply")
	}
}

func TestTelegramBindConfirmationRequiresSignatureOnBothRoutes(t *testing.T) {
	app := newTestApp(t)
	app.cfg().TelegramBotToken = "123:test-token"
	app.cfg().BotInternalSecret = ""
	const code = "SIGNTEST12"
	body := []byte(`{"code":"SIGNTEST12","telegram_id":42}`)
	for _, path := range []string{
		"/api/v1/users/me/telegram/bind-confirm",
		"/api/v2/registration/telegram/bind-confirm",
	} {
		t.Run(path, func(t *testing.T) {
			if err := app.upsertBindCode(store.BindCode{Code: code, Scene: "register", ExpiresAt: time.Now().Unix() + 60}); err != nil {
				t.Fatal(err)
			}
			for _, signed := range []bool{false, true} {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
				req.RemoteAddr = "127.0.0.1:54321"
				req.Header.Set("Content-Type", "application/json")
				if signed {
					signTelegramBindRequest(req, body, app.cfg().TelegramBotToken, time.Now())
				}
				rr := httptest.NewRecorder()
				app.ServeHTTP(rr, req)
				if !signed {
					if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), ErrInternalSecretInvalid) {
						t.Fatalf("unsigned request: %d %s", rr.Code, rr.Body.String())
					}
					if bind, _ := app.bindCode(code); bind.Confirmed {
						t.Fatal("unsigned request changed binding state")
					}
				} else if rr.Code != http.StatusOK {
					t.Fatalf("signed request with shared Bot token: %d %s", rr.Code, rr.Body.String())
				}
			}
		})
	}
}

func TestTelegramRebindCompletionV1V2AuditsOnce(t *testing.T) {
	for _, path := range []string{"/api/v1/users/me/telegram/rebind-complete", "/api/v2/me/telegram/rebind-complete"} {
		t.Run(path, func(t *testing.T) {
			app := newTestApp(t)
			app.cfg().AuditLogEnabled = true
			cookies := registerAndLogin(t, app, "rebinduser", "User123456")
			u, _ := app.store().FindUserByUsername("rebinduser")
			if _, err := app.store().UpdateUser(u.UID, func(u *store.User) error {
				u.TelegramID, u.RebindingInProgress, u.RebindingSince = 111, true, 100
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				rr := doJSONWithHeaders(app, http.MethodPost, path, "", cookies, map[string]string{"X-Twilight-Client": "webui"})
				if rr.Code != http.StatusOK {
					t.Fatalf("complete: %d %s", rr.Code, rr.Body.String())
				}
			}
			got, _ := app.store().User(u.UID)
			if got.RebindingInProgress || got.RebindingSince != 0 {
				t.Fatal("rebind state not cleared")
			}
			count := 0
			for _, entry := range app.store().ListAuditLogs() {
				if entry.Action == "complete_telegram_rebind" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("completion audit count=%d", count)
			}
		})
	}
}

func TestTelegramRebindCompletionDoesNotOverwriteConcurrentState(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		upstreamCode int
		wantCode     int
	}{
		{"member-identity-changed", "member", http.StatusOK, http.StatusConflict},
		{"membership-failure", "", http.StatusBadGateway, http.StatusForbidden},
		{"membership-missing", "left", http.StatusOK, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			cookies := registerAndLogin(t, app, "rebinduser", "User123456")
			u, _ := app.store().FindUserByUsername("rebinduser")
			if _, err := app.store().UpdateUser(u.UID, func(u *store.User) error {
				u.TelegramID, u.RebindingInProgress, u.RebindingSince = 111, true, 100
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			other := reopenTestStore(t)
			var checked atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/getChatMember") {
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				checked.Store(true)
				// Simulate another process changing the account while membership
				// for the request's original identity is being checked.
				if _, err := other.UpdateUser(u.UID, func(u *store.User) error {
					u.TelegramID = 222
					u.RebindingInProgress = tc.status == "member"
					u.RebindingSince = 0
					if u.RebindingInProgress {
						u.RebindingSince = 200
					}
					return nil
				}); err != nil {
					t.Error(err)
				}
				w.WriteHeader(tc.upstreamCode)
				if tc.upstreamCode == http.StatusOK {
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"status": tc.status}})
				} else {
					_, _ = w.Write([]byte(`{"ok":false,"description":"test upstream failure"}`))
				}
			}))
			defer server.Close()
			app.cfg().TelegramAPIURL = server.URL
			app.cfg().TelegramBotToken = "123:test-token"
			app.cfg().TelegramMode = true
			app.cfg().TelegramForceBindGroup = true
			app.cfg().TelegramGroupIDs = []string{"-1001"}
			rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/me/telegram/rebind-complete", "", cookies, map[string]string{"X-Twilight-Client": "webui"})
			if rr.Code != tc.wantCode || !checked.Load() {
				t.Fatalf("complete: checked=%v %d %s", checked.Load(), rr.Code, rr.Body.String())
			}
			got, _ := reopenTestStore(t).User(u.UID)
			wantSince := int64(0)
			if tc.status == "member" {
				wantSince = 200
			}
			if got.TelegramID != 222 || got.RebindingInProgress != (tc.status == "member") || got.RebindingSince != wantSince {
				t.Fatalf("overwrote concurrent state: %#v", got)
			}
		})
	}
}

func TestTelegramStatusDoesNotOfferStaleApproval(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store().CreateUser(store.User{Username: "statususer", Role: store.RoleNormal, TelegramID: 111})
	if err != nil {
		t.Fatal(err)
	}
	req, err := app.store().CreateRebindRequest(store.RebindRequest{UID: u.UID, OldTelegramID: 222})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().ReviewRebindRequest(req.ID, 77, "approved", "old identity"); err != nil {
		t.Fatal(err)
	}
	for _, result := range []telegramStatusResult{app.telegram().status(u), app.telegramStatusFields(u)} {
		if result.CanUnbind || result.RebindApproved {
			t.Fatal("approval for another identity offered as usable")
		}
	}
}
