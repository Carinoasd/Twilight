package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

func TestSchedulerNextRunMatchesDueDecision(t *testing.T) {
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.FixedZone("JST", 9*3600))
	daily := map[string]any{"type": "cron_daily", "hour": 3, "minute": 0}
	interval := map[string]any{"type": "interval", "seconds": 3600}
	last := func(at time.Time) store.SchedulerRunSnapshot {
		return store.SchedulerRunSnapshot{HasLatestAuto: true, LatestAuto: store.SchedulerRun{StartedAt: at.Unix()}}
	}
	for _, tt := range []struct {
		name     string
		spec     map[string]any
		snapshot store.SchedulerRunSnapshot
		want     time.Time
	}{
		{"missed daily", daily, store.SchedulerRunSnapshot{}, now},
		{"yesterday daily", daily, last(now.Add(-24 * time.Hour)), now},
		{"completed daily", daily, last(now.Add(-time.Hour)), now.AddDate(0, 0, 1).Add(-7 * time.Hour)},
		{"overdue interval", interval, last(now.Add(-2 * time.Hour)), now},
		{"future interval", interval, last(now.Add(-30 * time.Minute)), now.Add(30 * time.Minute)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := schedulerNextRunAtFromSnapshot(tt.spec, now, tt.snapshot)
			if got != tt.want.Unix() {
				t.Fatalf("next=%s want=%s", time.Unix(got, 0), tt.want)
			}
			if due := schedulerJobDueFromSnapshot(tt.spec, now, tt.snapshot); due != (got <= now.Unix()) {
				t.Fatalf("next run disagrees with due decision: next=%d due=%v", got, due)
			}
		})
	}
}

func TestSchedulerJobsRespectGlobalDisableAndQueuedWork(t *testing.T) {
	app := newTestApp(t)
	app.cfg().SchedulerEnabled = false
	jobs, _ := schedulerJobsForTest(t, app)
	for id, job := range jobs {
		if job["next_run_at"] != nil || !boolish(job["auto_disabled"]) {
			t.Fatalf("disabled scheduler advertises automatic work: %s %#v", id, job)
		}
	}
	// Disabling automatic scheduling must still allow manual work to queue.
	if _, created, err := app.enqueueSchedulerJob(context.Background(), "daily_stats", nil, "manual"); err != nil || !created {
		t.Fatalf("manual enqueue: created=%v err=%v", created, err)
	}
	app.cfg().SchedulerEnabled = true
	jobs, _ = schedulerJobsForTest(t, app)
	if job := jobs["daily_stats"]; !boolish(job["is_running"]) || job["next_run_at"] != nil || boolish(job["auto_disabled"]) {
		t.Fatalf("queued work should suppress an overlapping next run: %#v", job)
	}
}

func TestSchedulerNextRunIncludesSafeRetry(t *testing.T) {
	app := newTestApp(t)
	app.cfg().SchedulerEnabled = true
	app.cfg().SchedulerRetryFailedAfterMinutes = 15
	due := time.Date(2026, 9, 29, 3, 0, 0, 0, time.UTC)
	spec := map[string]any{"type": "cron_daily", "hour": 3, "minute": 0}
	snapshot := store.SchedulerRunSnapshot{HasLatestAuto: true, LatestAuto: store.SchedulerRun{Status: "failed", Trigger: "scheduler", StartedAt: due.Unix(), FinishedAt: due.Add(time.Minute).Unix()}}
	if got := app.schedulerNextAutomaticRunAt("check_expired", spec, due.Add(5*time.Minute), snapshot); got != due.Add(16*time.Minute).Unix() {
		t.Fatalf("next run omitted the pending retry: %s", time.Unix(got, 0))
	}
	if got := app.schedulerNextAutomaticRunAt("check_expired", spec, due.Add(time.Hour), snapshot); got != due.Add(time.Hour).Unix() {
		t.Fatalf("overdue retry should be due now: %s", time.Unix(got, 0))
	}
	if got := app.schedulerNextAutomaticRunAt("expiry_reminders", spec, due.Add(time.Hour), snapshot); got != due.AddDate(0, 0, 1).Unix() {
		t.Fatalf("notification delivery must not be automatically retried: %s", time.Unix(got, 0))
	}
	snapshot.LatestAuto.Trigger = schedulerRetryTrigger
	if got := app.schedulerNextAutomaticRunAt("check_expired", spec, due.Add(time.Hour), snapshot); got != due.AddDate(0, 0, 1).Unix() {
		t.Fatalf("failed retry must wait for the next daily cycle: %s", time.Unix(got, 0))
	}
	snapshot.LatestAuto.Trigger = "scheduler"
	snapshot.LatestAuto.FinishedAt = due.Add(20*time.Hour + 50*time.Minute).Unix()
	if got := app.schedulerNextAutomaticRunAt("check_expired", spec, due.Add(20*time.Hour+55*time.Minute), snapshot); got != due.AddDate(0, 0, 1).Unix() {
		t.Fatalf("retry cannot spill into the next local day: %s", time.Unix(got, 0))
	}
}

func TestSchedulerReminderFailureAndCancellation(t *testing.T) {
	for _, rateLimited := range []bool{false, true} {
		t.Run(fmt.Sprintf("rate_limited_%v", rateLimited), func(t *testing.T) {
			app := newTestApp(t)
			app.cfg().TelegramMode = true
			app.cfg().TelegramBotToken = "123:ABC"
			app.cfg().NotificationEnabled = true
			var calls atomic.Int32
			tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if rateLimited {
					_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":2}}`))
				} else {
					_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"bot was blocked"}`))
				}
			}))
			defer tg.Close()
			app.cfg().TelegramAPIURL = tg.URL
			for i := 0; i < 2; i++ {
				if _, err := app.store().CreateUser(store.User{Username: fmt.Sprintf("reminder-%d", i), Role: store.RoleNormal, Active: true, ExpiredAt: time.Now().Add(time.Hour).Unix(), TelegramID: int64(100 + i)}); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			if rateLimited {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				defer cancel()
			}
			started := time.Now()
			summary, logs, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil).WithContext(ctx), "expiry_reminders")
			elapsed := time.Since(started)
			if boolish(summary["success"]) || schedulerFinishedRun("expiry_reminders", "auto", "scheduler", started.Unix(), summary, logs, err).Status != "failed" {
				t.Fatalf("failed delivery reported success: %#v err=%v", summary, err)
			}
			if rateLimited && (!errors.Is(err, context.DeadlineExceeded) || elapsed > 1500*time.Millisecond || calls.Load() != 1) {
				t.Fatalf("cancellation must interrupt rate-limit wait: elapsed=%s calls=%d err=%v", elapsed, calls.Load(), err)
			}
			if !rateLimited && (err != nil || calls.Load() != 2 || len(summary["failed"].([]map[string]any)) != 2) {
				t.Fatalf("delivery failures lost: calls=%d summary=%#v err=%v", calls.Load(), summary, err)
			}
		})
	}
}

func TestSchedulerRemoteCleanupReportsPartialFailure(t *testing.T) {
	app := newTestApp(t)
	app.cfg().EmbyToken = "test-token"
	var deleted atomic.Int32
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[{"Id":"bad","Name":"bad"},{"Id":"good","Name":"good"}]`))
		case r.URL.Path == "/Users/bad":
			w.WriteHeader(http.StatusForbidden)
		default:
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer emby.Close()
	app.cfg().EmbyURL = emby.URL
	ctx := context.WithValue(context.Background(), schedulerFrozenParamsKey{}, map[string]any{"dry_run": false, "delete": true})
	summary, logs, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil).WithContext(ctx), "cleanup_unlinked_emby")
	if err != nil || boolish(summary["success"]) || numeric(summary["failed"]) != 1 || numeric(summary["deleted"]) != 1 || deleted.Load() != 1 {
		t.Fatalf("expected partial failure: summary=%#v err=%v", summary, err)
	}
	if run := schedulerFinishedRun("cleanup_unlinked_emby", "manual", "manual", time.Now().Unix(), summary, logs, err); run.Status != "failed" {
		t.Fatalf("remote cleanup falsely reported success: %#v", run)
	}
}

func TestSchedulerTicketCleanupReportsFileFailure(t *testing.T) {
	app := newTestApp(t)
	app.cfg().TicketImageRetentionDays = 1
	const filename = "0000000000000001.png"
	ticket, err := app.store().CreateTicket(store.Ticket{Title: "old ticket", Status: store.TicketStatusClosed, ClosedAt: time.Now().Add(-48 * time.Hour).Unix(), Attachments: []store.TicketAttachment{{Filename: filename}}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := app.ticketAttachmentDir(ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A nonempty directory at a file path fails removal on both Unix and Windows.
	target := filepath.Join(dir, filename)
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	summary, logs, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil), "cleanup_ticket_images")
	if err != nil || boolish(summary["success"]) || numeric(summary["failed"]) != 1 || numeric(summary["images"]) != 1 {
		t.Fatalf("file failure must be visible: summary=%#v logs=%v err=%v", summary, logs, err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); err != nil {
		t.Fatalf("cleanup must not recursively remove an unexpected directory: %v", err)
	}
}

func TestSchedulerExpiryCancellationPreservesCompletedAudit(t *testing.T) {
	app := newTestApp(t)
	app.cfg().EmbyToken = "test-token"
	app.cfg().AuditLogEnabled = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"Id":"remote","Policy":{"IsDisabled":false}}`))
			return
		}
		cancel()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer emby.Close()
	app.cfg().EmbyURL = emby.URL
	for i := 0; i < 3; i++ {
		if _, err := app.store().CreateUser(store.User{Username: fmt.Sprintf("expired-%d", i), EmbyID: fmt.Sprintf("remote-%d", i), Role: store.RoleNormal, Active: true, ExpiredAt: time.Now().Add(-time.Hour).Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	summary, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil).WithContext(ctx), "check_expired")
	if !errors.Is(err, context.Canceled) || numeric(summary["disabled"]) != 1 || boolish(summary["success"]) {
		t.Fatalf("cancellation did not stop before next user: summary=%#v err=%v", summary, err)
	}
	disabled := 0
	for _, user := range app.store().ListUsers() {
		if !user.Active {
			disabled++
		}
	}
	if disabled != 1 {
		t.Fatalf("cancelled expiry changed %d users, want 1", disabled)
	}
	for _, entry := range app.store().ListAuditLogs() {
		if entry.Action == "disable_expired_users" {
			return
		}
	}
	t.Fatal("completed mutation lost its audit entry on cancellation")
}

func TestSchedulerRenewalReportsRemoteEnableFailure(t *testing.T) {
	app := newTestApp(t)
	app.cfg().SigninRenewalEnabled = true
	app.cfg().SigninAutoRenewalEnabled = true
	app.cfg().SigninRenewalCost = 40
	app.cfg().SigninRenewalDays = 10
	app.cfg().EmbyToken = "test-token"
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer emby.Close()
	app.cfg().EmbyURL = emby.URL
	user, err := app.store().CreateUser(store.User{Username: "renew-enable-failure", EmbyID: "remote", EmbyDisabled: true, EmbyAutoDisabled: true, SigninAutoRenewal: true, Role: store.RoleNormal, Active: true, ExpiredAt: time.Now().Add(-time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.store().AddSigninWithOptions(user.UID, 100, nil, true); err != nil {
		t.Fatal(err)
	}
	summary, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil), "check_expired")
	if err != nil || boolish(summary["success"]) || numeric(summary["auto_renewed"]) != 1 || numeric(summary["auto_renewal_emby_enable_failed"]) != 1 {
		t.Fatalf("remote enable failure lost: summary=%#v err=%v", summary, err)
	}
	// A repeat scan must not charge again after the successful local renewal.
	if _, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil), "check_expired"); err != nil {
		t.Fatal(err)
	}
	if got := app.store().Signin(user.UID).Points; got != 60 {
		t.Fatalf("renewal charged more than once: points=%d", got)
	}
}
