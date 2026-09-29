package store

import (
	"context"
	"errors"
	"testing"
)

func TestTelegramIdentityChangesClosePreviousRebindRequests(t *testing.T) {
	changes := map[string]func(*Store, int64) error{
		"admin_unbind": func(st *Store, uid int64) error { _, _, err := st.BeginAdminTelegramRebind(uid, 77); return err },
		"admin_bind":   func(st *Store, uid int64) error { _, _, err := st.BindUserTelegramAtomic(uid, 222, 77); return err },
		"update": func(st *Store, uid int64) error {
			_, err := st.UpdateUser(uid, func(u *User) error { u.TelegramID = 222; return nil })
			return err
		},
		"batch_update": func(st *Store, uid int64) error {
			results, err := st.UpdateUsers([]int64{uid}, func(u *User) error { u.TelegramID = 222; return nil })
			if err != nil {
				return err
			}
			return results[uid]
		},
	}
	for name, change := range changes {
		for _, status := range []string{"pending", "approved"} {
			t.Run(name+"/"+status, func(t *testing.T) {
				st := newJSONStoreForTest(t)
				u := telegramBindingUser(t, st, 111)
				req, err := st.CreateRebindRequest(RebindRequest{UID: u.UID, OldTelegramID: 111})
				if err != nil {
					t.Fatal(err)
				}
				if status == "approved" {
					req = telegramBindingApproveRequest(t, st, req)
				}
				if err := change(st, u.UID); err != nil {
					t.Fatal(err)
				}
				for _, view := range []*Store{st, reopenTestStore(t)} {
					got, _ := view.UserLatestRebindRequest(u.UID)
					want := "revoked"
					if status == "approved" {
						want = "used"
					}
					if got.Status != want || got.AdminNote != req.AdminNote || got.ReviewerUID != req.ReviewerUID || got.ReviewedAt != req.ReviewedAt {
						t.Fatalf("previous request survives identity change: %+v", got)
					}
				}
				if _, _, err := st.BindUserTelegramAtomic(u.UID, 111, 77); err != nil {
					t.Fatal(err)
				}
				if _, err := st.UnbindUserTelegram(u.UID, 111); !errors.Is(err, ErrTelegramRebindApprovalRequired) {
					t.Fatalf("old identity revives permission: %v", err)
				}
				if _, err := st.ReviewRebindRequest(req.ID, 77, "approved", "replay"); !errors.Is(err, ErrConflict) {
					t.Fatalf("closed request reapproved: %v", err)
				}
			})
		}
	}
}

func telegramBindingApproveRequest(t *testing.T, st *Store, req RebindRequest) RebindRequest {
	t.Helper()
	reviewed, err := st.ReviewRebindRequest(req.ID, 77, "approved", "original review")
	if err != nil {
		t.Fatal(err)
	}
	return reviewed
}

func TestTelegramSameIdentityUpdatePreservesUnusedApproval(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	approved := telegramBindingApproval(t, st, u)
	if _, _, err := st.BindUserTelegramAtomicWithUsername(u.UID, 111, "newname", 77); err != nil {
		t.Fatal(err)
	}
	got, _ := st.UserLatestRebindRequest(u.UID)
	if got != approved {
		t.Fatalf("same identity update consumed unused approval: %+v", got)
	}
}

func TestTelegramReviewRejectsTerminalAndStaleRequests(t *testing.T) {
	for _, status := range []string{"approved", "used", "rejected", "revoked"} {
		t.Run(status, func(t *testing.T) {
			st := newJSONStoreForTest(t)
			u := telegramBindingUser(t, st, 111)
			req, err := st.CreateRebindRequest(RebindRequest{UID: u.UID, OldTelegramID: 111, Status: status, AdminNote: "original"})
			if err != nil {
				t.Fatal(err)
			}
			for _, action := range []string{"approved", "rejected"} {
				if _, err := st.ReviewRebindRequest(req.ID, 88, action, "replay"); !errors.Is(err, ErrConflict) {
					t.Fatalf("%s -> %s: %v", status, action, err)
				}
			}
			got, _ := st.UserLatestRebindRequest(u.UID)
			if got != req {
				t.Fatalf("failed review changed history: %+v", got)
			}
		})
	}
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	req, err := st.CreateRebindRequest(RebindRequest{UID: u.UID, OldTelegramID: 222})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReviewRebindRequest(req.ID, 77, "approved", "wrong identity"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale request approved: %v", err)
	}
}

func TestTelegramLegacyApprovalConsumedWhenRebindCompletes(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	if _, err := st.UpdateUser(u.UID, func(u *User) error { u.RebindingInProgress = true; u.RebindingSince = 123; return nil }); err != nil {
		t.Fatal(err)
	}
	// Simulate an already in-flight cycle from a previous deployment.
	if _, err := st.CreateRebindRequest(RebindRequest{UID: u.UID, OldTelegramID: 111, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	if _, changed, _, err := st.CompleteUserTelegramRebind(u.UID, 111, 123); err != nil || !changed {
		t.Fatalf("complete: changed=%t err=%v", changed, err)
	}
	if _, err := st.UnbindUserTelegram(u.UID, 111); !errors.Is(err, ErrTelegramRebindApprovalRequired) {
		t.Fatalf("legacy permission survived completion: %v", err)
	}
}

func TestTelegramAdminUnbindFailurePreservesApproval(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	approved := telegramBindingApproval(t, st, u)
	_, err := st.db.ExecContext(context.Background(), `ALTER TABLE twilight_telegram_identity_history ADD CONSTRAINT test_reject_admin_unbind CHECK (change_type <> 'admin_unbind') NOT VALID`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(context.Background(), `ALTER TABLE twilight_telegram_identity_history DROP CONSTRAINT IF EXISTS test_reject_admin_unbind`)
	})
	if _, _, err := st.BeginAdminTelegramRebind(u.UID, 77); err == nil {
		t.Fatal("expected history failure")
	}
	for _, view := range []*Store{st, reopenTestStore(t)} {
		got, _ := view.UserLatestRebindRequest(u.UID)
		current, _ := view.User(u.UID)
		if got != approved || current.TelegramID != 111 {
			t.Fatal("failed unbind partially consumed approval")
		}
	}
}

func TestTelegramStaleReviewerCannotRestoreConsumedApproval(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	req, err := st.CreateRebindRequest(RebindRequest{UID: u.UID, OldTelegramID: 111})
	if err != nil {
		t.Fatal(err)
	}
	// A separate process still sees pending when another process finishes a cycle.
	stale := reopenTestStore(t)
	approved := telegramBindingApproveRequest(t, st, req)
	if _, err := st.UnbindUserTelegram(u.UID, 111); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BindUnboundUserTelegram(u.UID, 111, "same"); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.ReviewRebindRequest(req.ID, 88, "approved", "late review"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale review resurrected approval: %v", err)
	}
	got, _ := reopenTestStore(t).UserLatestRebindRequest(u.UID)
	if got.Status != "used" || got.ReviewerUID != approved.ReviewerUID || got.AdminNote != approved.AdminNote {
		t.Fatalf("late review changed consumed request: %+v", got)
	}
}

func TestTelegramLinkConfirmationClosesLegacyApproval(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 0)
	if _, err := st.CreateRebindRequest(RebindRequest{UID: u.UID, OldTelegramID: 111, Status: "approved"}); err != nil {
		t.Fatal(err)
	}
	_, token, _ := newLinkForTest(t, st, "user", u.UID)
	bot := reopenTestStore(t)
	if _, _, bound, err := bot.ConfirmTelegramLink(context.Background(), token, 111, "same"); err != nil || !bound {
		t.Fatalf("confirm: bound=%t err=%v", bound, err)
	}
	if _, err := st.UnbindUserTelegram(u.UID, 111); !errors.Is(err, ErrTelegramRebindApprovalRequired) {
		t.Fatalf("confirmed link left old approval usable: %v", err)
	}
}
