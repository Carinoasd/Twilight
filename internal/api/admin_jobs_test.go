package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

func setupTelegramMembershipTest(t *testing.T) *App {
	t.Helper()
	app := newTestApp(t)
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:ABC"
	app.cfg().TelegramRequireMembership = true
	app.cfg().TelegramGroupIDs = []string{"-1001"}
	// newTestApp 直接构造 Config，不经 Load 的默认值；这里显式设成生产默认。
	app.cfg().TelegramMembershipBreakerPercent = 20
	app.cfg().TelegramMembershipBreakerMax = 50
	return app
}

func TestTelegramMembershipPendingRebindSkipsDisable(t *testing.T) {
	app := setupTelegramMembershipTest(t)
	user, err := app.store().CreateUser(store.User{
		Username:   "pending-rebind",
		Role:       store.RoleNormal,
		Active:     true,
		TelegramID: 42001,
		EmbyID:     "emby-pending-rebind",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().CreateRebindRequest(store.RebindRequest{UID: user.UID, Username: user.Username, OldTelegramID: user.TelegramID}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"status":"left","user":{"id":42001,"is_bot":false}}}`))
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL

	summary, logs, err := app.enforceTelegramMembership(context.Background(), false)
	if err != nil {
		t.Fatalf("membership enforcement failed: %v logs=%v", err, logs)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("pending rebind should skip remote membership checks, got %d", got)
	}
	if int(numeric(summary["disabled"])) != 0 || int(numeric(summary["rebind_protected"])) != 1 || int(numeric(summary["rebind_pending"])) != 1 {
		t.Fatalf("unexpected pending-rebind summary: %#v", summary)
	}
	updated, found := app.store().User(user.UID)
	if !found || !updated.Active {
		t.Fatalf("pending rebind user was disabled: %#v", updated)
	}
}

func TestTelegramMembershipRechecksRebindBeforeDisable(t *testing.T) {
	app := setupTelegramMembershipTest(t)
	user, err := app.store().CreateUser(store.User{
		Username:   "racing-rebind",
		Role:       store.RoleNormal,
		Active:     true,
		TelegramID: 42002,
		EmbyID:     "emby-racing-rebind",
	})
	if err != nil {
		t.Fatal(err)
	}
	checked := make(chan struct{})
	release := make(chan struct{})
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-checked:
		default:
			close(checked)
		}
		<-release
		_, _ = w.Write([]byte(`{"ok":true,"result":{"status":"left","user":{"id":42002,"is_bot":false}}}`))
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL

	type outcome struct {
		summary map[string]any
		logs    []string
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		summary, logs, runErr := app.enforceTelegramMembership(context.Background(), false)
		done <- outcome{summary: summary, logs: logs, err: runErr}
	}()
	select {
	case <-checked:
	case <-time.After(3 * time.Second):
		t.Fatal("membership check did not start")
	}
	if _, err := app.store().CreateRebindRequest(store.RebindRequest{UID: user.UID, Username: user.Username, OldTelegramID: user.TelegramID}); err != nil {
		t.Fatal(err)
	}
	close(release)
	var result outcome
	select {
	case result = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("membership enforcement did not finish")
	}
	if result.err != nil {
		t.Fatalf("membership enforcement failed: %v logs=%v", result.err, result.logs)
	}
	if int(numeric(result.summary["disabled"])) != 0 || int(numeric(result.summary["rebind_protected"])) != 1 {
		t.Fatalf("rebind submit race was not protected: %#v", result.summary)
	}
	updated, found := app.store().User(user.UID)
	if !found || !updated.Active {
		t.Fatalf("user was disabled despite a concurrent rebind request: %#v", updated)
	}
}

// createMembershipUsers 建 n 个绑定 Telegram 的普通用户，TG ID 从 base 起递增。
func createMembershipUsers(t *testing.T, app *App, prefix string, base int64, n int) []store.User {
	t.Helper()
	users := make([]store.User, 0, n)
	for i := 0; i < n; i++ {
		u, err := app.store().CreateUser(store.User{Username: fmt.Sprintf("%s-%d", prefix, i), Role: store.RoleNormal, Active: true, TelegramID: base + int64(i)})
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, u)
	}
	return users
}

// TestTelegramMembershipChatNotFoundAbortsWithoutDisabling 回归审查 H2：群 ID 配错时
// getChatMember 回 "chat not found"，旧实现把它当成用户退群，一轮停用全站。
func TestTelegramMembershipChatNotFoundAbortsWithoutDisabling(t *testing.T) {
	app := setupTelegramMembershipTest(t)
	users := createMembershipUsers(t, app, "chatnf", 43000, 2)
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`))
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL

	summary, _, err := app.enforceTelegramMembership(context.Background(), false)
	if err == nil {
		t.Fatalf("chat-level error must fail the run, summary=%#v", summary)
	}
	if !boolish(summary["aborted"]) || asString(summary["abort_reason"]) != "chat_level_error" || int(numeric(summary["disabled"])) != 0 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	for _, u := range users {
		if cur, _ := app.store().User(u.UID); !cur.Active {
			t.Fatalf("user %d disabled by a chat-level error", u.UID)
		}
	}
}

// TestTelegramMembershipBreakerTripsOnMassLeave 回归 H2 熔断：本轮拟停用比例超过阈值时
// 整轮中止，不停用任何人。
func TestTelegramMembershipBreakerTripsOnMassLeave(t *testing.T) {
	app := setupTelegramMembershipTest(t)
	users := createMembershipUsers(t, app, "massleave", 44000, 10)
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"status":"left","user":{"id":1,"is_bot":false}}}`))
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL

	summary, _, err := app.enforceTelegramMembership(context.Background(), false)
	if err == nil || !boolish(summary["circuit_breaker_tripped"]) {
		t.Fatalf("breaker should trip, err=%v summary=%#v", err, summary)
	}
	if int(numeric(summary["disabled"])) != 0 || int(numeric(summary["would_disable"])) != 10 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	for _, u := range users {
		if cur, _ := app.store().User(u.UID); !cur.Active {
			t.Fatalf("user %d disabled although breaker tripped", u.UID)
		}
	}
}

// TestTelegramMembershipDryRunDoesNotDisable 覆盖 enforce_group_membership 的 dry_run：
// 只回报名单，不停用。
func TestTelegramMembershipDryRunDoesNotDisable(t *testing.T) {
	app := setupTelegramMembershipTest(t)
	users := createMembershipUsers(t, app, "dryrun", 45000, 10)
	leaver := users[0].TelegramID
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID int64 `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		status := "member"
		if body.UserID == leaver {
			status = "left"
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"status":%q,"user":{"id":%d,"is_bot":false}}}`, status, body.UserID)
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL

	summary, _, err := app.enforceTelegramMembershipWithOptions(context.Background(), telegramMembershipOptions{DryRun: true, BreakerPercent: 20, BreakerMax: 50})
	if err != nil {
		t.Fatalf("dry-run failed: %v summary=%#v", err, summary)
	}
	if int(numeric(summary["would_disable"])) != 1 || int(numeric(summary["disabled"])) != 0 {
		t.Fatalf("unexpected dry-run summary: %#v", summary)
	}
	if cur, _ := app.store().User(users[0].UID); !cur.Active {
		t.Fatal("dry-run must not disable users")
	}
	// 同样的数据实跑：1/10 未触发熔断，真正停用该用户。
	summary, _, err = app.enforceTelegramMembership(context.Background(), false)
	if err != nil || int(numeric(summary["disabled"])) != 1 {
		t.Fatalf("real run should disable the leaver, err=%v summary=%#v", err, summary)
	}
}

func TestClassifyTelegramMembershipError(t *testing.T) {
	cases := map[string]telegramMembershipErrKind{
		"telegram getChatMember failed: Bad Request: chat not found (400)":                       telegramMembershipErrChatLevel,
		"telegram getChatMember failed: Forbidden: bot was kicked from the supergroup chat (403)": telegramMembershipErrChatLevel,
		"telegram getChatMember failed: Bad Request: user not found (400)":                       telegramMembershipErrUserMissing,
		"telegram getChatMember failed: Bad Request: PARTICIPANT_ID_INVALID (400)":               telegramMembershipErrUserMissing,
		"telegram getChatMember failed: Internal Server Error (500)":                             telegramMembershipErrOther,
	}
	for msg, want := range cases {
		if got := classifyTelegramMembershipError(errors.New(msg)); got != want {
			t.Errorf("%q => %v, want %v", msg, got, want)
		}
	}
}

// TestTelegramRejoinSkipsManuallyDisabledUser 回归审查 H3：管理员手动停权的账号即使
// 人在群里，回群自动启用也不能把它放出来。
func TestTelegramRejoinSkipsManuallyDisabledUser(t *testing.T) {
	app := setupTelegramMembershipTest(t)
	user, err := app.store().CreateUser(store.User{Username: "manual-ban", Role: store.RoleNormal, Active: true, TelegramID: 46001})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().SetUserActiveAtomic(user.UID, false); err != nil {
		t.Fatal(err)
	}
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"status":"member","user":{"id":46001,"is_bot":false}}}`))
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL

	summary, _, err := app.enforceTelegramMembership(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if int(numeric(summary["rejoined_enabled"])) != 0 || int(numeric(summary["skipped_other_disabled"])) != 1 {
		t.Fatalf("manually disabled user must not be re-enabled: %#v", summary)
	}
	if cur, _ := app.store().User(user.UID); cur.Active {
		t.Fatal("manually disabled user was re-enabled by rejoin")
	}
}

// fakeEmbyPolicyServer 模拟 Emby 的 GET /Users/{id} 与 POST /Users/{id}/Policy，
// 记录每个账号当前的 IsDisabled。
type fakeEmbyPolicyServer struct {
	mu       sync.Mutex
	disabled map[string]bool
	admins   map[string]bool
	server   *httptest.Server
}

func newFakeEmbyPolicyServer(t *testing.T, app *App, users map[string]bool) *fakeEmbyPolicyServer {
	t.Helper()
	f := &fakeEmbyPolicyServer{disabled: users, admins: map[string]bool{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/Users" {
			out := []map[string]any{}
			for id, disabled := range f.disabled {
				out = append(out, map[string]any{"Id": id, "Name": "n-" + id, "Policy": map[string]any{"IsDisabled": disabled, "IsAdministrator": f.admins[id]}})
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/Users/"), "/")
		id := parts[0]
		disabled, ok := f.disabled[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet && len(parts) == 1:
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Name": "n-" + id, "Policy": map[string]any{"IsDisabled": disabled}})
		case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "Policy":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.disabled[id] = body["IsDisabled"] == true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	app.cfg().EmbyURL = f.server.URL
	app.cfg().EmbyToken = "emby-token"
	return f
}

func (f *fakeEmbyPolicyServer) isDisabled(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disabled[id]
}

// TestTelegramLeaveThenRejoinRestoresEmby 回归审查 H3/M8：退群停用时 Emby 随之停用，
// 回群自动启用时 Web 与 Emby 都要恢复；管理员单独封禁的 Emby 保持停用。
func TestTelegramLeaveThenRejoinRestoresEmby(t *testing.T) {
	app := setupTelegramMembershipTest(t)
	leaver, err := app.store().CreateUser(store.User{Username: "leaver", Role: store.RoleNormal, Active: true, TelegramID: 47001, EmbyID: "emby-leaver"})
	if err != nil {
		t.Fatal(err)
	}
	banned, err := app.store().CreateUser(store.User{Username: "embybanned", Role: store.RoleNormal, Active: true, TelegramID: 47002, EmbyID: "emby-banned"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ { // 凑够人数，避免触发百分比熔断
		if _, err := app.store().CreateUser(store.User{Username: fmt.Sprintf("stay-%d", i), Role: store.RoleNormal, Active: true, TelegramID: int64(47100 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	emby := newFakeEmbyPolicyServer(t, app, map[string]bool{"emby-leaver": false, "emby-banned": false})
	// 管理员先单独封禁 banned 的 Emby（Web 正常 → 不是系统停用）。
	if err := app.embyApplyEnabledState(context.Background(), banned.UID, banned.EmbyID, false); err != nil {
		t.Fatal(err)
	}
	var left atomic.Bool
	left.Store(true)
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			UserID int64 `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		status := "member"
		if left.Load() && (body.UserID == 47001 || body.UserID == 47002) {
			status = "left"
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"status":%q,"user":{"id":%d,"is_bot":false}}}`, status, body.UserID)
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL

	if summary, _, err := app.enforceTelegramMembership(context.Background(), true); err != nil || int(numeric(summary["disabled"])) != 2 {
		t.Fatalf("leave run: err=%v summary=%#v", err, summary)
	}
	if cur, _ := app.store().User(leaver.UID); cur.Active || cur.DisabledReason != store.DisabledReasonTelegramMembership || !cur.EmbyAutoDisabled || !emby.isDisabled("emby-leaver") {
		t.Fatalf("leaver not disabled as expected: %#v", cur)
	}
	left.Store(false)
	summary, _, err := app.enforceTelegramMembership(context.Background(), true)
	if err != nil || int(numeric(summary["rejoined_enabled"])) != 2 {
		t.Fatalf("rejoin run: err=%v summary=%#v", err, summary)
	}
	if cur, _ := app.store().User(leaver.UID); !cur.Active || cur.DisabledReason != "" || cur.EmbyDisabled || emby.isDisabled("emby-leaver") {
		t.Fatalf("rejoin must restore web and Emby: %#v remote=%v", cur, emby.isDisabled("emby-leaver"))
	}
	if cur, _ := app.store().User(banned.UID); !cur.Active || !emby.isDisabled("emby-banned") {
		t.Fatalf("admin Emby ban must survive rejoin: %#v remote=%v", cur, emby.isDisabled("emby-banned"))
	}
}
