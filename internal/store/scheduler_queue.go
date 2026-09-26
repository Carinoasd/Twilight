package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const schedulerQueueLock = 724193821
const SchedulerLeaseSeconds = 30
const SchedulerConcurrency = 4

// Runtime commands are independent of the large business-state JSON document.
// Legacy history stays readable; snapshots fold the new rows into the old shape.
func prepareSchedulerQueueSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schedulerQueueLock); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS twilight_job_schema (id smallint PRIMARY KEY CHECK(id=1), version integer NOT NULL);
INSERT INTO twilight_job_schema VALUES (1,1) ON CONFLICT DO NOTHING;
CREATE SEQUENCE IF NOT EXISTS twilight_job_run_id;
CREATE TABLE IF NOT EXISTS twilight_job_runs (
 id bigint PRIMARY KEY DEFAULT nextval('twilight_job_run_id'), job_id text NOT NULL,
 status text NOT NULL CHECK(status IN ('queued','running','cancel_requested','success','failed','cancelled','interrupted')),
 owner text NOT NULL DEFAULT '', lease_until bigint NOT NULL DEFAULT 0,
 heartbeat_at bigint NOT NULL DEFAULT 0, created_at bigint NOT NULL, payload jsonb NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS twilight_job_one_active ON twilight_job_runs(job_id) WHERE status IN ('queued','running','cancel_requested');
CREATE INDEX IF NOT EXISTS twilight_job_history ON twilight_job_runs(job_id,id DESC);
`); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT version FROM twilight_job_schema WHERE id=1`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("unsupported scheduler schema version %d", version)
	}
	return tx.Commit()
}

func SchedulerRunActive(status string) bool {
	return status == "queued" || status == "running" || status == "cancel_requested"
}

func scanQueuedRun(scan func(...any) error) (SchedulerRun, error) {
	var run SchedulerRun
	var data []byte
	var id, created, lease, heartbeat int64
	var status, owner string
	if err := scan(&id, &status, &owner, &lease, &heartbeat, &created, &data); err != nil {
		return run, err
	}
	if err := json.Unmarshal(data, &run); err != nil {
		return run, err
	}
	run.ID, run.Status, run.Owner, run.LeaseUntil, run.HeartbeatAt, run.CreatedAt = id, status, owner, lease, heartbeat, created
	return run, nil
}

const queuedRunColumns = `id,status,owner,lease_until,heartbeat_at,created_at,payload`

type schedulerQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func schedulerQueueRows(ctx context.Context, db schedulerQueryer) ([]SchedulerRun, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+queuedRunColumns+` FROM twilight_job_runs ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SchedulerRun
	for rows.Next() {
		run, err := scanQueuedRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, run)
	}
	return result, rows.Err()
}

func mergeSchedulerHistory(legacy, current []SchedulerRun) []SchedulerRun {
	out := make([]SchedulerRun, 0, len(legacy)+len(current))
	out = append(out, current...)
	seen := make(map[int64]bool, len(current))
	for _, r := range current {
		seen[r.ID] = true
	}
	for _, r := range legacy {
		if !seen[r.ID] {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func (s *Store) ReadSchedulerOverview(ctx context.Context, jobIDs []string, limit int) (SchedulerOverview, error) {
	return s.readSchedulerOverview(ctx, jobIDs, limit, false)
}

func (s *Store) ReadSchedulerHistory(ctx context.Context, jobID string, limit int) (SchedulerOverview, error) {
	return s.readSchedulerOverview(ctx, []string{jobID}, limit, true)
}

func (s *Store) readSchedulerOverview(ctx context.Context, jobIDs []string, limit int, includeLogs bool) (SchedulerOverview, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if limit < 1 || limit > 100 {
		limit = 20
	}
	payload := "payload - 'logs'"
	if includeLogs {
		payload = "payload"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,status,owner,lease_until,heartbeat_at,created_at,`+payload+` FROM (
SELECT *, row_number() OVER(PARTITION BY job_id ORDER BY id DESC) recent,
row_number() OVER(PARTITION BY job_id,payload->>'type' ORDER BY id DESC) latest_type
FROM twilight_job_runs WHERE job_id=ANY($1)) history
WHERE recent <= $2 OR latest_type=1 OR status IN ('queued','running','cancel_requested') ORDER BY id DESC`, jobIDs, limit)
	if err != nil {
		return SchedulerOverview{}, err
	}
	defer rows.Close()
	var runs []SchedulerRun
	for rows.Next() {
		run, err := scanQueuedRun(rows.Scan)
		if err != nil {
			return SchedulerOverview{}, err
		}
		runs = append(runs, run)
	}
	if err = rows.Err(); err != nil {
		return SchedulerOverview{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	runs = mergeSchedulerHistory(s.state.SchedulerRuns, runs)
	for i := range runs {
		// Legacy records have no lease. This is a read projection only; new
		// leased work never expires based on its start time.
		if runs[i].LeaseUntil == 0 && runs[i].Status == "running" && runs[i].StartedAt < time.Now().Unix()-1800 {
			runs[i].Summary = cloneSchedulerMap(runs[i].Summary)
			markSchedulerRunInterrupted(&runs[i], time.Now().Unix())
		}
	}
	return SchedulerOverview{Runs: schedulerRunSnapshots(runs, jobIDs, limit), Schedules: schedulerSchedules(s.state.SchedulerSchedules, jobIDs)}, nil
}

func schedulerSchedules(all map[string]SchedulerSchedule, ids []string) map[string]SchedulerSchedule {
	out := make(map[string]SchedulerSchedule, len(ids))
	for _, id := range ids {
		if schedule, ok := all[id]; ok {
			out[id] = cloneSchedulerSchedule(schedule)
		}
	}
	return out
}

// EnqueueSchedulerRun is serialized with claim/recovery. An automatic launch
// must still refer to the last automatic run it inspected, preventing two
// schedulers from enqueueing the same due occurrence even if the first finished.
func (s *Store) EnqueueSchedulerRun(ctx context.Context, run SchedulerRun, expectedAutoID *int64) (SchedulerRun, bool, error) {
	if run.JobID == "" || len(run.JobID) > 80 {
		return run, false, ErrInvalid
	}
	params, marshalErr := json.Marshal(run.Params)
	if marshalErr != nil || len(params) > 65536 {
		return run, false, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return run, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schedulerQueueLock); err != nil {
		return run, false, err
	}
	var now int64
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT extract(epoch FROM clock_timestamp())::bigint, jsonb_build_object('scheduler_runs',state->'scheduler_runs','scheduler_schedules',state->'scheduler_schedules') FROM twilight_state WHERE id=1`).Scan(&now, &raw); err != nil {
		return run, false, err
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		return run, false, err
	}
	// Queued cancellations never pass through Finish. Prune here as well so
	// repeated submit/cancel cycles cannot permanently exhaust queue capacity.
	if _, err = tx.ExecContext(ctx, pruneSchedulerHistorySQL); err != nil {
		return run, false, err
	}
	// Capacity and scheduling decisions need metadata only, never old logs.
	rows, err := tx.QueryContext(ctx, `SELECT id,status,owner,lease_until,heartbeat_at,created_at,payload - 'logs' - 'summary' FROM twilight_job_runs`)
	if err != nil {
		return run, false, err
	}
	var current []SchedulerRun
	for rows.Next() {
		item, readErr := scanQueuedRun(rows.Scan)
		if readErr != nil {
			rows.Close()
			return run, false, readErr
		}
		current = append(current, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return run, false, err
	}
	all := mergeSchedulerHistory(state.SchedulerRuns, current)
	var latestAuto, maxID int64
	for _, r := range all {
		if r.ID > maxID {
			maxID = r.ID
		}
		if r.JobID != run.JobID {
			continue
		}
		if SchedulerRunActive(r.Status) && (r.LeaseUntil > 0 || r.Status == "queued" || r.StartedAt > now-1800) {
			return r, false, nil
		}
		if r.Type == "auto" && r.ID > latestAuto {
			latestAuto = r.ID
		}
	}
	if expectedAutoID != nil && *expectedAutoID != latestAuto {
		return run, false, nil
	}
	if expectedAutoID != nil && state.SchedulerSchedules[run.JobID].Revision != run.ScheduleRevision {
		return run, false, nil
	}
	if len(current) >= 4000 {
		return run, false, ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `SELECT setval('twilight_job_run_id', GREATEST((SELECT last_value FROM twilight_job_run_id),$1),true)`, maxID); err != nil {
		return run, false, err
	}
	run.Status = "queued"
	run.Message = "job queued"
	run.CreatedAt = now
	run.StartedAt = 0
	if run.Type == "" {
		run.Type = "manual"
	}
	if run.Trigger == "" {
		run.Trigger = run.Type
	}
	data, err := json.Marshal(run)
	if err != nil {
		return run, false, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO twilight_job_runs(job_id,status,created_at,payload) VALUES($1,'queued',$2,$3) RETURNING id`, run.JobID, now, data).Scan(&run.ID)
	if err != nil {
		return run, false, err
	}
	err = tx.Commit()
	return run, err == nil, err
}

// Expired work is interrupted, never automatically retried: many jobs send
// notifications or modify remote accounts and cannot safely replay blindly.
func (s *Store) ClaimSchedulerRun(ctx context.Context, owner string) (SchedulerRun, bool, error) {
	if owner == "" {
		return SchedulerRun{}, false, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SchedulerRun{}, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schedulerQueueLock); err != nil {
		return SchedulerRun{}, false, err
	}
	var now int64
	if err = tx.QueryRowContext(ctx, `SELECT extract(epoch FROM clock_timestamp())::bigint`).Scan(&now); err != nil {
		return SchedulerRun{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE twilight_job_runs SET status='interrupted', payload=payload || jsonb_build_object('message','worker lease expired; review before rerunning','finished_at',$1::bigint,'ended_at',$1::bigint,'summary',jsonb_build_object('interrupted',true,'success',false)) WHERE status IN ('running','cancel_requested') AND lease_until <= $1`, now); err != nil {
		return SchedulerRun{}, false, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM twilight_job_runs WHERE status IN ('running','cancel_requested')`).Scan(&active); err != nil {
		return SchedulerRun{}, false, err
	}
	if active >= SchedulerConcurrency {
		return SchedulerRun{}, false, tx.Commit()
	}
	run, err := scanQueuedRun(tx.QueryRowContext(ctx, `SELECT `+queuedRunColumns+` FROM twilight_job_runs WHERE status='queued' ORDER BY id LIMIT 1 FOR UPDATE`).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SchedulerRun{}, false, tx.Commit()
	}
	if err != nil {
		return run, false, err
	}
	run.StartedAt = now
	run.Message = "running"
	run.Status = "running"
	run.Owner = owner
	run.LeaseUntil = now + SchedulerLeaseSeconds
	run.HeartbeatAt = now
	data, err := json.Marshal(run)
	if err != nil {
		return run, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE twilight_job_runs SET status='running',owner=$2,heartbeat_at=$3,lease_until=$4,payload=$5 WHERE id=$1`, run.ID, owner, now, run.LeaseUntil, data); err != nil {
		return run, false, err
	}
	err = tx.Commit()
	return run, err == nil, err
}

// Cancellation remains an active state until the owner acknowledges completion.
func (s *Store) RequestSchedulerCancellation(ctx context.Context, jobID string) (SchedulerRun, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run, err := scanQueuedRun(s.db.QueryRowContext(ctx, `UPDATE twilight_job_runs SET status=CASE WHEN status='queued' THEN 'cancelled' ELSE 'cancel_requested' END,
payload=payload || CASE WHEN status='queued' THEN jsonb_build_object('message','job cancelled before execution','finished_at',extract(epoch FROM clock_timestamp())::bigint,'ended_at',extract(epoch FROM clock_timestamp())::bigint) ELSE jsonb_build_object('message','cancellation requested') END
WHERE job_id=$1 AND status IN ('queued','running','cancel_requested') RETURNING `+queuedRunColumns, jobID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return SchedulerRun{}, false, nil
	}
	return run, err == nil, err
}

func (s *Store) HeartbeatSchedulerRun(ctx context.Context, id int64, owner string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var status string
	err := s.db.QueryRowContext(ctx, `UPDATE twilight_job_runs SET heartbeat_at=extract(epoch FROM clock_timestamp())::bigint,lease_until=extract(epoch FROM clock_timestamp())::bigint+$3 WHERE id=$1 AND owner=$2 AND status IN ('running','cancel_requested') AND lease_until>extract(epoch FROM clock_timestamp())::bigint RETURNING status`, id, owner, SchedulerLeaseSeconds).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrConflict
	}
	return status == "cancel_requested", err
}

// Persist the effective configuration fingerprint before invoking side effects,
// including for runs whose process crashes before it can write completion.
func (s *Store) RecordSchedulerConfigRevision(ctx context.Context, id int64, owner, revision string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `UPDATE twilight_job_runs SET payload=jsonb_set(payload,'{config_revision}',to_jsonb($3::text)) WHERE id=$1 AND owner=$2 AND status IN ('running','cancel_requested') AND lease_until>extract(epoch FROM clock_timestamp())::bigint`, id, owner, revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrConflict
	}
	return err
}

// Retain the newest 100 rows per job plus its latest automatic watermark.
// Active commands are never removed, even when restoring historical data.
const pruneSchedulerHistorySQL = `DELETE FROM twilight_job_runs WHERE id IN (
SELECT id FROM (
 SELECT id,status,row_number() OVER(PARTITION BY job_id ORDER BY id DESC) recent,
 max(id) FILTER(WHERE payload->>'type'='auto') OVER(PARTITION BY job_id) latest_auto
 FROM twilight_job_runs
) history WHERE recent>100 AND status NOT IN ('queued','running','cancel_requested') AND id<>COALESCE(latest_auto,0))`

func (s *Store) FinishSchedulerRun(ctx context.Context, run SchedulerRun, owner string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if run.Status != "success" && run.Status != "failed" && run.Status != "cancelled" && run.Status != "interrupted" {
		return ErrInvalid
	}
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE twilight_job_runs SET status=CASE WHEN status='cancel_requested' THEN 'cancelled' ELSE $3 END,
payload=$4::jsonb || CASE WHEN status='cancel_requested' THEN jsonb_build_object('message','job cancelled') ELSE '{}'::jsonb END
WHERE id=$1 AND owner=$2 AND status IN ('running','cancel_requested') AND lease_until>extract(epoch FROM clock_timestamp())::bigint`, run.ID, owner, run.Status, data)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	// Keep a bounded history per job, retaining the latest automatic record so
	// manual activity cannot erase the automatic scheduling watermark.
	_, err = s.db.ExecContext(ctx, `DELETE FROM twilight_job_runs WHERE job_id=$1 AND status NOT IN ('queued','running','cancel_requested') AND id NOT IN (SELECT id FROM twilight_job_runs WHERE job_id=$1 ORDER BY id DESC LIMIT 100) AND id <> COALESCE((SELECT max(id) FROM twilight_job_runs WHERE job_id=$1 AND payload->>'type'='auto'),0)`, run.JobID)
	return err
}

// A restore never resumes archived commands. Refuse to replace a live worker's
// history; deployments must drain workers before restoring business state.
func resetSchedulerQueueTx(ctx context.Context, tx *sql.Tx, state *State) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, schedulerQueueLock); err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM twilight_job_runs WHERE status IN ('running','cancel_requested') AND lease_until>extract(epoch FROM clock_timestamp())::bigint)`).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrConflict
	}
	for i := range state.SchedulerRuns {
		if SchedulerRunActive(state.SchedulerRuns[i].Status) {
			markSchedulerRunInterrupted(&state.SchedulerRuns[i], time.Now().Unix())
			state.SchedulerRuns[i].Status = "interrupted"
		}
		state.SchedulerRuns[i].Owner = ""
		state.SchedulerRuns[i].LeaseUntil = 0
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM twilight_job_runs`)
	return err
}
