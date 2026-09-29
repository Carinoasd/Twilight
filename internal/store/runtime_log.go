package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"sync/atomic"
	"time"
)

// pgRuntimeLogPruneEvery 控制 PG 后端 runtime log 的 prune 节奏：
// 旧实现是每条 INSERT 后立即跑一次 `DELETE … WHERE id NOT IN (SELECT … LIMIT N)`
// 全表反向扫描，busy 期间会反向卡住所有 zap 调用方。改成每 N 条触发一次
// 后台 prune（异步、带自身 ctx），写入路径只做 INSERT。
const pgRuntimeLogPruneEvery = 256
const defaultRuntimeLogLimit = 1000

// runtimeLogAppendLockKey 是 runtime log 追加时串行化取号与提交的 advisory lock 键。
const runtimeLogAppendLockKey int64 = 7_410_001

const (
	pgRuntimeLogWriteTimeout = 5 * time.Second
	pgRuntimeLogReadTimeout  = 5 * time.Second
	pgRuntimeLogPruneTimeout = 10 * time.Second
)

const pgRuntimeLogPruneSQL = `
WITH keep AS (
	SELECT MIN(id) AS min_id
	FROM (
		SELECT id FROM twilight_runtime_logs ORDER BY id DESC LIMIT $1
	) latest
)
DELETE FROM twilight_runtime_logs
WHERE id < COALESCE((SELECT min_id FROM keep), 0)`

// pgRuntimeLogPruneCounter 用 atomic 共享自增计数；触发阈值时跳一次
// goroutine 异步 prune。pruneInFlight 互斥锁防止多 goroutine 同时跑同一个
// DELETE，避免在 burst 时叠加成 N 个并发全表扫描。
var (
	pgRuntimeLogPruneCounter atomic.Uint64
	pgRuntimeLogPruneGate    atomic.Bool
)

func (s *Store) AddRuntimeLog(entry RuntimeLogEntry, limit int) (RuntimeLogEntry, error) {
	if s == nil {
		return entry, ErrNotFound
	}
	limit = clampRuntimeLogLimit(limit)
	if entry.Time == 0 {
		entry.Time = time.Now().Unix()
	}
	attrs, err := json.Marshal(entry.Attrs)
	if err != nil {
		return entry, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgRuntimeLogWriteTimeout)
	defer cancel()
	var id int64
	// 追加时先取交易级 advisory lock：bigserial 的取号顺序与提交顺序不保证一致，
	// id 101 可能比 102 晚提交，而追尾读者用 `id > 游标` 前进，101 就永远读不到。
	// 锁在同一条语句内取得、提交时释放，nextval 发生在拿到锁之后，于是 id 顺序
	// 与提交顺序一致（跨进程同样成立）。代价是日志写入串行化，但每次只持有一条
	// 单行 INSERT 到提交的时间。
	err = s.db.QueryRowContext(
		ctx,
		`WITH append_lock AS (SELECT pg_advisory_xact_lock($5))
INSERT INTO twilight_runtime_logs (time, level, message, attrs)
SELECT $1, $2, $3, $4::jsonb FROM append_lock
RETURNING id`,
		entry.Time,
		entry.Level,
		entry.Message,
		string(attrs),
		runtimeLogAppendLockKey,
	).Scan(&id)
	if err != nil {
		return entry, err
	}
	entry.ID = id
	s.maybeAsyncPrunePGRuntimeLogs(limit)
	return entry, nil
}

// maybeAsyncPrunePGRuntimeLogs 每 pgRuntimeLogPruneEvery 条 INSERT 触发一次
// 后台 prune；写入路径不再阻塞在 DELETE 上。pgRuntimeLogPruneGate 保证同一
// 时刻只有一个 prune goroutine 在跑，避免 burst 时叠加并发全表 DELETE。
func (s *Store) maybeAsyncPrunePGRuntimeLogs(limit int) {
	if s == nil {
		return
	}
	if pgRuntimeLogPruneCounter.Add(1)%pgRuntimeLogPruneEvery != 0 {
		return
	}
	if !pgRuntimeLogPruneGate.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer pgRuntimeLogPruneGate.Store(false)
		defer func() {
			// 异步 goroutine 入口加 recover：prune SQL 异常不能反向拖垮调用 zap.Info 的协程。
			_ = recover()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), pgRuntimeLogPruneTimeout)
		defer cancel()
		_, _ = s.db.ExecContext(ctx, pgRuntimeLogPruneSQL, clampRuntimeLogLimit(limit))
	}()
}

func (s *Store) RuntimeLogs(limit int, after int64) ([]RuntimeLogEntry, int64) {
	if s == nil {
		return nil, after
	}
	return s.postgresRuntimeLogs(limit, after)
}

func runtimeLogWindow(entries []RuntimeLogEntry, limit int, after int64) (int, int) {
	if len(entries) == 0 || limit <= 0 {
		return 0, 0
	}
	if limit > len(entries) {
		limit = len(entries)
	}
	if after <= 0 {
		start := len(entries) - limit
		if start < 0 {
			start = 0
		}
		return start, len(entries)
	}
	start := sort.Search(len(entries), func(i int) bool {
		return entries[i].ID > after
	})
	if start >= len(entries) {
		return len(entries), len(entries)
	}
	end := start + limit
	if end > len(entries) {
		end = len(entries)
	}
	return start, end
}

func (s *Store) RuntimeLogStats() (int64, int) {
	if s == nil {
		return 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgRuntimeLogReadTimeout)
	defer cancel()
	var next sql.NullInt64
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT max(id), count(*) FROM twilight_runtime_logs`).Scan(&next, &count); err != nil {
		return 0, 0
	}
	return next.Int64, count
}

func (s *Store) PruneRuntimeLogs(limit int) error {
	if s == nil {
		return nil
	}
	limit = clampRuntimeLogLimit(limit)
	ctx, cancel := context.WithTimeout(context.Background(), pgRuntimeLogPruneTimeout)
	defer cancel()
	_, err := s.db.ExecContext(ctx, pgRuntimeLogPruneSQL, limit)
	return err
}

func (s *Store) postgresRuntimeLogs(limit int, after int64) ([]RuntimeLogEntry, int64) {
	limit = clampRuntimeLogReadLimit(limit)
	var (
		rows *sql.Rows
		err  error
	)
	ctx, cancel := context.WithTimeout(context.Background(), pgRuntimeLogReadTimeout)
	defer cancel()
	if after > 0 {
		// 游标比表内最大 id 还大，只可能是还原/匯入以 RESTART IDENTITY 重置了序列：
		// 旧游标已失效，按「首次读取」返回最新一页，并以新序列的 id 作为游标，
		// 否则要等新 id 追上旧值才看得到新日志。
		if maxID := s.postgresRuntimeLogMaxID(); after > maxID {
			after = 0
		}
	}
	if after > 0 {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, time, level, message, COALESCE(attrs, '{}'::jsonb)::text
FROM twilight_runtime_logs
WHERE id > $1
ORDER BY id ASC
LIMIT $2`, after, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, time, level, message, COALESCE(attrs, '{}'::jsonb)::text
FROM twilight_runtime_logs
ORDER BY id DESC
LIMIT $1`, limit)
	}
	if err != nil {
		return nil, after
	}
	defer rows.Close()
	out := []RuntimeLogEntry{}
	for rows.Next() {
		var entry RuntimeLogEntry
		var attrsText string
		if err := rows.Scan(&entry.ID, &entry.Time, &entry.Level, &entry.Message, &attrsText); err != nil {
			continue
		}
		if attrsText != "" {
			_ = json.Unmarshal([]byte(attrsText), &entry.Attrs)
		}
		out = append(out, entry)
	}
	if after <= 0 {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	next := after
	if len(out) > 0 {
		next = out[len(out)-1].ID
	} else if maxID := s.postgresRuntimeLogMaxID(); maxID > next {
		next = maxID
	}
	return out, next
}

func (s *Store) postgresRuntimeLogMaxID() int64 {
	ctx, cancel := context.WithTimeout(context.Background(), pgRuntimeLogReadTimeout)
	defer cancel()
	var next sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(id) FROM twilight_runtime_logs`).Scan(&next); err != nil {
		return 0
	}
	return next.Int64
}

func clampRuntimeLogReadLimit(limit int) int {
	if limit <= 0 {
		return 200
	}
	if limit > 50000 {
		return 50000
	}
	return limit
}

func clampRuntimeLogLimit(limit int) int {
	if limit < 100 {
		return 100
	}
	if limit > 50000 {
		return 50000
	}
	return limit
}
