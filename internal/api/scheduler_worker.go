package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/prejudice-studio/twilight/internal/security"
	"github.com/prejudice-studio/twilight/internal/store"
	"go.uber.org/zap"
)

type schedulerFrozenParamsKey struct{}

func (a *App) enqueueSchedulerJob(ctx context.Context, id string, supplied map[string]any, kind string) (store.SchedulerRun, bool, error) {
	if !schedulerJobExists(id) {
		return store.SchedulerRun{}, false, store.ErrNotFound
	}
	schedule, _ := a.store().SchedulerSchedule(id)
	params := a.schedulerRuntimeParamsFromSchedule(id, schedule.RuntimeParams)
	if params == nil {
		params = map[string]any{}
	}
	for k, v := range supplied {
		params[k] = v
	}
	// Keep only known, bounded job parameters, including manual preview flags.
	params = a.normalizeSchedulerRuntimeParams(id, params)
	if params == nil {
		params = map[string]any{}
	}
	for _, key := range []string{"dry_run", "delete", "preserve_tg_bound", "ignore_enabled_flag"} {
		if value, ok := supplied[key].(bool); ok {
			params[key] = value
		}
	}
	if value, ok := supplied["days"]; ok {
		params["days"] = clamp(int(numeric(value)), 1, 3650)
	}
	return a.store().EnqueueSchedulerRun(ctx, store.SchedulerRun{JobID: id, Type: kind, Trigger: map[bool]string{true: "scheduler", false: "manual"}[kind == "auto"], Params: params, ScheduleRevision: schedule.Revision}, nil)
}

func (a *App) claimSchedulerJobs(ctx context.Context) {
	for range store.SchedulerConcurrency {
		active := 0
		a.schedulerLocks.Range(func(_, _ any) bool { active++; return true })
		if active >= store.SchedulerConcurrency {
			return
		}
		owner, err := security.RandomHex(24)
		if err != nil {
			return
		}
		a.runtimeMu.Lock()
		st := a.store()
		run, claimed, err := st.ClaimSchedulerRun(ctx, owner)
		if err != nil {
			a.runtimeMu.Unlock()
			zap.L().Warn("scheduler claim failed")
			return
		}
		if !claimed {
			a.runtimeMu.Unlock()
			return
		}
		runCtx, process, finish, ok := a.startSchedulerRun(ctx, run.JobID)
		a.runtimeMu.Unlock()
		if !ok {
			run.Status = "interrupted"
			run.Message = "previous local execution has not stopped"
			run.FinishedAt = time.Now().Unix()
			run.EndedAt = run.FinishedAt
			_ = st.FinishSchedulerRun(context.Background(), run, owner)
			continue
		}
		process.runID.Store(run.ID)
		a.schedulerWorkers.Add(1)
		go func() {
			defer a.schedulerWorkers.Done()
			a.executeClaimedSchedulerRun(runCtx, process, st, run, finish)
		}()
	}
}

// Each execution reads one immutable runtime snapshot. Hot reload affects only
// subsequent runs; no secrets or full configuration are persisted in history.
func (a *App) schedulerExecutionApp() *App {
	execution := &App{telegramPanels: map[string]telegramPanelContext{}, developerJSCallbacks: map[string]developerJSCallbackContext{}, developerJSWaiters: map[string]developerJSMessageWaiter{}, embyAdminCache: map[string]embyAdminCacheEntry{}}
	execution.runtime.Store(a.runtimeSnapshot())
	return execution
}

func (a *App) executeClaimedSchedulerRun(ctx context.Context, process *schedulerProcessRun, st *store.Store, run store.SchedulerRun, finish func()) {
	defer finish()
	execution := a.schedulerExecutionApp()
	data, _ := json.Marshal(execution.cfg())
	digest := sha256.Sum256(data)
	run.ConfigRevision = hex.EncodeToString(digest[:])
	done := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				cancelRequested, err := st.HeartbeatSchedulerRun(context.Background(), run.ID, run.Owner)
				if err != nil {
					process.cancel()
					return
				}
				if cancelRequested {
					process.terminated.Store(true)
					process.cancel()
				}
			}
		}
	}()
	var summary map[string]any
	var logs []string
	var jobErr error
	defer func() {
		if value := recover(); value != nil {
			jobErr = errors.New("scheduler job panic")
		}
		close(done)
		<-heartbeatDone
		if jobErr == nil && ctx.Err() != nil {
			jobErr = ctx.Err()
		}
		result := schedulerFinishedRun(run.JobID, run.Type, run.Trigger, run.StartedAt, summary, logs, jobErr)
		result.ID = run.ID
		result.Params = run.Params
		result.ScheduleRevision = run.ScheduleRevision
		result.ConfigRevision = run.ConfigRevision
		if errors.Is(jobErr, context.Canceled) {
			result.Status = "interrupted"
			result.Message = "worker stopped"
			if process.terminated.Load() {
				result.Status = "cancelled"
				result.Message = "job cancelled"
			}
		}
		if err := st.FinishSchedulerRun(context.Background(), result, run.Owner); err != nil {
			zap.L().Warn("scheduler completion rejected or unavailable", zap.Int64("run_id", run.ID))
		}
	}()
	if err := st.RecordSchedulerConfigRevision(ctx, run.ID, run.Owner, run.ConfigRevision); err != nil {
		jobErr = err
		return
	}
	// Observe a cancellation racing the claim before starting any work.
	cancelRequested, err := st.HeartbeatSchedulerRun(ctx, run.ID, run.Owner)
	if err != nil {
		jobErr = err
		return
	}
	if cancelRequested {
		process.terminated.Store(true)
		jobErr = context.Canceled
		return
	}
	ctx = context.WithValue(ctx, schedulerFrozenParamsKey{}, run.Params)
	ctx = context.WithValue(ctx, schedulerManualContextKey, run.Type == "manual")
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/scheduler/internal", nil)
	summary, logs, jobErr = execution.runSchedulerJob(req, run.JobID)
}
