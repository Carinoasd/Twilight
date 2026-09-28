package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

// TestCheckExpiredCountsEmbyDisableFailure 回归审查 H4：check_expired 先停 Web 再停 Emby，
// 旧实现把 Emby 失败吞掉、摘要仍是成功；现在要计数、列出 uid 并让本轮显示失败。
func TestCheckExpiredCountsEmbyDisableFailure(t *testing.T) {
	app := newTestApp(t)
	user, err := app.store().CreateUser(store.User{Username: "expired-emby", Role: store.RoleNormal, Active: true, EmbyID: "emby-exp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().UpdateUser(user.UID, func(u *store.User) error { u.ExpiredAt = 1000000000; return nil }); err != nil {
		t.Fatal(err)
	}
	app.cfg().EmbyToken = "emby-token"
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer emby.Close()
	app.cfg().EmbyURL = emby.URL

	summary, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler/internal", nil), "check_expired")
	if err != nil {
		t.Fatal(err)
	}
	if int(numeric(summary["disabled"])) != 1 || int(numeric(summary["emby_disable_failed"])) != 1 || boolish(summary["success"]) {
		t.Fatalf("Emby failure must be counted and mark the run unsuccessful: %#v", summary)
	}
}

// TestEmbyStateReconcileConvergesMissedChanges 覆盖新的 emby_state_reconcile：
//   - Web 已停用、Emby 仍启用 → 停用；
//   - 系统自动停用（EmbyAutoDisabled）且 Web 已恢复 → 重新启用；
//   - 管理员单独封禁（无标记）→ 保持停用；
//   - 远端 Emby 管理员 → 跳过。
func TestEmbyStateReconcileConvergesMissedChanges(t *testing.T) {
	app := newTestApp(t)
	mk := func(name, embyID string, mutate func(u *store.User)) store.User {
		t.Helper()
		u, err := app.store().CreateUser(store.User{Username: name, Role: store.RoleNormal, Active: true, EmbyID: embyID})
		if err != nil {
			t.Fatal(err)
		}
		u, err = app.store().UpdateUser(u.UID, func(u *store.User) error { mutate(u); return nil })
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	inactive := mk("r-inactive", "e-inactive", func(u *store.User) { u.Active = false })
	renewed := mk("r-renewed", "e-renewed", func(u *store.User) { u.EmbyDisabled = true; u.EmbyAutoDisabled = true })
	banned := mk("r-banned", "e-banned", func(u *store.User) { u.EmbyDisabled = true })
	embyAdmin := mk("r-admin", "e-admin", func(u *store.User) { u.Active = false })
	emby := newFakeEmbyPolicyServer(t, app, map[string]bool{"e-inactive": false, "e-renewed": true, "e-banned": true, "e-admin": false})
	emby.admins["e-admin"] = true

	preview, _, err := app.runEmbyStateReconcile(context.Background(), true, 200)
	if err != nil || int(numeric(preview["planned"])) != 2 || emby.isDisabled("e-inactive") {
		t.Fatalf("dry-run must only plan: err=%v summary=%#v", err, preview)
	}
	summary, _, err := app.runEmbyStateReconcile(context.Background(), false, 200)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}
	if int(numeric(summary["disabled"])) != 1 || int(numeric(summary["enabled"])) != 1 || int(numeric(summary["skipped_manual_ban"])) != 1 || int(numeric(summary["skipped_emby_admin"])) != 1 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	if !emby.isDisabled("e-inactive") || emby.isDisabled("e-renewed") || !emby.isDisabled("e-banned") || emby.isDisabled("e-admin") {
		t.Fatalf("remote state not converged: %#v", emby.disabled)
	}
	if cur, _ := app.store().User(inactive.UID); !cur.EmbyDisabled || !cur.EmbyAutoDisabled {
		t.Fatalf("inactive user should carry the auto-disabled marker: %#v", cur)
	}
	if cur, _ := app.store().User(renewed.UID); cur.EmbyDisabled || cur.EmbyAutoDisabled {
		t.Fatalf("renewed user mirror not cleared: %#v", cur)
	}
	_ = banned
	_ = embyAdmin
}

// TestEmbyStateReconcileAbortsOverMaxChanges 计划改动超过上限时整轮中止、不改任何账号。
func TestEmbyStateReconcileAbortsOverMaxChanges(t *testing.T) {
	app := newTestApp(t)
	remote := map[string]bool{}
	for _, id := range []string{"m1", "m2", "m3"} {
		u, err := app.store().CreateUser(store.User{Username: "max-" + id, Role: store.RoleNormal, Active: true, EmbyID: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := app.store().SetUserActiveAtomic(u.UID, false); err != nil {
			t.Fatal(err)
		}
		remote[id] = false
	}
	emby := newFakeEmbyPolicyServer(t, app, remote)
	summary, _, err := app.runEmbyStateReconcile(context.Background(), false, 2)
	if err == nil || !boolish(summary["aborted"]) {
		t.Fatalf("expected abort, err=%v summary=%#v", err, summary)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		if emby.isDisabled(id) {
			t.Fatalf("%s changed despite abort", id)
		}
	}
}

// TestEmbySyncMaxUsersUsesCursor 回归审查 L10：emby_sync 的 max_users 截断后要留下游标，
// 下一批从游标之后继续，而不是每次都只处理前 N 个用户。
func TestEmbySyncMaxUsersUsesCursor(t *testing.T) {
	app := newTestApp(t)
	remote := map[string]bool{}
	users := []store.User{}
	for _, id := range []string{"c1", "c2", "c3"} {
		u, err := app.store().CreateUser(store.User{Username: "cursor-" + id, Role: store.RoleNormal, Active: true, EmbyID: id, EmbyUsername: "old-" + id})
		if err != nil {
			t.Fatal(err)
		}
		users = append(users, u)
		remote[id] = false
	}
	newFakeEmbyPolicyServer(t, app, remote)
	run := func(params map[string]any) map[string]any {
		t.Helper()
		ctx := context.WithValue(context.Background(), schedulerFrozenParamsKey{}, params)
		req := httptest.NewRequest(http.MethodPost, "/scheduler/internal", nil).WithContext(ctx)
		summary, _, err := app.runSchedulerJob(req, "emby_sync")
		if err != nil {
			t.Fatal(err)
		}
		return summary
	}
	first := run(map[string]any{"max_users": 2})
	if int64(numeric(first["next_after_uid"])) != users[1].UID || !boolish(first["truncated"]) || int(numeric(first["batch_users"])) != 2 {
		t.Fatalf("first batch should stop after the second user: %#v", first)
	}
	if cur, _ := app.store().User(users[2].UID); cur.EmbyUsername != "old-c3" {
		t.Fatalf("third user must not be touched in the first batch: %#v", cur)
	}
	second := run(map[string]any{"max_users": 2, "after_uid": first["next_after_uid"]})
	if int(numeric(second["batch_users"])) != 1 || int64(numeric(second["next_after_uid"])) != 0 {
		t.Fatalf("second batch should cover the rest and wrap: %#v", second)
	}
	if cur, _ := app.store().User(users[2].UID); cur.EmbyUsername != "n-c3" {
		t.Fatalf("third user should be synced in the second batch: %#v", cur)
	}
}

// TestEmbyCreateUserDeletesAccountWhenPolicyFails 回归审查 L11：新建 Emby 账号后收紧权限
// 失败时，旧实现忽略错误、留下带默认（过宽）权限的账号；现在要删掉并回错。
func TestEmbyCreateUserDeletesAccountWhenPolicyFails(t *testing.T) {
	app := newTestApp(t)
	deleted := false
	app.cfg().EmbyToken = "emby-token"
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/New":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"fresh","Name":"fresh"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/Users/fresh":
			w.WriteHeader(http.StatusForbidden)
		case r.Method == http.MethodDelete && r.URL.Path == "/Users/fresh":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Emby request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer emby.Close()
	app.cfg().EmbyURL = emby.URL
	if _, err := app.embyCreateUser(context.Background(), "fresh", "Passw0rd!x"); err == nil {
		t.Fatal("policy hardening failure must fail user creation")
	}
	if !deleted {
		t.Fatal("new Emby user with default policy must be deleted")
	}
}
