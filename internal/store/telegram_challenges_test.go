package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/migration"
)

func newChallengeForTest(t *testing.T, st *Store, seed string, uid int64) (TelegramChallenge, string) {
	t.Helper()
	token := strings.ToUpper(TelegramChallengeHash(seed)[:32])
	c := TelegramChallenge{ID: strings.ToUpper(TelegramChallengeHash("id:" + seed)[:32]), TokenHash: TelegramChallengeHash(token), OwnerHash: TelegramBrowserHash("browser:" + seed), BindCode: BindCode{Scene: "register", CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 300}}
	if uid > 0 {
		c.UID, c.Scene, c.OwnerHash = uid, "user", ""
	}
	if err := st.CreateTelegramChallenge(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c, token
}

func TestTelegramChallengeSeparateStoreConfirmationAndRegistration(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	c, token := newChallengeForTest(t, st, "register", 0)
	bot := reopenTestStore(t)
	if _, _, _, err := bot.ConfirmTelegramChallenge(ctx, c.ID, 42, "test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("observation ID accepted as token: %v", err)
	}
	if _, _, _, err := bot.ConfirmTelegramChallenge(ctx, token, 42, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := bot.ConfirmTelegramChallenge(ctx, token, 43, "attacker"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-identity replay: %v", err)
	}
	if _, _, _, err := st.RegisterWithTelegramChallenge(ctx, User{Username: "alice"}, "", c.ID, "wrong-browser", nil); !errors.Is(err, ErrTelegramChallengeOwner) {
		t.Fatalf("owner: %v", err)
	}
	if _, _, _, err := st.RegisterWithTelegramChallenge(ctx, User{Username: "alice"}, "", c.ID, c.OwnerHash, func(*User, RegCode, BindCode) error { return ErrConflict }); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	got, err := st.TelegramChallenge(ctx, c.ID)
	if err != nil || got.State != "verified" || st.UserCount() != 0 {
		t.Fatalf("failed creation consumed challenge: %s %v", got.State, err)
	}
	u, _, _, err := st.RegisterWithTelegramChallenge(ctx, User{Username: "alice"}, "", c.ID, c.OwnerHash, nil)
	if err != nil || u.TelegramID != 42 {
		t.Fatalf("register: %v", err)
	}
	if _, _, _, err := bot.RegisterWithTelegramChallenge(ctx, User{Username: "bob"}, "", c.ID, c.OwnerHash, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("consumed twice: %v", err)
	}
	got, err = bot.TelegramChallenge(ctx, c.ID)
	if err != nil || got.State != "consumed" || got.ResultUID != u.UID {
		t.Fatalf("result: %+v %v", got, err)
	}
	for _, kind := range []string{"state", "export"} {
		var raw string
		if kind == "state" {
			data, e := st.Snapshot()
			if e != nil {
				t.Fatal(e)
			}
			raw = string(data)
		} else {
			files, e := st.ExportMigrationFiles(ctx)
			if e != nil {
				t.Fatal(e)
			}
			for _, f := range files {
				raw += string(f.Data)
			}
		}
		if strings.Contains(raw, token) || strings.Contains(raw, c.TokenHash) || strings.Contains(raw, c.OwnerHash) {
			t.Fatalf("credential leaked to %s", kind)
		}
	}
}

func TestTelegramChallengeConcurrentSingleConsumer(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	c, token := newChallengeForTest(t, st, "concurrent", 0)
	if _, _, _, err := st.ConfirmTelegramChallenge(ctx, token, 81, "test"); err != nil {
		t.Fatal(err)
	}
	other := reopenTestStore(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, repo := range []*Store{st, other} {
		wg.Add(1)
		go func(i int, repo *Store) {
			defer wg.Done()
			_, _, _, err := repo.RegisterWithTelegramChallenge(ctx, User{Username: fmt.Sprintf("user%d", i)}, "", c.ID, c.OwnerHash, nil)
			results <- err
		}(i, repo)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("commits=%d", success)
	}
	if err := st.Refresh(); err != nil {
		t.Fatal(err)
	}
	if st.UserCount() != 1 {
		t.Fatalf("users=%d", st.UserCount())
	}
}

func TestTelegramChallengeRegistrationHistoryFailureRollsBackGrant(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	c, token := newChallengeForTest(t, st, "rollback", 0)
	if _, _, _, err := st.ConfirmTelegramChallenge(ctx, token, 93, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRegCode(RegCode{Code: "REGTEST", Type: 1, Days: 7, UseCountLimit: 1, Active: true}); err != nil {
		t.Fatal(err)
	}
	_, err := st.db.Exec(`CREATE FUNCTION fail_challenge_history() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected history failure'; END $$; CREATE TRIGGER fail_challenge_history BEFORE INSERT ON twilight_telegram_identity_history FOR EACH ROW EXECUTE FUNCTION fail_challenge_history()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = st.db.Exec(`DROP TRIGGER IF EXISTS fail_challenge_history ON twilight_telegram_identity_history; DROP FUNCTION IF EXISTS fail_challenge_history()`)
	})
	if _, _, _, err := st.RegisterWithTelegramChallenge(ctx, User{Username: "rollback"}, "REGTEST", c.ID, c.OwnerHash, nil); err == nil || !strings.Contains(err.Error(), "injected history failure") {
		t.Fatalf("history injection not reached: %v", err)
	}
	other := reopenTestStore(t)
	got, err := other.TelegramChallenge(ctx, c.ID)
	if err != nil || got.State != "verified" || other.UserCount() != 0 || st.UserCount() != 0 {
		t.Fatalf("partial registration: %+v %v", got, err)
	}
	reg, _ := other.RegCode("REGTEST")
	if reg.UseCount != 0 {
		t.Fatalf("grant consumed: %+v", reg)
	}
}

func TestTelegramChallengeDistinctChallengesCompeteForOneRegistrationCode(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	if err := st.UpsertRegCode(RegCode{Code: "SINGLE", Type: 1, Days: 7, UseCountLimit: 1, Active: true}); err != nil {
		t.Fatal(err)
	}
	var challenges []TelegramChallenge
	for i := 0; i < 2; i++ {
		c, token := newChallengeForTest(t, st, fmt.Sprintf("grant%d", i), 0)
		if _, _, _, err := st.ConfirmTelegramChallenge(ctx, token, int64(800+i), "user"); err != nil {
			t.Fatal(err)
		}
		challenges = append(challenges, c)
	}
	other := reopenTestStore(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, repo := range []*Store{st, other} {
		wg.Add(1)
		go func(i int, repo *Store) {
			defer wg.Done()
			c := challenges[i]
			_, _, _, err := repo.RegisterWithTelegramChallenge(ctx, User{Username: fmt.Sprintf("grant%d", i)}, "SINGLE", c.ID, c.OwnerHash, nil)
			results <- err
		}(i, repo)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if err := st.Refresh(); err != nil {
		t.Fatal(err)
	}
	reg, _ := st.RegCode("SINGLE")
	if success != 1 || st.UserCount() != 1 || reg.UseCount != 1 {
		t.Fatalf("grant double-spent: success=%d users=%d uses=%d", success, st.UserCount(), reg.UseCount)
	}
	verified, consumed := 0, 0
	for _, c := range challenges {
		got, err := st.TelegramChallenge(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == "verified" {
			verified++
		}
		if got.State == "consumed" {
			consumed++
		}
	}
	if verified != 1 || consumed != 1 {
		t.Fatalf("loser lost challenge: verified=%d consumed=%d", verified, consumed)
	}
}

func TestTelegramChallengeRetryFailureCannotOverwriteSuccess(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	c, token := newChallengeForTest(t, st, "retry", 0)
	c, err := st.TelegramChallenge(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.SetTelegramChallengeFailure(ctx, c, "TG_UPSTREAM", "retry", 502, true); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = st.ConfirmTelegramChallenge(ctx, token, 95, "test"); err != nil {
		t.Fatal(err)
	}
	if err = st.SetTelegramChallengeFailure(ctx, c, "TG_UPSTREAM", "late failure", 502, true); err != nil {
		t.Fatal(err)
	}
	got, err := st.TelegramChallenge(ctx, c.ID)
	if err != nil || got.State != "verified" || got.ErrorCode != "" {
		t.Fatalf("late failure replaced success: %+v %v", got, err)
	}
}

func TestTelegramChallengeUserLifecycleAndReplacement(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	u, err := st.CreateUser(User{Username: "lifecycle", Role: RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	old, oldToken := newChallengeForTest(t, st, "old", u.UID)
	c, token := newChallengeForTest(t, st, "new", u.UID)
	if _, _, _, err = st.ConfirmTelegramChallenge(ctx, oldToken, 72, "old"); !errors.Is(err, ErrConflict) {
		t.Fatalf("superseded challenge usable: %v", err)
	}
	other := reopenTestStore(t)
	if _, _, changed, err := other.ConfirmTelegramChallenge(ctx, token, 72, "new"); err != nil || !changed {
		t.Fatalf("bind: %v", err)
	}
	got, err := st.TelegramChallenge(ctx, c.ID)
	if err != nil || got.CurrentTelegramID == nil || *got.CurrentTelegramID != 72 {
		t.Fatalf("stale account projection: %+v %v", got, err)
	}
	if _, _, changed, err := st.ConfirmTelegramChallenge(ctx, token, 72, "new"); err != nil || changed {
		t.Fatalf("retry not idempotent: %v", err)
	}
	history, err := st.GetTelegramIdentityHistory(ctx, u.UID, 10)
	if err != nil || len(history) != 1 {
		t.Fatalf("history duplicated: %v %v", history, err)
	}
	if _, err = st.UnbindUserTelegram(u.UID, 72); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{old.ID, c.ID} {
		if _, err = st.TelegramChallenge(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unbind retained challenge: %v", err)
		}
	}
}

func TestTelegramChallengeRestoreInvalidatesAndDatabaseFailureIsNotMissing(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	c, _ := newChallengeForTest(t, st, "restore", 0)
	data, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = st.LoadSnapshot(data); err != nil {
		t.Fatal(err)
	}
	if _, err = st.TelegramChallenge(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restore retained challenge: %v", err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = st.TelegramChallenge(ctx, c.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("database error masked: %v", err)
	}
}

func TestTelegramChallengeCapacityAndExpiry(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	now := time.Now().Unix()
	_, err := st.db.Exec(`INSERT INTO twilight_telegram_challenges (id,token_hash,scene,state,created_at,expires_at) SELECT 'fixture-'||i,'fixture-token-'||i,'register','pending',$1::bigint,$2::bigint FROM generate_series(1,$3) i`, now, now+300, TelegramChallengeLimit)
	if err != nil {
		t.Fatal(err)
	}
	c := TelegramChallenge{ID: strings.Repeat("A", 32), TokenHash: TelegramChallengeHash(strings.Repeat("B", 32)), OwnerHash: TelegramBrowserHash("owner"), BindCode: BindCode{Scene: "register", CreatedAt: now, ExpiresAt: now + 300}}
	if err = st.CreateTelegramChallenge(ctx, c); !errors.Is(err, ErrTelegramChallengeCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	if _, err = st.db.Exec(`UPDATE twilight_telegram_challenges SET expires_at=$1`, now-1); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTelegramChallenge(ctx, c); err != nil {
		t.Fatalf("expired capacity not reclaimed: %v", err)
	}
	if _, err = st.db.Exec(`UPDATE twilight_telegram_challenges SET expires_at=$1`, now-1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = st.ConfirmTelegramChallenge(ctx, strings.Repeat("B", 32), 72, "test"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expiry: %v", err)
	}
}

func TestTelegramChallengeMigrationInvalidatesAndSchemaVersionIsGuarded(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	c, _ := newChallengeForTest(t, st, "migration", 0)
	files, err := st.ExportMigrationFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := migration.Create(migration.Input{TwilightVersion: "test", DatabaseSchemaVersion: "postgres-state-v1", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := migration.Open(data, "", migration.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ImportMigrationArchive(ctx, archive); err != nil {
		t.Fatal(err)
	}
	if _, err = st.TelegramChallenge(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("migration retained challenge: %v", err)
	}
	if err = prepareTelegramChallengeSchema(ctx, st.db); err != nil {
		t.Fatalf("repeated schema prepare: %v", err)
	}
	if _, err = st.db.Exec(`UPDATE twilight_telegram_challenge_schema SET version=999 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.db.Exec(`UPDATE twilight_telegram_challenge_schema SET version=1 WHERE id=1`) })
	if err = prepareTelegramChallengeSchema(ctx, st.db); err == nil {
		t.Fatal("future schema accepted")
	}
}
