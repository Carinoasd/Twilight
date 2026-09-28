package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

func auditEntriesForTest(app *App, action string) []store.AuditLog {
	return app.store().QueryAuditLogs(store.AuditLogQuery{Action: action, Limit: 50}).Logs
}

// TestAdminUnbindEmbyWritesExplicitAudit 回归稽核缺口：管理员解绑 Emby 旧实现只有 fallback，
// 看不出旧 Emby ID 与远端是否停用。
func TestAdminUnbindEmbyWritesExplicitAudit(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AuditLogEnabled = true
	adminCookies := registerAndLogin(t, app, "admin", "Admin123456")
	user, err := app.store().CreateUser(store.User{Username: "unbind-audit", Role: store.RoleNormal, Active: true, EmbyID: "emby-ua", EmbyUsername: "ua"})
	if err != nil {
		t.Fatal(err)
	}
	newFakeEmbyPolicyServer(t, app, map[string]bool{"emby-ua": false})
	resp := doJSONWithHeaders(app, http.MethodDelete, fmt.Sprintf("/api/v1/admin/users/%d/emby", user.UID), "", adminCookies, map[string]string{"X-Twilight-Client": "webui"})
	if resp.Code != http.StatusOK {
		t.Fatalf("unbind status=%d body=%s", resp.Code, resp.Body.String())
	}
	logs := auditEntriesForTest(app, "admin_unbind_emby")
	if len(logs) != 1 || logs[0].TargetUID != user.UID || asString(logs[0].Detail["old_emby_id"]) != "emby-ua" {
		t.Fatalf("expected explicit admin_unbind_emby audit, got %#v", logs)
	}
}

// TestSchedulerStateChangesAreAudited 回归稽核缺口：会改用户状态的排程任务要写系统稽核并附 uid。
func TestSchedulerStateChangesAreAudited(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AuditLogEnabled = true
	app.cfg().AutoCleanupPendingEmby = true
	app.cfg().AutoCleanupPendingEmbyDays = 1
	old := time.Now().AddDate(0, 0, -10).Unix()
	days := 30
	user, err := app.store().CreateUser(store.User{Username: "pending-audit", Role: store.RoleNormal, Active: true, PendingEmby: true, PendingEmbyDays: &days, RegisterTime: old, CreatedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil), "cleanup_pending_emby_entitlements"); err != nil {
		t.Fatal(err)
	}
	logs := auditEntriesForTest(app, "clear_pending_emby_entitlements")
	if len(logs) != 1 {
		t.Fatalf("expected clear_pending_emby_entitlements audit, got %#v", logs)
	}
	uids, _ := logs[0].Detail["uids"].([]any)
	if len(uids) != 1 || int64(numeric(uids[0])) != user.UID {
		t.Fatalf("audit must list affected uids: %#v", logs[0].Detail)
	}

	// 裁剪稽核日志本身也要留痕。
	for i := 0; i < 5; i++ {
		app.auditSystem("scheduler", "filler", 0, map[string]any{"i": i})
	}
	ctx := context.WithValue(context.Background(), schedulerFrozenParamsKey{}, map[string]any{"enabled": true, "max_entries": 2})
	if _, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil).WithContext(ctx), "cleanup_audit_logs"); err != nil {
		t.Fatal(err)
	}
	if len(auditEntriesForTest(app, "prune_audit_logs")) != 1 {
		t.Fatal("pruning audit logs must leave its own audit entry")
	}
}
