package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

// 单条删除审计日志要确认短语，删除后留下不可删的 delete_audit_log 记录；
// 裁剪同样留下 prune_audit_logs 记录。
func TestAuditLogMaintenanceRequiresConfirmAndLeavesProtectedRecord(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AuditLogEnabled = true
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	app.auditEntryIP("telegram", 0, "victim", "sample_action", "user", 0, nil)
	target := app.store().QueryAuditLogs(storeAuditQueryAction("sample_action")).Logs[0]
	path := "/api/v2/admin/audit-logs/" + strconv.FormatInt(target.ID, 10)

	if rr := doJSON(app, http.MethodDelete, path, ``, admin); rr.Code != http.StatusBadRequest {
		t.Fatalf("delete without confirm status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr := doJSON(app, http.MethodDelete, path, `{"confirm":"`+confirmDeleteAuditLog+`"}`, admin)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}
	records := app.store().QueryAuditLogs(storeAuditQueryAction("delete_audit_log")).Logs
	if len(records) != 1 || records[0].Detail["deleted_action"] != "sample_action" || records[0].UID == 0 {
		t.Fatalf("delete_audit_log record missing: %#v", records)
	}
	protectedPath := "/api/v2/admin/audit-logs/" + strconv.FormatInt(records[0].ID, 10)
	if rr := doJSON(app, http.MethodDelete, protectedPath, `{"confirm":"`+confirmDeleteAuditLog+`"}`, admin); rr.Code != http.StatusForbidden {
		t.Fatalf("deleting protected record status=%d body=%s", rr.Code, rr.Body.String())
	}

	rr = doJSON(app, http.MethodPost, "/api/v2/admin/audit-logs/prune", `{"confirm":"`+confirmPruneAuditLogs+`","max_entries":1,"preserve_admin":false}`, admin)
	if rr.Code != http.StatusOK {
		t.Fatalf("prune status=%d body=%s", rr.Code, rr.Body.String())
	}
	if page := app.store().QueryAuditLogs(storeAuditQueryAction("prune_audit_logs")); page.Total != 1 {
		t.Fatalf("prune_audit_logs record missing, total=%d", page.Total)
	}
	if page := app.store().QueryAuditLogs(storeAuditQueryAction("delete_audit_log")); page.Total != 1 {
		t.Fatalf("prune removed the protected delete record, total=%d", page.Total)
	}
}

func storeAuditQueryAction(action string) store.AuditLogQuery {
	return store.AuditLogQuery{Action: action, Limit: 10}
}
