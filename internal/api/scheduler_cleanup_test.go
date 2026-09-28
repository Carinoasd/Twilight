package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// TestSchedulerCleanupNoEmbyKeepsRecentlyUnboundUser 回归审查 M7：注册很久、刚解绑 Emby
// 的老用户不能被 cleanup_no_emby 当成「注册后长期未开通」直接删掉。
func TestSchedulerCleanupNoEmbyKeepsRecentlyUnboundUser(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AutoCleanupNoEmby = true
	app.cfg().AutoCleanupNoEmbyDays = 7
	old := time.Now().AddDate(-1, 0, 0).Unix()
	veteran, err := app.store().CreateUser(store.User{Username: "veteran", Role: store.RoleNormal, Active: true, EmbyID: "emby-veteran", RegisterTime: old, CreatedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().UpdateUser(veteran.UID, func(u *store.User) error { u.EmbyID = ""; u.EmbyUsername = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	stale, err := app.store().CreateUser(store.User{Username: "never-emby", Role: store.RoleNormal, Active: true, RegisterTime: old, CreatedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	summary, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil), "cleanup_no_emby")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := app.store().User(veteran.UID); !ok {
		t.Fatalf("recently unbound veteran was deleted: %#v", summary)
	}
	if _, ok := app.store().User(stale.UID); ok {
		t.Fatalf("long-time no-Emby user should still be cleaned: %#v", summary)
	}
}

// TestSchedulerFinishedRunTreatsSuccessFalseAsFailed 回归排程缺口：摘要 success=false
// 但没有 error 时，旧实现仍显示成功。
func TestSchedulerFinishedRunTreatsSuccessFalseAsFailed(t *testing.T) {
	run := schedulerFinishedRun("refresh_bangumi_collections", "auto", "scheduler", time.Now().Unix(), map[string]any{"success": false, "failed": 2}, nil, nil)
	if run.Status != "failed" || run.Error == "" {
		t.Fatalf("success=false must be reported as failed: %#v", run)
	}
	ok := schedulerFinishedRun("daily_stats", "auto", "scheduler", time.Now().Unix(), map[string]any{"users": 1}, nil, nil)
	if ok.Status != "success" {
		t.Fatalf("summary without success key must stay successful: %#v", ok)
	}
}

// TestCleanupSessionsPartialWhenEmbyUnavailable 回归排程缺口：前面的清理都做完了，只是
// 读 Emby 会话失败时，不能把整轮标成失败；同时 Telegram 绑定链接要并入例行清理。
func TestCleanupSessionsPartialWhenEmbyUnavailable(t *testing.T) {
	app := newTestApp(t)
	app.cfg().EmbyToken = "emby-token"
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer emby.Close()
	app.cfg().EmbyURL = emby.URL
	summary, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil), "cleanup_sessions")
	if err != nil {
		t.Fatalf("Emby read failure must not fail cleanup_sessions: %v", err)
	}
	if !boolish(summary["success"]) || !boolish(summary["partial"]) || asString(summary["emby_error"]) == "" {
		t.Fatalf("expected partial success: %#v", summary)
	}
	if _, ok := summary["expired_telegram_links"]; !ok {
		t.Fatalf("Telegram bind links must be cleaned up routinely: %#v", summary)
	}
}

// TestCleanupPendingEmbyEntitlementsRespectsAge 回归排程缺口：cleanup_pending_emby_entitlements
// 旧实现没有年龄门槛，刚发放的资格下一分钟就会被收回。
func TestCleanupPendingEmbyEntitlementsRespectsAge(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AutoCleanupPendingEmby = true
	app.cfg().AutoCleanupPendingEmbyDays = 7
	old := time.Now().AddDate(0, 0, -30).Unix()
	fresh, err := app.store().CreateUser(store.User{Username: "fresh-grant", Role: store.RoleNormal, Active: true, RegisterTime: old, CreatedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	days := 30
	// 老用户今天才被发放资格：发放时间由 store 自动记录为现在。
	if _, err := app.store().UpdateUser(fresh.UID, func(u *store.User) error { u.PendingEmby = true; u.PendingEmbyDays = &days; return nil }); err != nil {
		t.Fatal(err)
	}
	stale, err := app.store().CreateUser(store.User{Username: "stale-grant", Role: store.RoleNormal, Active: true, PendingEmby: true, PendingEmbyDays: &days, RegisterTime: old, CreatedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	summary, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil), "cleanup_pending_emby_entitlements")
	if err != nil {
		t.Fatal(err)
	}
	if cur, _ := app.store().User(fresh.UID); !cur.PendingEmby {
		t.Fatalf("fresh grant was revoked: %#v", summary)
	}
	if cur, _ := app.store().User(stale.UID); cur.PendingEmby {
		t.Fatalf("stale grant should be revoked: %#v", summary)
	}
}
