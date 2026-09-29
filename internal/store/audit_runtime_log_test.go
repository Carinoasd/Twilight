package store

import (
	"context"
	"testing"
	"time"
)

// 第 8 条：另一个写者先拿到较小 id 但较晚提交时，追尾游标不能跳过它。
// 测试在自己的交易里模拟「进行中的 AddRuntimeLog」：先取同一把追加锁再插入、暂不提交。
func TestRuntimeLogTailDoesNotSkipLateCommit(t *testing.T) {
	st := newJSONStoreForTest(t)
	ctx := context.Background()
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(7_410_001)); err != nil {
		t.Fatal(err)
	}
	var slowID int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO twilight_runtime_logs (time, level, message, attrs) VALUES (1, 'info', 'slow', '{}'::jsonb) RETURNING id`).Scan(&slowID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.AddRuntimeLog(RuntimeLogEntry{Level: "info", Message: "fast", Time: 2}, 100)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	first, cursor := st.RuntimeLogs(100, 0)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	second, _ := st.RuntimeLogs(100, cursor)
	seen := map[string]bool{}
	for _, e := range append(first, second...) {
		seen[e.Message] = true
	}
	if !seen["slow"] || !seen["fast"] {
		t.Fatalf("tail missed a row: first=%+v cursor=%d second=%+v", first, cursor, second)
	}
}

// 第 8 条：还原（RESTART IDENTITY）后序列重来，旧的大游标不能让新日志永远不可见。
func TestRuntimeLogCursorRecoversAfterSequenceReset(t *testing.T) {
	st := newJSONStoreForTest(t)
	for i := 0; i < 5; i++ {
		if _, err := st.AddRuntimeLog(RuntimeLogEntry{Level: "info", Message: "old", Time: int64(i)}, 100); err != nil {
			t.Fatal(err)
		}
	}
	_, cursor := st.RuntimeLogs(100, 0)
	if cursor != 5 {
		t.Fatalf("cursor=%d", cursor)
	}
	if _, err := st.db.ExecContext(context.Background(), `TRUNCATE twilight_runtime_logs RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddRuntimeLog(RuntimeLogEntry{Level: "info", Message: "after-restore", Time: 9}, 100); err != nil {
		t.Fatal(err)
	}
	entries, next := st.RuntimeLogs(100, cursor)
	if len(entries) != 1 || entries[0].Message != "after-restore" || next != entries[0].ID {
		t.Fatalf("after reset: entries=%+v next=%d", entries, next)
	}
}
