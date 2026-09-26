package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/prejudice-studio/twilight/internal/migration"
)

func TestSchedulerQueueConcurrencyCancellationAndFencing(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	other := reopenTestStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	created := make(chan bool, 2)
	failures := make(chan error, 2)
	for _, db := range []*Store{st, other} {
		wg.Add(1)
		go func(db *Store) {
			defer wg.Done()
			_, ok, err := db.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: "same", Params: map[string]any{"dry_run": true}}, nil)
			created <- ok
			failures <- err
		}(db)
	}
	wg.Wait()
	close(created)
	close(failures)
	count := 0
	for ok := range created {
		if ok {
			count++
		}
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("created %d duplicates", count)
	}
	for i := 0; i < 5; i++ {
		if _, ok, err := st.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: fmt.Sprint("job", i)}, nil); err != nil || !ok {
			t.Fatalf("enqueue: %v %v", ok, err)
		}
	}
	runs := []SchedulerRun{}
	for i := 0; i < SchedulerConcurrency; i++ {
		run, ok, err := other.ClaimSchedulerRun(ctx, fmt.Sprint("worker", i))
		if err != nil || !ok {
			t.Fatalf("claim %d: %v", i, err)
		}
		runs = append(runs, run)
	}
	if _, ok, err := st.ClaimSchedulerRun(ctx, "overflow"); err != nil || ok {
		t.Fatalf("concurrency exceeded: %v %v", ok, err)
	}
	run := runs[0]
	if _, ok, err := st.RequestSchedulerCancellation(ctx, run.JobID); err != nil || !ok {
		t.Fatalf("cancel %v", err)
	}
	if _, ok, err := st.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: run.JobID}, nil); err != nil || ok {
		t.Fatalf("cancel released mutex: %v %v", ok, err)
	}
	if cancel, err := other.HeartbeatSchedulerRun(ctx, run.ID, run.Owner); err != nil || !cancel {
		t.Fatalf("cross-store cancel invisible: %v %v", cancel, err)
	}
	run.Status = "success"
	if err := st.FinishSchedulerRun(ctx, run, "wrong-owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign owner accepted: %v", err)
	}
	if err := other.FinishSchedulerRun(ctx, run, run.Owner); err != nil {
		t.Fatal(err)
	}
	overview, err := st.ReadSchedulerHistory(ctx, run.JobID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Runs[run.JobID].Runs[0].Status != "cancelled" {
		t.Fatal("completion lost cancellation")
	}
	if _, ok, err := st.ClaimSchedulerRun(ctx, "next"); err != nil || !ok {
		t.Fatalf("slot not released: %v", err)
	}
}

func TestSchedulerQueueLeaseRecoveryAndSnapshot(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	_, _, err = st.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: "long", Type: "auto"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := st.ClaimSchedulerRun(ctx, "owner")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE twilight_job_runs SET payload=jsonb_set(payload,'{started_at}',to_jsonb(1::bigint)) WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.HeartbeatSchedulerRun(ctx, run.ID, run.Owner); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.ClaimSchedulerRun(ctx, "observer"); err != nil || ok {
		t.Fatalf("unexpected claim: %v", err)
	}
	overview, err := st.ReadSchedulerOverview(ctx, []string{"long"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if overview.Runs["long"].Runs[0].Status != "running" {
		t.Fatal("long live run expired by start time")
	}
	backup, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LoadSnapshot(backup); !errors.Is(err, ErrConflict) {
		t.Fatalf("restore accepted live worker: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE twilight_job_runs SET lease_until=1 WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.ClaimSchedulerRun(ctx, "recovery"); err != nil || ok {
		t.Fatalf("unsafe automatic replay: %v", err)
	}
	run.Status = "success"
	if err := st.FinishSchedulerRun(ctx, run, run.Owner); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale completion accepted: %v", err)
	}
	if err := st.LoadSnapshot(backup); err != nil {
		t.Fatal(err)
	}
	var state State
	restored, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(restored, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.SchedulerRuns) != 1 || state.SchedulerRuns[0].Status == "running" {
		t.Fatalf("restore resumed command: %#v", state.SchedulerRuns)
	}
}

func TestSchedulerQueueAutoWatermarkAndScheduleRevision(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	revision := int64(0)
	schedule, err := st.SetSchedulerScheduleRevision("daily", map[string]any{"type": "manual"}, nil, true, &revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetSchedulerScheduleRevision("daily", nil, nil, true, &revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale config accepted: %v", err)
	}
	if _, err := st.SetSchedulerScheduleRevision("daily", nil, nil, false, &schedule.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetSchedulerScheduleRevision("daily", nil, nil, true, &schedule.Revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("reset revision lost: %v", err)
	}
	last := int64(0)
	if _, ok, err := st.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: "auto", Type: "auto"}, &last); err != nil || !ok {
		t.Fatalf("enqueue: %v", err)
	}
	run, ok, err := st.ClaimSchedulerRun(ctx, "one")
	if err != nil || !ok {
		t.Fatal(err)
	}
	run.Status = "success"
	if err = st.FinishSchedulerRun(ctx, run, run.Owner); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: "auto", Type: "auto"}, &last); err != nil || ok {
		t.Fatalf("stale auto occurrence queued: %v", err)
	}
	if _, ok, err := st.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: "pending"}, nil); err != nil || !ok {
		t.Fatal(err)
	}
	cancelled, ok, err := st.RequestSchedulerCancellation(ctx, "pending")
	if err != nil || !ok || cancelled.Status != "cancelled" {
		t.Fatalf("queued cancellation: %#v %v", cancelled, err)
	}
}

func TestSchedulerQueueCancellationHistoryIsBounded(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// A queue full of cancelled runs must be recoverable without running work.
	_, err = st.db.Exec(`INSERT INTO twilight_job_runs(job_id,status,created_at,payload)
SELECT 'retention','cancelled',1,jsonb_build_object('job_id','retention','type',CASE WHEN i=1 THEN 'auto' ELSE 'manual' END)
FROM generate_series(1,4000) i`)
	if err != nil {
		t.Fatal(err)
	}
	var watermark int64
	if err = st.db.QueryRow(`SELECT min(id) FROM twilight_job_runs`).Scan(&watermark); err != nil {
		t.Fatal(err)
	}
	if _, created, err := st.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: "retention", Type: "auto"}, &watermark); err != nil || !created {
		t.Fatalf("cancelled history blocked queue: created=%v err=%v", created, err)
	}
	var count int
	if err = st.db.QueryRow(`SELECT count(*) FROM twilight_job_runs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count > 102 {
		t.Fatalf("history unbounded: %d", count)
	}
	run, claimed, err := st.ClaimSchedulerRun(ctx, "retention-worker")
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err := st.RecordSchedulerConfigRevision(ctx, run.ID, "wrong-owner", "wrong"); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign owner changed fingerprint: %v", err)
	}
	if err := st.RecordSchedulerConfigRevision(ctx, run.ID, run.Owner, "fingerprint"); err != nil {
		t.Fatal(err)
	}
	overview, err := st.ReadSchedulerHistory(ctx, run.JobID, 1)
	if err != nil || overview.Runs[run.JobID].Runs[0].ConfigRevision != "fingerprint" {
		t.Fatal("running fingerprint missing", err)
	}
}

func TestSchedulerQueueMigrationRoundTrip(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	queued, _, err := st.EnqueueSchedulerRun(ctx, SchedulerRun{JobID: "archived", Params: map[string]any{"days": 7}}, nil)
	if err != nil {
		t.Fatal(err)
	}
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
	for i := 0; i < 2; i++ {
		if _, err = st.ImportMigrationArchive(ctx, archive); err != nil {
			t.Fatal(err)
		}
		view, err := st.ReadSchedulerHistory(ctx, "archived", 20)
		if err != nil {
			t.Fatal(err)
		}
		runs := view.Runs["archived"].Runs
		if len(runs) != 1 || runs[0].ID != queued.ID || SchedulerRunActive(runs[0].Status) {
			t.Fatalf("bad restore: %#v", runs)
		}
	}
	if _, claimed, err := st.ClaimSchedulerRun(ctx, "worker"); err != nil || claimed {
		t.Fatal("archived command replayed", err)
	}
	if _, err := st.db.Exec(`UPDATE twilight_job_schema SET version=2`); err != nil {
		t.Fatal(err)
	}
	if err := prepareSchedulerQueueSchema(ctx, st.db); err == nil {
		t.Fatal("future schema accepted")
	}
	if _, err := st.db.Exec(`UPDATE twilight_job_schema SET version=1`); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerOverviewLegacyProjectionDoesNotMutateState(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddSchedulerRunReturning(SchedulerRun{JobID: "legacy", Status: "running", StartedAt: 1, Summary: map[string]any{"kept": true}}); err != nil {
		t.Fatal(err)
	}
	view, err := st.ReadSchedulerHistory(context.Background(), "legacy", 1)
	if err != nil {
		t.Fatal(err)
	}
	if view.Runs["legacy"].Runs[0].Summary["interrupted"] != true {
		t.Fatal("missing interrupted projection")
	}
	original := st.SchedulerRuns("legacy", 1)[0]
	if original.Status != "running" || original.Summary["interrupted"] != nil {
		t.Fatal("read modified resident history")
	}
}
