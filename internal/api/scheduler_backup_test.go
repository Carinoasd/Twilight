package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// TestAutoBackupDatabaseKeepsOnlyRecentAutoBackups 覆盖新增的定期备份任务：只保留最近 keep 份
// 自动备份，手动备份永远不删；未开启时自动排程空转。
func TestAutoBackupDatabaseKeepsOnlyRecentAutoBackups(t *testing.T) {
	app := newTestApp(t)
	dir := app.cfg().DatabaseBackupDir
	manual, err := app.store().BackupWithNote(dir, "manual")
	if err != nil {
		t.Fatal(err)
	}
	run := func(manualRun bool, params map[string]any) map[string]any {
		t.Helper()
		ctx := context.WithValue(context.Background(), schedulerFrozenParamsKey{}, params)
		ctx = context.WithValue(ctx, schedulerManualContextKey, manualRun)
		summary, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler/internal", nil).WithContext(ctx), "auto_backup_database")
		if err != nil {
			t.Fatal(err)
		}
		return summary
	}
	if summary := run(false, map[string]any{"enabled": false, "keep": 2}); !boolish(summary["skipped"]) {
		t.Fatalf("disabled auto backup must skip: %#v", summary)
	}
	names := []string{}
	for i := 0; i < 3; i++ {
		summary := run(false, map[string]any{"enabled": true, "keep": 2})
		names = append(names, asString(summary["backup"]))
		time.Sleep(20 * time.Millisecond)
	}
	backups, err := store.ListBackups(dir)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, b := range backups {
		have[b.Name] = true
	}
	if !have[manual.Name] {
		t.Fatal("manual backup must never be rotated")
	}
	if have[names[0]] || !have[names[1]] || !have[names[2]] {
		t.Fatalf("expected only the two newest auto backups, have=%v names=%v", have, names)
	}
}
