package store

import (
	"errors"
	"testing"
)

// 写入时的条数上限不能把管理员记录挤掉。
func TestAddAuditLogLimitPreservesAdminRows(t *testing.T) {
	st := newJSONStoreForTest(t)
	if err := st.AddAuditLog(AuditLog{Action: "admin_delete_user", Category: "admin", CreatedAt: 1}, 3); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := st.AddAuditLog(AuditLog{Action: "login", Category: "user", CreatedAt: int64(2 + i)}, 3); err != nil {
			t.Fatal(err)
		}
	}
	page := st.QueryAuditLogs(AuditLogQuery{Category: "admin", Limit: 10})
	if page.Total != 1 {
		t.Fatalf("admin audit row was evicted by the count limit, admin total=%d", page.Total)
	}
	if user := st.QueryAuditLogs(AuditLogQuery{Category: "user", Limit: 10}); user.Total != 3 {
		t.Fatalf("user rows should be capped at 3, got %d", user.Total)
	}
}

// 删除 / 清空 / 裁剪审计日志留下的自保记录不能被任何删除路径带走。
func TestProtectedAuditLogsSurviveDeleteClearAndPrune(t *testing.T) {
	st := newJSONStoreForTest(t)
	if err := st.AddAuditLog(AuditLog{Action: "clear_audit_logs", Category: "admin", CreatedAt: 1}, 100); err != nil {
		t.Fatal(err)
	}
	if err := st.AddAuditLog(AuditLog{Action: "login", Category: "user", CreatedAt: 2}, 100); err != nil {
		t.Fatal(err)
	}
	protected := st.QueryAuditLogs(AuditLogQuery{Action: "clear_audit_logs", Limit: 1}).Logs[0]
	if _, err := st.DeleteAuditLog(protected.ID); !errors.Is(err, ErrAuditLogProtected) {
		t.Fatalf("protected delete err=%v, want ErrAuditLogProtected", err)
	}
	if _, err := st.PruneAuditLogsWithPolicy(AuditLogPruneOptions{CutoffUnix: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PruneAuditLogs(1, false); err != nil {
		t.Fatal(err)
	}
	removed, err := st.ClearAuditLogs()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("login row should already be pruned by age, removed=%d", removed)
	}
	logs := st.ListAuditLogs()
	if len(logs) != 1 || logs[0].ID != protected.ID {
		t.Fatalf("protected record should survive, logs=%#v", logs)
	}
}

// 手动按条数裁剪在 preserve_admin=true 时同样不删管理员记录。
func TestPruneAuditLogsByCountHonorsPreserveAdmin(t *testing.T) {
	st := newJSONStoreForTest(t)
	for i, category := range []string{"admin", "user", "user", "user"} {
		if err := st.AddAuditLog(AuditLog{Action: "x", Category: category, CreatedAt: int64(i + 1)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	result, err := st.PruneAuditLogsWithPolicy(AuditLogPruneOptions{MaxEntries: 1, PreserveAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.RemovedByLimit != 2 || result.Current != 2 {
		t.Fatalf("result=%+v, want removed 2 user rows and keep admin + newest user", result)
	}
	if page := st.QueryAuditLogs(AuditLogQuery{Category: "admin", Limit: 1}); page.Total != 1 {
		t.Fatalf("admin row pruned despite preserve_admin")
	}
}
