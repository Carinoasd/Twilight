package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"
)

func randomHexForTest(t *testing.T, size int) string {
	t.Helper()
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

// newLinkForTest 签发一条链接并返回 (链接, start token, browser secret)。
func newLinkForTest(t *testing.T, st *Store, scene string, uid int64) (TelegramLink, string, string) {
	t.Helper()
	now := time.Now().Unix()
	token := randomHexForTest(t, 16)
	secret := ""
	l := TelegramLink{ID: randomHexForTest(t, 16), StartHash: TelegramLinkStartHash(token), Scene: scene, UID: uid, CreatedAt: now, ExpiresAt: now + 300}
	if scene == "register" {
		secret = randomHexForTest(t, 32)
		l.SecretHash = TelegramLinkSecretHash(secret)
	}
	if err := st.CreateTelegramLink(context.Background(), l); err != nil {
		t.Fatalf("create %s link: %v", scene, err)
	}
	return l, token, secret
}

func TestTelegramLinkRegistrationAcrossProcesses(t *testing.T) {
	api := newJSONStoreForTest(t)
	bot := reopenTestStore(t)
	ctx := context.Background()
	l, token, secret := newLinkForTest(t, api, "register", 0)

	// 资源 ID 不是 Bot 凭据。
	if _, _, _, err := bot.ConfirmTelegramLink(ctx, l.ID, 321, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resource id confirmed: %v", err)
	}
	confirmed, _, bound, err := bot.ConfirmTelegramLink(ctx, token, 321, "@alice")
	if err != nil || bound || confirmed.State != "confirmed" || confirmed.TelegramID != 321 || confirmed.TelegramUsername != "alice" {
		t.Fatalf("bot confirm: %+v %v bound=%v", confirmed, err, bound)
	}
	// 另一个 Telegram 不能覆盖；同一个 Telegram 重放幂等。
	if _, _, _, err := bot.ConfirmTelegramLink(ctx, token, 322, "mallory"); !errors.Is(err, ErrConflict) {
		t.Fatalf("identity replay: %v", err)
	}
	if again, _, _, err := bot.ConfirmTelegramLink(ctx, token, 321, "alice"); err != nil || again.State != "confirmed" {
		t.Fatalf("idempotent replay: %+v %v", again, err)
	}
	// API 进程从数据库看到确认结果。
	got, err := api.TelegramLink(ctx, l.ID)
	if err != nil || !got.Confirmed() || got.TelegramID != 321 || got.CurrentTelegramID != nil {
		t.Fatalf("api projection: %+v %v", got, err)
	}
	if !got.OwnedBy(0, TelegramLinkSecretHash(secret)) || got.OwnedBy(0, TelegramLinkSecretHash("other")) || got.OwnedBy(1, "") {
		t.Fatal("ownership must depend on the browser secret only")
	}
	// 错误的 secret 不能消费。
	if _, _, _, err := api.RegisterWithTelegramLink(ctx, User{Username: "alice"}, "", l.ID, TelegramLinkSecretHash("other"), nil); !errors.Is(err, ErrTelegramLinkOwner) {
		t.Fatalf("foreign browser consumed link: %v", err)
	}
	// 账号创建失败保留 confirmed，可重试。
	if _, _, bind, err := api.RegisterWithTelegramLink(ctx, User{Username: "alice"}, "", l.ID, TelegramLinkSecretHash(secret), func(*User, RegCode, BindCode) error { return ErrConflict }); !errors.Is(err, ErrConflict) || bind.TelegramID != 321 {
		t.Fatalf("failed registration: %v %+v", err, bind)
	}
	if got, _ := api.TelegramLink(ctx, l.ID); got.State != "confirmed" || api.UserCount() != 0 {
		t.Fatalf("failed registration consumed link: %+v users=%d", got, api.UserCount())
	}
	created, _, _, err := api.RegisterWithTelegramLink(ctx, User{Username: "alice"}, "", l.ID, TelegramLinkSecretHash(secret), nil)
	if err != nil || created.TelegramID != 321 || created.TelegramUsername != "alice" || !created.Active {
		t.Fatalf("register: %+v %v", created, err)
	}
	if got, _ := api.TelegramLink(ctx, l.ID); got.State != "consumed" || got.ResultUID != created.UID {
		t.Fatalf("link not consumed: %+v", got)
	}
	if _, _, _, err := api.RegisterWithTelegramLink(ctx, User{Username: "alice2"}, "", l.ID, TelegramLinkSecretHash(secret), nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("consumed link reused: %v", err)
	}
	history, err := bot.GetTelegramIdentityHistory(ctx, created.UID, 10)
	if err != nil || len(history) != 1 || history[0].ChangeType != "register" {
		t.Fatalf("identity history: %+v %v", history, err)
	}
	// 已 consumed 的注册链接：同一 Telegram 重放仍然幂等，因为账号现状一致。
	if _, _, _, err := bot.ConfirmTelegramLink(ctx, token, 321, "alice"); err != nil {
		t.Fatalf("replay after consume: %v", err)
	}
}

func TestTelegramLinkUserSceneBindsImmediately(t *testing.T) {
	api := newJSONStoreForTest(t)
	bot := reopenTestStore(t)
	ctx := context.Background()
	u, err := api.CreateUser(User{Username: "member", Role: RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	old, oldToken, _ := newLinkForTest(t, api, "user", u.UID)
	l, token, _ := newLinkForTest(t, api, "user", u.UID)
	if got, _ := api.TelegramLink(ctx, old.ID); got.State != "cancelled" {
		t.Fatalf("reissue did not cancel previous link: %+v", got)
	}
	if _, _, _, err := bot.ConfirmTelegramLink(ctx, oldToken, 72, "old"); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancelled link confirmed: %v", err)
	}
	confirmed, user, bound, err := bot.ConfirmTelegramLink(ctx, token, 72, "new")
	if err != nil || !bound || confirmed.State != "consumed" || user.TelegramID != 72 || confirmed.ResultUID != u.UID {
		t.Fatalf("user bind: %+v %+v bound=%v err=%v", confirmed, user, bound, err)
	}
	got, err := api.TelegramLink(ctx, l.ID)
	if err != nil || got.CurrentTelegramID == nil || *got.CurrentTelegramID != 72 || !got.OwnedBy(u.UID, "") || got.OwnedBy(u.UID+1, "") {
		t.Fatalf("projection: %+v %v", got, err)
	}
	if _, _, bound, err := bot.ConfirmTelegramLink(ctx, token, 72, "new"); err != nil || bound {
		t.Fatalf("replay not idempotent: bound=%v %v", bound, err)
	}
	// 已绑定账号不能再签发普通链接。
	if err := api.CreateTelegramLink(ctx, TelegramLink{ID: randomHexForTest(t, 16), StartHash: TelegramLinkStartHash("x"), Scene: "user", UID: u.UID, CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 60}); !errors.Is(err, ErrTelegramAlreadyBound) {
		t.Fatalf("bound account issued link: %v", err)
	}
	history, _ := api.GetTelegramIdentityHistory(ctx, u.UID, 10)
	if len(history) != 1 || history[0].ChangeType != "bind" {
		t.Fatalf("history: %+v", history)
	}
	if _, err := api.UnbindUserTelegram(u.UID, 72); err != nil {
		t.Fatal(err)
	}
	if _, err := api.TelegramLink(ctx, l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbind kept link: %v", err)
	}
}

func TestTelegramLinkRejectsTakenIdentityAndStaleRebind(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	if _, err := st.CreateUser(User{Username: "owner", Role: RoleAdmin, TelegramID: 500}); err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(User{Username: "second", Role: RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	_, token, _ := newLinkForTest(t, st, "register", 0)
	if _, _, _, err := st.ConfirmTelegramLink(ctx, token, 500, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("taken identity accepted for registration: %v", err)
	}
	_, userToken, _ := newLinkForTest(t, st, "user", u.UID)
	if _, _, _, err := st.ConfirmTelegramLink(ctx, userToken, 500, "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("taken identity accepted for account: %v", err)
	}
	// 链接签发后账号进入了新的换绑周期：旧链接不能再绑定。
	if _, err := st.UpdateUser(u.UID, func(u *User) error { u.RebindingInProgress, u.RebindingSince = true, 123; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.ConfirmTelegramLink(ctx, userToken, 501, "fresh"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale rebind cycle accepted: %v", err)
	}
}

func TestTelegramLinkFailureRecordCannotOverwriteConfirmation(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	l, token, _ := newLinkForTest(t, st, "register", 0)
	if err := st.SetTelegramLinkFailure(ctx, l.ID, "TG_BIND_GROUP_MEMBERSHIP_REQUIRED", "join first", true); err != nil {
		t.Fatal(err)
	}
	got, _ := st.TelegramLink(ctx, l.ID)
	if got.State != "pending" || got.ErrorCode == "" || !got.Retryable {
		t.Fatalf("failure not recorded: %+v", got)
	}
	if _, _, _, err := st.ConfirmTelegramLink(ctx, token, 9, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetTelegramLinkFailure(ctx, l.ID, "LATE", "late failure", true); err != nil {
		t.Fatal(err)
	}
	got, _ = st.TelegramLink(ctx, l.ID)
	if got.State != "confirmed" || got.ErrorCode != "" {
		t.Fatalf("late failure overwrote confirmation: %+v", got)
	}
}

func TestTelegramLinkValidationCapacityAndExpiry(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	now := time.Now().Unix()
	base := TelegramLink{ID: randomHexForTest(t, 16), StartHash: TelegramLinkStartHash("t"), Scene: "register", SecretHash: TelegramLinkSecretHash("s"), CreatedAt: now, ExpiresAt: now + 60}
	for name, mutate := range map[string]func(*TelegramLink){
		"short id":      func(l *TelegramLink) { l.ID = "abc" },
		"bad scene":     func(l *TelegramLink) { l.Scene = "admin" },
		"no secret":     func(l *TelegramLink) { l.SecretHash = "" },
		"user w/secret": func(l *TelegramLink) { l.Scene, l.UID = "user", 1 },
		"too long ttl":  func(l *TelegramLink) { l.ExpiresAt = now + int64(TelegramLinkTTL/time.Second) + 1 },
		"expired":       func(l *TelegramLink) { l.ExpiresAt = now - 1 },
		"pre-confirmed": func(l *TelegramLink) { l.TelegramID = 5 },
	} {
		l := base
		mutate(&l)
		if err := st.CreateTelegramLink(ctx, l); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: accepted (%v)", name, err)
		}
	}
	if err := st.CreateTelegramLink(ctx, TelegramLink{ID: randomHexForTest(t, 16), StartHash: TelegramLinkStartHash("t"), Scene: "user", UID: 99, CreatedAt: now, ExpiresAt: now + 60}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing account: %v", err)
	}
	l, token, _ := newLinkForTest(t, st, "register", 0)
	// 同一 token 不能重复签发。
	if err := st.CreateTelegramLink(ctx, TelegramLink{ID: randomHexForTest(t, 16), StartHash: TelegramLinkStartHash(token), Scene: "register", SecretHash: TelegramLinkSecretHash("x"), CreatedAt: now, ExpiresAt: now + 60}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate token: %v", err)
	}
	// 过期链接：读取仍可见（由调用方判定），确认被拒，清理会删除。
	if _, err := st.db.ExecContext(ctx, `UPDATE twilight_telegram_links SET expires_at=$2 WHERE id=$1`, l.ID, now-1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.ConfirmTelegramLink(ctx, token, 1, "late"); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired confirmed: %v", err)
	}
	if n, err := st.CleanupTelegramLinks(ctx, now, 0, 0); err != nil || n != 1 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	if _, err := st.TelegramLink(ctx, l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired link survived cleanup: %v", err)
	}
	// 容量：塞满后拒绝，过期行会先被回收。
	for i := 0; i < 3; i++ {
		newLinkForTest(t, st, "register", 0)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO twilight_telegram_links (id,start_hash,secret_hash,scene,uid,state,created_at,expires_at)
 SELECT md5(g::text), md5('s'||g::text), md5('p'||g::text), 'register', 0, 'pending', $1, $2 FROM generate_series(1, $3) g`, now, now+60, TelegramLinkLimit); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTelegramLink(ctx, TelegramLink{ID: randomHexForTest(t, 16), StartHash: TelegramLinkStartHash("full"), Scene: "register", SecretHash: TelegramLinkSecretHash("f"), CreatedAt: now, ExpiresAt: now + 60}); !errors.Is(err, ErrTelegramLinkCapacity) {
		t.Fatalf("capacity: %v", err)
	}
}

func TestTelegramLinkCleanupFollowsAccountLifecycle(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	u, err := st.CreateUser(User{Username: "lifecycle", Role: RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	userLink, _, _ := newLinkForTest(t, st, "user", u.UID)
	regLink, token, _ := newLinkForTest(t, st, "register", 0)
	if _, _, _, err := st.ConfirmTelegramLink(ctx, token, 8080, "reg"); err != nil {
		t.Fatal(err)
	}
	keep, _, _ := newLinkForTest(t, st, "register", 0)
	// 账号从主状态消失（例如旧快照被另一进程直接写回）→ 孤儿链接被清理，注册链接保留。
	if _, err := st.db.ExecContext(ctx, `UPDATE twilight_state SET state = state #- ARRAY['users', $1::text] WHERE id=1`, strconv.FormatInt(u.UID, 10)); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CleanupOrphanedTelegramLinks(ctx); err != nil || n != 1 {
		t.Fatalf("orphan cleanup: %d %v", n, err)
	}
	if _, err := st.TelegramLink(ctx, userLink.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("orphan link survived")
	}
	if _, err := st.TelegramLink(ctx, keep.ID); err != nil {
		t.Fatal("register link removed by orphan cleanup")
	}
	// 按 Telegram 身份清理。
	if n, err := st.CleanupTelegramLinks(ctx, 0, 0, 8080); err != nil || n != 1 {
		t.Fatalf("identity cleanup: %d %v", n, err)
	}
	if _, err := st.TelegramLink(ctx, regLink.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("identity link survived")
	}
	// 恢复快照使所有链接失效。
	if err := st.LoadSnapshot([]byte(`{"users":{}}`)); err != nil {
		t.Fatal(err)
	}
	if n, err := st.TelegramLinkCount(ctx); err != nil || n != 0 {
		t.Fatalf("restore kept links: %d %v", n, err)
	}
}

func TestTelegramLinkDatabaseFailureIsNotMissing(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TelegramLink(ctx, randomHexForTest(t, 16)); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("closed database reported as missing link: %v", err)
	}
}
