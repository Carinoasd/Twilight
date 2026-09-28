package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// 未开通 Emby 的普通用户 ExpiredAt=-1 表示"未设置"，不能按永久号发 365 天邀请码。
func TestMaxCodeDaysDoesNotTreatUnactivatedUserAsPermanent(t *testing.T) {
	app := newTestApp(t)
	pendingDays := 30
	pending := store.User{UID: 10, Role: store.RoleNormal, Active: true, ExpiredAt: -1, PendingEmby: true, PendingEmbyDays: &pendingDays}
	if days, _ := app.maxCodeDays(pending); days != 30 {
		t.Fatalf("pending user must be capped by pending days, got %d", days)
	}
	bare := store.User{UID: 11, Role: store.RoleNormal, Active: true, ExpiredAt: -1}
	if days, _ := app.maxCodeDays(bare); days != 0 {
		t.Fatalf("user without Emby and without pending grant must not mint codes, got %d", days)
	}
	admin := store.User{UID: 12, Role: store.RoleAdmin, Active: true, ExpiredAt: -1}
	if days, _ := app.maxCodeDays(admin); days != 365 {
		t.Fatalf("admin keeps permanent cap, got %d", days)
	}
	bound := store.User{UID: 13, Role: store.RoleNormal, Active: true, ExpiredAt: -1, EmbyID: "e"}
	if days, _ := app.maxCodeDays(bound); days != 365 {
		t.Fatalf("permanent Emby user keeps permanent cap, got %d", days)
	}
}

// 延后开通 Emby 时，开通后的到期仍不得超过邀请人的到期时间。
func TestRegisterEmbyClampsInviteGrantToInviterExpiry(t *testing.T) {
	app := newTestApp(t)
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/Users/New":
			_, _ = w.Write([]byte(`{"Id":"inv-emby","Name":"inv-emby"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/Users/inv-emby":
			_, _ = w.Write([]byte(`{"Id":"inv-emby","Name":"inv-emby","Policy":{}}`))
		case r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer emby.Close()
	app.cfg().EmbyURL = emby.URL
	app.cfg().EmbyToken = "test-token"

	inviterExpiry := time.Now().Add(5 * 24 * time.Hour).Unix()
	inviter, err := app.store().CreateUser(store.User{Username: "clamp-inviter", Role: store.RoleNormal, Active: true, EmbyID: "p-emby", ExpiredAt: inviterExpiry})
	if err != nil {
		t.Fatal(err)
	}
	child, err := app.store().CreateUser(store.User{Username: "clamp-child", Role: store.RoleNormal, Active: true, ExpiredAt: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store().UpsertInviteCode(store.InviteCode{Code: "CLAMP-INV", UID: inviter.UID, InviterUID: inviter.UID, Days: 30, UseCountLimit: 1, Active: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().ConsumeInviteCode("CLAMP-INV", child.UID); err != nil {
		t.Fatal(err)
	}
	days := 30
	child, err = app.store().UpdateUser(child.UID, func(u *store.User) error {
		u.PendingEmby = true
		u.PendingEmbyDays = &days
		u.EmbyGrantLocked = true
		u.RegistrationSource = registrationSourceInvite
		u.RegistrationCode = "CLAMP-INV"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/emby/register", strings.NewReader(`{"emby_username":"inv-emby","emby_password":"Strong123"}`))
	req = req.WithContext(context.WithValue(req.Context(), principalKey, principal{User: child}))
	rr := httptest.NewRecorder()
	app.handleRegisterEmby(rr, req, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", rr.Code, rr.Body.String())
	}
	updated, _ := app.store().User(child.UID)
	if updated.EmbyID != "inv-emby" {
		t.Fatalf("emby not bound: %#v", updated)
	}
	if updated.ExpiredAt > inviterExpiry {
		t.Fatalf("activation expiry %d exceeds inviter expiry %d", updated.ExpiredAt, inviterExpiry)
	}
}
