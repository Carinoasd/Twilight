package api

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// TestSchedulerRetryDueOnlyOncePerDailyFailure 覆盖失败重试：只有可安全重放的每日任务、
// 今天这轮失败、尚未重试、且过了设定分钟数才重试一次。
func TestSchedulerRetryDueOnlyOncePerDailyFailure(t *testing.T) {
	app := newTestApp(t)
	app.cfg().SchedulerRetryFailedAfterMinutes = 15
	loc := time.Local
	now := time.Now().In(loc)
	today3 := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, loc)
	at := today3.Add(time.Hour)
	spec := map[string]any{"type": "cron_daily", "hour": 3, "minute": 0}
	failed := store.SchedulerRunSnapshot{HasLatestAuto: true, LatestAuto: store.SchedulerRun{Status: "failed", Trigger: "scheduler", StartedAt: today3.Unix(), FinishedAt: today3.Add(time.Minute).Unix()}}
	if !app.schedulerRetryDue("check_expired", spec, at, failed) {
		t.Fatal("failed idempotent daily job should be retried")
	}
	if app.schedulerRetryDue("expiry_reminders", spec, at, failed) {
		t.Fatal("non-idempotent job must not be retried")
	}
	if app.schedulerRetryDue("check_expired", spec, today3.Add(5*time.Minute), failed) {
		t.Fatal("retry must wait for the configured delay")
	}
	retried := failed
	retried.LatestAuto.Trigger = schedulerRetryTrigger
	if app.schedulerRetryDue("check_expired", spec, at, retried) {
		t.Fatal("a failed retry must not be retried again")
	}
	app.cfg().SchedulerRetryFailedAfterMinutes = 0
	if app.schedulerRetryDue("check_expired", spec, at, failed) {
		t.Fatal("retry disabled by config")
	}
}

// TestSchedulerFailureNotifyThrottled 覆盖失败通知：连续失败只通知第一次，恢复时再通知一次。
func TestSchedulerFailureNotifyThrottled(t *testing.T) {
	app := newTestApp(t)
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:ABC"
	app.cfg().TelegramAdminIDs = []int64{777}
	app.cfg().SchedulerFailureNotify = true
	var sent atomic.Int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bot123:ABC/sendMessage" {
			sent.Add(1)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1,"chat":{"id":777}}}`))
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL
	add := func(status string) store.SchedulerRun {
		t.Helper()
		run, err := app.store().AddSchedulerRunReturning(store.SchedulerRun{JobID: "daily_stats", Type: "auto", Trigger: "scheduler", Status: status, Message: status, Error: "boom", StartedAt: time.Now().Unix(), FinishedAt: time.Now().Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	app.notifySchedulerOutcome(app.store(), add("failed"))
	app.notifySchedulerOutcome(app.store(), add("failed"))
	if got := sent.Load(); got != 1 {
		t.Fatalf("consecutive failures should notify once, sent=%d", got)
	}
	app.notifySchedulerOutcome(app.store(), add("success"))
	if got := sent.Load(); got != 2 {
		t.Fatalf("recovery should notify once, sent=%d", got)
	}
	app.notifySchedulerOutcome(app.store(), add("success"))
	if got := sent.Load(); got != 2 {
		t.Fatalf("steady success must not notify, sent=%d", got)
	}
}
