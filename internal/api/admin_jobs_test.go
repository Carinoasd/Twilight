package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
