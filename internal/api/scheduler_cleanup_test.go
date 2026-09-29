package api

import (
	"encoding/json"
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

func schedulerJobsForTest(t *testing.T, app *App) (map[string]map[string]any, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	app.handleSchedulerJobs(rr, httptest.NewRequest(http.MethodGet, "/api/v1/admin/scheduler/jobs", nil), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("jobs status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	jobs := map[string]map[string]any{}
	list, _ := body.Data["jobs"].([]any)
	for _, raw := range list {
		job, _ := raw.(map[string]any)
		jobs[asString(job["id"])] = job
	}
	return jobs, body.Data
}

// TestSchedulerJobsTimezoneAndDisabledNextRun 回归排程缺口：
//   - 返回调度时区，cron_daily 的下次时间按该时区计算；
//   - 不会自动执行的任务（cleanup_unlinked_emby、未开启的 system_auto_update）不显示下次时间；
//   - 群成员巡检与绑定检查有自己的默认时间，不再都挤在 03:00。
func TestSchedulerJobsTimezoneAndDisabledNextRun(t *testing.T) {
	app := newTestApp(t)
	app.cfg().SchedulerTimezone = "Asia/Tokyo"
	app.cfg().SchedulerEnabled = true
	app.cfg().SchedulerExpiredCheckTime = "03:00"
	app.cfg().SchedulerGroupMembershipCheckTime = "03:10"
	app.cfg().SchedulerTelegramBindingsCheckTime = "03:20"
	app.cfg().SystemUpdateEnabled = false
	// This cycle already completed; without it an overdue daily job is due now.
	if err := app.store().AddSchedulerRun(store.SchedulerRun{JobID: "check_expired", Type: "auto", Status: "success", StartedAt: time.Now().Unix(), FinishedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	jobs, data := schedulerJobsForTest(t, app)
	if asString(data["timezone"]) != "Asia/Tokyo" || int(numeric(data["utc_offset_seconds"])) != 9*3600 {
		t.Fatalf("timezone not reported: %#v", data)
	}
	next := time.Unix(int64(numeric(jobs["check_expired"]["next_run_at"])), 0).In(time.FixedZone("JST", 9*3600))
	if next.Hour() != 3 || next.Minute() != 0 {
		t.Fatalf("check_expired should run at 03:00 Asia/Tokyo, got %s", next)
	}
	for _, id := range []string{"cleanup_unlinked_emby", "system_auto_update"} {
		if jobs[id]["next_run_at"] != nil || !boolish(jobs[id]["auto_disabled"]) {
			t.Fatalf("%s must not show a next run time: %#v", id, jobs[id])
		}
	}
	for id, minute := range map[string]int{"enforce_group_membership": 10, "check_telegram_bindings": 20} {
		spec, _ := jobs[id]["trigger_spec"].(map[string]any)
		if int(numeric(spec["hour"])) != 3 || int(numeric(spec["minute"])) != minute {
			t.Fatalf("%s default time wrong: %#v", id, spec)
		}
	}
}
