package store

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newLoginForTest(t *testing.T, s *Store) (TelegramLogin, string, string) {
	t.Helper()
	token, secret := randomHexForTest(t, 16), randomHexForTest(t, 32)
	l := TelegramLogin{ID: randomHexForTest(t, 16), StartHash: TelegramLoginHash("start", token), SecretHash: TelegramLoginHash("secret", secret), DeviceID: "device", UserAgent: "Firefox", Site: "Test", CheckCode: "ABC123", ExpiresAt: time.Now().Add(TelegramLoginTTL).Unix()}
	if err := s.CreateTelegramLogin(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return l, token, secret
}

func TestTelegramLoginAcrossProcessesAndSingleConsumption(t *testing.T) {
	api := newJSONStoreForTest(t)
	bot := reopenTestStore(t)
	ctx := context.Background()
	u, err := api.CreateUser(User{Username: "qr-user", Active: true, TelegramID: 789})
	if err != nil {
		t.Fatal(err)
	}
	l, token, secret := newLoginForTest(t, api)
	if _, err = bot.ScanTelegramLogin(ctx, l.ID, 789); err == nil {
		t.Fatal("observation ID used as token")
	}
	if _, err = api.TelegramLogin(ctx, l.ID, TelegramLoginHash("secret", "wrong")); err == nil {
		t.Fatal("foreign browser read")
	}
	if _, err = bot.ScanTelegramLogin(ctx, token, 789); err != nil {
		t.Fatal(err)
	}
	if _, err = bot.ScanTelegramLogin(ctx, token, 790); err == nil {
		t.Fatal("identity overwritten")
	}
	if err = bot.DecideTelegramLogin(ctx, l.ID, 790, true); err == nil {
		t.Fatal("foreign confirmation")
	}
	if _, err = api.ConsumeTelegramLogin(ctx, l.ID, TelegramLoginHash("secret", secret), "device"); err == nil {
		t.Fatal("scan alone granted login")
	}
	if err = bot.DecideTelegramLogin(ctx, l.ID, 789, true); err != nil {
		t.Fatal(err)
	}
	if _, err = api.ConsumeTelegramLogin(ctx, l.ID, TelegramLoginHash("secret", secret), "other-device"); err == nil {
		t.Fatal("device mismatch")
	}
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			user, e := s.ConsumeTelegramLogin(ctx, l.ID, TelegramLoginHash("secret", secret), "device")
			if e == nil {
				if user.UID != u.UID {
					t.Error("wrong account")
				}
				won.Add(1)
			}
		}([]*Store{api, bot}[i%2])
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("successful redemptions=%d", won.Load())
	}
	if err = bot.DecideTelegramLogin(ctx, l.ID, 789, true); err == nil {
		t.Fatal("re-approved used request")
	}
}

func TestTelegramLoginInvalidationCannotBeReversed(t *testing.T) {
	changes := []struct {
		name   string
		change func(*User)
	}{
		{"password", func(u *User) { u.PasswordHash = "changed" }},
		{"disable", func(u *User) { u.Active = false }},
		{"unbind", func(u *User) { u.TelegramID = 0 }},
		{"role", func(u *User) { u.Role = 2 }},
		{"rebind", func(u *User) { u.RebindingInProgress = true }},
	}
	for _, tt := range changes {
		t.Run(tt.name, func(t *testing.T) {
			s := newJSONStoreForTest(t)
			bot := reopenTestStore(t)
			ctx := context.Background()
			u, err := s.CreateUser(User{Username: "qr-user", Active: true, TelegramID: 789, Role: 1})
			if err != nil {
				t.Fatal(err)
			}
			l, token, secret := newLoginForTest(t, s)
			if _, err = bot.ScanTelegramLogin(ctx, token, 789); err != nil {
				t.Fatal(err)
			}
			if err = bot.DecideTelegramLogin(ctx, l.ID, 789, true); err != nil {
				t.Fatal(err)
			}
			if _, err = s.UpdateUser(u.UID, func(v *User) error { tt.change(v); return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err = s.UpdateUser(u.UID, func(v *User) error { *v = u; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err = bot.ConsumeTelegramLogin(ctx, l.ID, TelegramLoginHash("secret", secret), "device"); err == nil {
				t.Fatal("restoring identity revived approval")
			}
			got, err := s.TelegramLogin(ctx, l.ID, TelegramLoginHash("secret", secret))
			if err != nil || got.State != "cancelled" {
				t.Fatalf("state=%s err=%v", got.State, err)
			}
		})
	}
}

func TestTelegramLoginExpiryCancelRejectAndRestore(t *testing.T) {
	for _, action := range []string{"expire", "cancel", "reject", "restore", "delete"} {
		t.Run(action, func(t *testing.T) {
			s := newJSONStoreForTest(t)
			ctx := context.Background()
			u, err := s.CreateUser(User{Username: "qr-user", Active: true, TelegramID: 789})
			if err != nil {
				t.Fatal(err)
			}
			l, token, secret := newLoginForTest(t, s)
			hash := TelegramLoginHash("secret", secret)
			if _, err = s.ScanTelegramLogin(ctx, token, 789); err != nil {
				t.Fatal(err)
			}
			if action == "reject" {
				err = s.DecideTelegramLogin(ctx, l.ID, 789, false)
			} else {
				err = s.DecideTelegramLogin(ctx, l.ID, 789, true)
			}
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "expire":
				_, err = s.db.Exec(`UPDATE twilight_telegram_logins SET expires_at=$1 WHERE id=$2`, time.Now().Unix()-1, l.ID)
			case "cancel":
				err = s.CancelTelegramLogin(ctx, l.ID, hash)
			case "restore":
				data, e := s.Snapshot()
				if e != nil {
					t.Fatal(e)
				}
				err = s.LoadSnapshot(data)
			case "delete":
				err = s.DeleteUser(u.UID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ConsumeTelegramLogin(ctx, l.ID, hash, "device"); err == nil {
				t.Fatalf("%s still redeemable", action)
			}
		})
	}
}

func TestTelegramLoginRollbackAndCapacity(t *testing.T) {
	s := newJSONStoreForTest(t)
	ctx := context.Background()
	_, err := s.CreateUser(User{Username: "qr-user", Active: true, TelegramID: 789})
	if err != nil {
		t.Fatal(err)
	}
	l, token, secret := newLoginForTest(t, s)
	if _, err = s.ScanTelegramLogin(ctx, token, 789); err != nil {
		t.Fatal(err)
	}
	if err = s.DecideTelegramLogin(ctx, l.ID, 789, true); err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`ALTER TABLE twilight_telegram_logins ADD CONSTRAINT test_reject_consumption CHECK (state<>'consumed')`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = s.db.Exec(`ALTER TABLE twilight_telegram_logins DROP CONSTRAINT IF EXISTS test_reject_consumption`)
	})
	if _, err = s.ConsumeTelegramLogin(ctx, l.ID, TelegramLoginHash("secret", secret), "device"); err == nil {
		t.Fatal("constraint did not reject")
	}
	_, err = s.db.Exec(`ALTER TABLE twilight_telegram_logins DROP CONSTRAINT test_reject_consumption`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.TelegramLogin(ctx, l.ID, TelegramLoginHash("secret", secret))
	if err != nil || got.State != "approved" {
		t.Fatalf("rollback state=%s %v", got.State, err)
	}
	_, err = s.db.Exec(`INSERT INTO twilight_telegram_logins (`+telegramLoginColumns+`) SELECT 'id'||n,'start'||n,'secret','pending','','','','',0,0,$1 FROM generate_series(1,$2) n`, time.Now().Add(TelegramLoginTTL).Unix(), TelegramLoginLimit-1)
	if err != nil {
		t.Fatal(err)
	}
	err = s.CreateTelegramLogin(ctx, TelegramLogin{ID: "overflow", StartHash: "overflow", SecretHash: "secret", ExpiresAt: time.Now().Add(TelegramLoginTTL).Unix()})
	if err != ErrTelegramLinkCapacity {
		t.Fatal(fmt.Sprintf("capacity: %v", err))
	}
}

func TestTelegramLoginDeviceBlockAndLogoutRevoke(t *testing.T) {
	for _, kind := range []string{"device", "logout"} {
		t.Run(kind, func(t *testing.T) {
			s := newJSONStoreForTest(t)
			ctx := context.Background()
			u, err := s.CreateUser(User{Username: "qr-user", Active: true, TelegramID: 789})
			if err != nil {
				t.Fatal(err)
			}
			l, token, secret := newLoginForTest(t, s)
			if _, err = s.ScanTelegramLogin(ctx, token, 789); err != nil {
				t.Fatal(err)
			}
			if err = s.DecideTelegramLogin(ctx, l.ID, 789, true); err != nil {
				t.Fatal(err)
			}
			if kind == "device" {
				if err = s.UpsertDevice(Device{UID: u.UID, DeviceID: "device", Blocked: true}); err != nil {
					t.Fatal(err)
				}
				if err = s.UpdateDevice(u.UID, "device", func(d *Device) { d.Blocked = false }); err != nil {
					t.Fatal(err)
				}
			} else if err = s.RevokeTelegramLogins(ctx, u.UID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.ConsumeTelegramLogin(ctx, l.ID, TelegramLoginHash("secret", secret), "device"); err == nil {
				t.Fatal("revoked request usable")
			}
		})
	}
}
