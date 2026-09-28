package store

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func telegramBindingUser(t *testing.T, st *Store, telegramID int64) User {
	t.Helper()
	u, err := st.CreateUser(User{Username: "binding-user", Role: RoleNormal, TelegramID: telegramID, TelegramUsername: "old-name"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func telegramBindingApproval(t *testing.T, st *Store, u User) RebindRequest {
	t.Helper()
	req, err := st.CreateRebindRequest(RebindRequest{UID: u.UID, OldTelegramID: u.TelegramID})
	if err != nil {
		t.Fatal(err)
	}
	req, err = st.ReviewRebindRequest(req.ID, 77, "approved", "verified manually")
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestTelegramBindingRejectsSelfServiceOverwrite(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	telegramBindingApproval(t, st, u)
	if _, err := st.BindUnboundUserTelegram(u.UID, 222, "new-name"); !errors.Is(err, ErrTelegramAlreadyBound) {
		t.Fatalf("err=%v", err)
	}
	got, _ := st.User(u.UID)
	if got.TelegramID != 111 || got.TelegramUsername != "old-name" {
		t.Fatalf("overwrote identity: %#v", got)
	}
	// The explicit administrator operation keeps its override semantics.
	if _, _, err := st.BindUserTelegramAtomic(u.UID, 222, 77); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramUnbindConsumesApprovalAtomically(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	if _, err := st.UnbindUserTelegram(u.UID, 111); !errors.Is(err, ErrTelegramRebindApprovalRequired) {
		t.Fatalf("err=%v", err)
	}
	approved := telegramBindingApproval(t, st, u)
	updated, err := st.UnbindUserTelegram(u.UID, 111)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TelegramID != 0 || updated.TelegramUsername != "" || !updated.RebindingInProgress || updated.RebindingSince == 0 {
		t.Fatalf("partial unbind: %#v", updated)
	}
	used, _ := st.UserLatestRebindRequest(u.UID)
	if used.Status != "used" || used.ReviewerUID != approved.ReviewerUID || used.ReviewedAt != approved.ReviewedAt || used.AdminNote != approved.AdminNote {
		t.Fatalf("lost approval metadata: %#v", used)
	}
	history, err := st.GetTelegramIdentityHistory(context.Background(), u.UID, 10)
	if err != nil || len(history) != 1 || history[0].ChangeType != "unbind" || history[0].TelegramID != 111 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
	if _, err := st.BindUnboundUserTelegram(u.UID, 222, "@new-name"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UnbindUserTelegram(u.UID, 222); !errors.Is(err, ErrTelegramRebindApprovalRequired) {
		t.Fatalf("reused approval: %v", err)
	}
}

func TestTelegramUnbindRejectsStaleIdentityOrApproval(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	approved := telegramBindingApproval(t, st, u)
	other := reopenTestStore(t)
	if _, err := other.ReviewRebindRequest(approved.ID, 77, "revoked", "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UnbindUserTelegram(u.UID, 111); !errors.Is(err, ErrTelegramRebindApprovalRequired) {
		t.Fatalf("stale approval accepted: %v", err)
	}
	if _, err := other.ReviewRebindRequest(approved.ID, 77, "approved", "approved"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.UpdateUser(u.UID, func(u *User) error { u.TelegramID = 222; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UnbindUserTelegram(u.UID, 111); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale identity accepted: %v", err)
	}
	if _, err := st.UnbindUserTelegram(u.UID, 222); !errors.Is(err, ErrTelegramRebindApprovalRequired) {
		t.Fatalf("approval for other identity accepted: %v", err)
	}
}

func TestTelegramBindingCompetingStoresHaveOneWinner(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 0)
	other := reopenTestStore(t)
	start := make(chan struct{})
	errorsOut := make(chan error, 2)
	var wg sync.WaitGroup
	for index, s := range []*Store{st, other} {
		wg.Add(1)
		go func(s *Store, id int64) {
			defer wg.Done()
			<-start
			_, err := s.BindUnboundUserTelegram(u.UID, id, "name")
			errorsOut <- err
		}(s, int64(111+index))
	}
	close(start)
	wg.Wait()
	close(errorsOut)
	successes := 0
	for err := range errorsOut {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrTelegramAlreadyBound) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successes=%d", successes)
	}
	history, err := st.GetTelegramIdentityHistory(context.Background(), u.UID, 10)
	if err != nil || len(history) != 1 {
		t.Fatalf("history=%#v err=%v", history, err)
	}
}

func TestTelegramUnbindHistoryFailureRollsBackAllState(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	approved := telegramBindingApproval(t, st, u)
	ctx := context.Background()
	// Force the dedicated-table write to fail after the guarded state UPDATE.
	_, err := st.db.ExecContext(ctx, `ALTER TABLE twilight_telegram_identity_history ADD CONSTRAINT test_reject_unbind CHECK (change_type <> 'unbind') NOT VALID`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(ctx, `ALTER TABLE twilight_telegram_identity_history DROP CONSTRAINT IF EXISTS test_reject_unbind`)
	})
	if _, err := st.UnbindUserTelegram(u.UID, 111); err == nil {
		t.Fatal("expected history persistence failure")
	}
	for _, s := range []*Store{st, reopenTestStore(t)} {
		got, _ := s.User(u.UID)
		request, _ := s.UserLatestRebindRequest(u.UID)
		if got.TelegramID != 111 || got.RebindingInProgress || request.Status != "approved" || request.ID != approved.ID {
			t.Fatalf("partial commit: user=%#v request=%#v", got, request)
		}
		if indexed, ok := s.FindUserByTelegramID(111); !ok || indexed.UID != u.UID {
			t.Fatal("identity index not restored")
		}
	}
}

func TestTelegramRebindCompletionRejectsStaleChecks(t *testing.T) {
	st := newJSONStoreForTest(t)
	u := telegramBindingUser(t, st, 111)
	if _, err := st.UpdateUser(u.UID, func(u *User) error {
		u.RebindingInProgress, u.RebindingSince = true, 100
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	other := reopenTestStore(t)
	if _, err := other.UpdateUser(u.UID, func(u *User) error {
		u.TelegramID, u.RebindingSince = 222, 200
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range [][2]int64{{111, 100}, {222, 100}} {
		if _, _, _, err := st.CompleteUserTelegramRebind(u.UID, expected[0], expected[1]); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale completion accepted: %v", err)
		}
	}
	updated, changed, _, err := st.CompleteUserTelegramRebind(u.UID, 222, 200)
	if err != nil || !changed || updated.RebindingInProgress || updated.RebindingSince != 0 {
		t.Fatalf("completion: changed=%v err=%v user=%#v", changed, err, updated)
	}
	if _, changed, _, err := other.CompleteUserTelegramRebind(u.UID, 222, 200); err != nil || changed {
		t.Fatalf("duplicate completion: changed=%v err=%v", changed, err)
	}
	if _, err := st.UpdateUser(u.UID, func(u *User) error { u.Active = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := other.CompleteUserTelegramRebind(u.UID, 222, 200); !errors.Is(err, ErrInvalid) {
		t.Fatalf("inactive user accepted: %v", err)
	}
}
