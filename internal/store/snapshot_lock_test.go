package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Snapshot（备份 / 还原预览 / 迁移）在读取大专表期间不得持有 Store 锁，
// 否则整个进程的请求都会排队。用另一连接对 runtime_logs 加排他锁让 Snapshot
// 卡在专表读取上，此时普通读操作必须仍能立即返回。
func TestSnapshotDoesNotBlockStoreWhileReadingSideTables(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateUser(User{Username: "alice", Active: true, Role: RoleNormal}); err != nil {
		t.Fatal(err)
	}
	blocker, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lockTx, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.ExecContext(ctx, `LOCK TABLE twilight_runtime_logs IN ACCESS EXCLUSIVE MODE`); err != nil {
		_ = lockTx.Rollback()
		t.Fatal(err)
	}
	snapshotDone := make(chan error, 1)
	go func() {
		_, err := st.Snapshot()
		snapshotDone <- err
	}()
	// 等 Snapshot 进入被锁住的专表读取。
	time.Sleep(500 * time.Millisecond)
	readDone := make(chan int, 1)
	go func() { readDone <- st.UserCount() }()
	select {
	case n := <-readDone:
		if n != 1 {
			t.Errorf("unexpected user count %d", n)
		}
	case <-time.After(3 * time.Second):
		_ = lockTx.Rollback()
		<-snapshotDone
		t.Fatal("store read blocked while Snapshot was reading side tables")
	}
	_ = lockTx.Rollback()
	select {
	case err := <-snapshotDone:
		if err != nil {
			t.Fatalf("snapshot failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("snapshot did not finish after lock release")
	}
}
