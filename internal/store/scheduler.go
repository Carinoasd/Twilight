package store

import (
	"fmt"
	"time"
)

const maxStoredSchedulerRuns = 200

type SchedulerRunSnapshot struct {
	Runs             []SchedulerRun
	LatestAuto       SchedulerRun
	HasLatestAuto    bool
	LatestManual     SchedulerRun
	HasLatestManual  bool
	LatestRunning    SchedulerRun
	HasLatestRunning bool
}

type SchedulerOverview struct {
	Runs        map[string]SchedulerRunSnapshot
	Schedules   map[string]SchedulerSchedule
	Interrupted int
}

func (s *Store) AddSchedulerRun(run SchedulerRun) error {
	_, err := s.AddSchedulerRunReturning(run)
	return err
}

func (s *Store) AddSchedulerRunReturning(run SchedulerRun) (SchedulerRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 走 mutateAndSaveLocked：失败回滚、冲突重放。input 保持原值，每次重放都以
	// 调用方传入的 run 为起点重新分配 ID，避免沿用上一轮分配出去的 ID。
	input := run
	err := s.mutateAndSaveLocked(func() error {
		run = input
		if run.ID == 0 {
			run.ID = s.state.NextSchedulerRunID
			s.state.NextSchedulerRunID++
		}
		if run.Type == "" {
			run.Type = "manual"
		}
		if run.Trigger == "" {
			run.Trigger = "manual"
		}
		normalizeSchedulerRunTimestamps(&run)
		s.state.SchedulerRuns = prependBoundedHead(s.state.SchedulerRuns, run, maxStoredSchedulerRuns)
		return nil
	})
	if err != nil {
		return SchedulerRun{}, err
	}
	return cloneSchedulerRun(run), nil
}

// TryStartSchedulerRun persists a running row only when the same job has no
// fresh persisted run. The check and insert share one store critical section,
// so API-triggered and daemon-triggered runs cannot both pass through a stale
// in-memory snapshot in the same process. Persisted rows also provide a guard
// between API and scheduler processes that share the state backend.
func (s *Store) TryStartSchedulerRun(run SchedulerRun, staleBeforeUnix, nowUnix int64) (SchedulerRun, bool, error) {
	if run.JobID == "" {
		return SchedulerRun{}, false, fmt.Errorf("scheduler job id is empty")
	}
	if nowUnix <= 0 {
		nowUnix = time.Now().Unix()
	}
	if run.StartedAt <= 0 {
		run.StartedAt = nowUnix
	}
	if run.Status == "" {
		run.Status = "running"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// 检查与插入放进同一个 mutate 闭包：版本冲突重放时会基于他进程的最新写重新检查，
	// 旧实现冲突即直接报错，也不会重新判断是否已有新鲜的 running 行。
	input := run
	started := false
	err := s.mutateAndSaveLocked(func() error {
		run, started = input, false
		for _, current := range s.state.SchedulerRuns {
			if current.JobID == run.JobID && current.Status == "running" && current.StartedAt > staleBeforeUnix {
				return errNoChange
			}
		}
		for i := range s.state.SchedulerRuns {
			current := &s.state.SchedulerRuns[i]
			if current.JobID == run.JobID && current.Status == "running" && current.StartedAt <= staleBeforeUnix {
				markSchedulerRunInterrupted(current, nowUnix)
			}
		}
		if run.ID == 0 {
			run.ID = s.state.NextSchedulerRunID
			s.state.NextSchedulerRunID++
		}
		if run.Type == "" {
			run.Type = "manual"
		}
		if run.Trigger == "" {
			run.Trigger = "manual"
		}
		normalizeSchedulerRunTimestamps(&run)
		s.state.SchedulerRuns = prependBoundedHead(s.state.SchedulerRuns, run, maxStoredSchedulerRuns)
		started = true
		return nil
	})
	if err != nil || !started {
		return SchedulerRun{}, false, err
	}
	return cloneSchedulerRun(run), true, nil
}

func (s *Store) UpdateSchedulerRun(id int64, fn func(*SchedulerRun) error) (SchedulerRun, error) {
	if id == 0 {
		return SchedulerRun{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 走 mutateAndSaveLocked：存档失败回滚内存，冲突时基于最新 run 重放 fn。
	var result SchedulerRun
	err := s.mutateAndSaveLocked(func() error {
		result = SchedulerRun{}
		for i := range s.state.SchedulerRuns {
			if s.state.SchedulerRuns[i].ID != id {
				continue
			}
			// fn 可能写 Summary/Params 的键：先深拷贝，旧 map 可能正被锁外读者持有。
			run := cloneSchedulerRun(s.state.SchedulerRuns[i])
			if err := fn(&run); err != nil {
				return err
			}
			normalizeSchedulerRunTimestamps(&run)
			s.state.SchedulerRuns[i] = run
			result = run
			return nil
		}
		return ErrNotFound
	})
	if err != nil {
		return SchedulerRun{}, err
	}
	return cloneSchedulerRun(result), nil
}

func (s *Store) SchedulerRuns(jobID string, limit int) []SchedulerRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	out := make([]SchedulerRun, 0, limit)
	for _, run := range s.state.SchedulerRuns {
		if jobID == "" || run.JobID == jobID {
			// 深拷贝 Params/Summary/Logs：锁外编码不能与写者共用 map。
			out = append(out, cloneSchedulerRun(run))
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func (s *Store) SchedulerRunSnapshot(jobID string, limit int) SchedulerRunSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	snapshot := SchedulerRunSnapshot{Runs: make([]SchedulerRun, 0, limit)}
	for _, run := range s.state.SchedulerRuns {
		if jobID != "" && run.JobID != jobID {
			continue
		}
		run = cloneSchedulerRun(run) // 回传副本不与 s.state 共用 map/slice
		if len(snapshot.Runs) < limit {
			snapshot.Runs = append(snapshot.Runs, run)
		}
		switch run.Type {
		case "auto":
			if !snapshot.HasLatestAuto || run.StartedAt > snapshot.LatestAuto.StartedAt {
				snapshot.LatestAuto = run
				snapshot.HasLatestAuto = true
			}
		case "manual":
			if !snapshot.HasLatestManual || run.StartedAt > snapshot.LatestManual.StartedAt {
				snapshot.LatestManual = run
				snapshot.HasLatestManual = true
			}
		}
		if run.Status == "running" && (!snapshot.HasLatestRunning || run.StartedAt > snapshot.LatestRunning.StartedAt) {
			snapshot.LatestRunning = run
			snapshot.HasLatestRunning = true
		}
	}
	return snapshot
}

// BatchSchedulerRunSnapshots 一次性获取多个 jobID 的 snapshot，只加一次读锁。
// handleSchedulerJobs 之前对每个 job 单独调用 SchedulerRunSnapshot → 13 个 job
// 就要 13 次 RLock/RUnlock + 13 次全量遍历 SchedulerRuns 切片。改为单次遍历
// 同时按 jobID 分桶，O(N) 代替 O(N*M)。
func (s *Store) BatchSchedulerRunSnapshots(jobIDs []string, limit int) map[string]SchedulerRunSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return schedulerRunSnapshots(s.state.SchedulerRuns, jobIDs, limit)
}

func schedulerRunSnapshots(runs []SchedulerRun, jobIDs []string, limit int) map[string]SchedulerRunSnapshot {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	result := make(map[string]SchedulerRunSnapshot, len(jobIDs))
	needed := make(map[string]bool, len(jobIDs))
	for _, id := range jobIDs {
		needed[id] = true
		result[id] = SchedulerRunSnapshot{Runs: make([]SchedulerRun, 0, limit)}
	}
	for _, run := range runs {
		if !needed[run.JobID] {
			continue
		}
		run = cloneSchedulerRun(run) // 回传副本不与 s.state 共用 map/slice
		snap := result[run.JobID]
		if len(snap.Runs) < limit {
			snap.Runs = append(snap.Runs, run)
		}
		switch run.Type {
		case "auto":
			if !snap.HasLatestAuto || schedulerRunTime(run) > schedulerRunTime(snap.LatestAuto) {
				snap.LatestAuto = run
				snap.HasLatestAuto = true
			}
		case "manual":
			if !snap.HasLatestManual || schedulerRunTime(run) > schedulerRunTime(snap.LatestManual) {
				snap.LatestManual = run
				snap.HasLatestManual = true
			}
		}
		if SchedulerRunActive(run.Status) && (!snap.HasLatestRunning || run.StartedAt > snap.LatestRunning.StartedAt) {
			snap.LatestRunning = run
			snap.HasLatestRunning = true
		}
		result[run.JobID] = snap
	}
	return result
}

// SchedulerStateOverview refreshes shared state once, reconciles stale running
// rows in one write, and returns run/schedule snapshots under the same lock.
// This is the polling and daemon hot path: it avoids one refresh and full scan
// per job while still observing updates written by another process.
func (s *Store) SchedulerStateOverview(jobIDs []string, limit int, activeJobIDs map[string]bool, staleBeforeUnix, nowUnix int64) (SchedulerOverview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	needed := make(map[string]bool, len(jobIDs))
	for _, jobID := range jobIDs {
		if jobID != "" {
			needed[jobID] = true
		}
	}
	// 标记中断走 mutateAndSaveLocked：失败回滚、冲突重放；没有过期 running 行时
	// errNoChange 跳过落盘（refresh 仍由 helper 完成，读到他进程的最新写）。
	interrupted := 0
	err := s.mutateAndSaveLocked(func() error {
		interrupted = 0
		for i, run := range s.state.SchedulerRuns {
			if !needed[run.JobID] || activeJobIDs[run.JobID] || run.Status != "running" || run.StartedAt > staleBeforeUnix {
				continue
			}
			markSchedulerRunInterrupted(&s.state.SchedulerRuns[i], nowUnix)
			interrupted++
		}
		if interrupted == 0 {
			return errNoChange
		}
		return nil
	})
	if err != nil {
		return SchedulerOverview{}, err
	}

	overview := SchedulerOverview{
		Runs:        schedulerRunSnapshots(s.state.SchedulerRuns, jobIDs, limit),
		Schedules:   make(map[string]SchedulerSchedule, len(jobIDs)),
		Interrupted: interrupted,
	}
	for _, jobID := range jobIDs {
		if schedule, ok := s.state.SchedulerSchedules[jobID]; ok {
			overview.Schedules[jobID] = cloneSchedulerSchedule(schedule)
		}
	}
	return overview, nil
}

func (s *Store) SchedulerSchedules(jobIDs []string) map[string]SchedulerSchedule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]SchedulerSchedule, len(jobIDs))
	for _, jobID := range jobIDs {
		if schedule, ok := s.state.SchedulerSchedules[jobID]; ok {
			out[jobID] = cloneSchedulerSchedule(schedule)
		}
	}
	return out
}

func cloneSchedulerSchedule(schedule SchedulerSchedule) SchedulerSchedule {
	schedule.TriggerSpec = cloneSchedulerMap(schedule.TriggerSpec)
	schedule.RuntimeParams = cloneSchedulerMap(schedule.RuntimeParams)
	return schedule
}

func cloneSchedulerMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// LastSchedulerRunByType 返回 (jobID, runType) 命中的最新一条 SchedulerRun（按
// StartedAt 取大者，处理乱序记录）。该方法**不**走 SchedulerRuns 的 20 条窗
// 口——cron_daily 任务的"今天是否已经自动跑过"判定必须基于完整历史，否则
// admin 在 30 分钟内对同 job 手动重跑 21 次就能挤掉当天的 auto 记录，导致
// schedulerJobDue 误判 last=0、再次启动一次 auto run。
//
// runType 留空时按 jobID 取最新一条；否则只挑 run.Type==runType 的命中。
// 没有匹配时返回 zero value 与 false。
func (s *Store) LastSchedulerRunByType(jobID, runType string) (SchedulerRun, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var (
		best  SchedulerRun
		found bool
	)
	for _, run := range s.state.SchedulerRuns {
		if jobID != "" && run.JobID != jobID {
			continue
		}
		if runType != "" && run.Type != runType {
			continue
		}
		if !found || run.StartedAt > best.StartedAt {
			best = run
			found = true
		}
	}
	return cloneSchedulerRun(best), found
}

func (s *Store) SetSchedulerSchedule(jobID string, spec map[string]any, custom bool) (SchedulerSchedule, error) {
	return s.SetSchedulerScheduleWithParams(jobID, spec, nil, custom)
}

func (s *Store) SetSchedulerScheduleWithParams(jobID string, spec map[string]any, params map[string]any, custom bool) (SchedulerSchedule, error) {
	return s.SetSchedulerScheduleRevision(jobID, spec, params, custom, nil)
}

// Resets keep a revision tombstone so a stale editor cannot recreate an override.
func (s *Store) SetSchedulerScheduleRevision(jobID string, spec, params map[string]any, custom bool, expected *int64) (SchedulerSchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result SchedulerSchedule
	err := s.mutateAndSaveLocked(func() error {
		// 冲突重放时重置闭包外的结果变量，避免沿用上一轮的值或重复累加。
		result = SchedulerSchedule{}
		previous := s.state.SchedulerSchedules[jobID]
		if expected != nil && previous.Revision != *expected {
			return ErrConflict
		}
		result = SchedulerSchedule{JobID: jobID, TriggerSpec: cloneSchedulerMap(spec), RuntimeParams: cloneSchedulerMap(params), IsCustom: custom, UpdatedAt: time.Now().Unix(), Revision: previous.Revision + 1}
		s.state.SchedulerSchedules[jobID] = result
		return nil
	})
	return result, err
}

func (s *Store) MarkInterruptedSchedulerRuns(jobID string, beforeUnix int64, nowUnix int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := 0
	err := s.mutateAndSaveLocked(func() error {
		changed = 0 // 冲突重放时重新计数
		for i := range s.state.SchedulerRuns {
			run := &s.state.SchedulerRuns[i]
			if jobID != "" && run.JobID != jobID {
				continue
			}
			if run.Status != "running" || run.StartedAt > beforeUnix {
				continue
			}
			markSchedulerRunInterrupted(run, nowUnix)
			changed++
		}
		if changed == 0 {
			return errNoChange
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}

func markSchedulerRunInterrupted(run *SchedulerRun, nowUnix int64) {
	run.Status = "failed"
	run.Message = "job interrupted before completion"
	run.Error = "job interrupted before completion"
	run.FinishedAt = nowUnix
	run.EndedAt = nowUnix
	// 先 clone 再写：旧 Summary map 可能已交给锁外读者（SchedulerRuns 等的副本、
	// 未走 clone 的 legacy 读路径），就地写键会触发 concurrent map read/write fatal。
	summary := cloneSchedulerMap(run.Summary)
	if summary == nil {
		summary = map[string]any{}
	}
	summary["interrupted"] = true
	summary["success"] = false
	run.Summary = summary
}

func (s *Store) SchedulerSchedule(jobID string) (SchedulerSchedule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	schedule, ok := s.state.SchedulerSchedules[jobID]
	return cloneSchedulerSchedule(schedule), ok && schedule.IsCustom
}

func normalizeSchedulerRunTimestamps(run *SchedulerRun) {
	if run.FinishedAt == 0 && run.EndedAt != 0 {
		run.FinishedAt = run.EndedAt
	}
}

func schedulerRunTime(run SchedulerRun) int64 {
	if run.StartedAt > 0 {
		return run.StartedAt
	}
	return run.CreatedAt
}
