package api

import (
	"net/http"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

// 管理员写请求失败也要留下 *_failed 记录（含 status / error_code），预览请求带 dry_run 旗标。
func TestFallbackAuditRecordsAdminFailuresAndDryRun(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AuditLogEnabled = true
	admin := registerAndLogin(t, app, "admin", "Admin123456")

	if rr := doJSON(app, http.MethodPost, "/api/v2/admin/users/cleanup-invalid", `{"dry_run":true}`, admin); rr.Code != http.StatusOK {
		t.Fatalf("dry-run status=%d body=%s", rr.Code, rr.Body.String())
	}
	preview := app.store().QueryAuditLogs(store.AuditLogQuery{Action: "post_admin_users_cleanup_invalid", Limit: 5}).Logs
	if len(preview) != 1 || preview[0].Detail["dry_run"] != true {
		t.Fatalf("dry-run fallback should carry dry_run=true, got %#v", preview)
	}

	if rr := doJSON(app, http.MethodPost, "/api/v2/admin/users/cleanup-invalid", `{"dry_run":false}`, admin); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing confirm status=%d body=%s", rr.Code, rr.Body.String())
	}
	failed := app.store().QueryAuditLogs(store.AuditLogQuery{Action: "post_admin_users_cleanup_invalid_failed", Limit: 5}).Logs
	if len(failed) != 1 {
		t.Fatalf("admin failure should write a _failed fallback audit, got %#v", failed)
	}
	detail := failed[0].Detail
	if detail["status"] != float64(http.StatusBadRequest) || detail["error_code"] != string(ErrBatchConfirmRequired) || detail["dry_run"] != false || failed[0].Category != "admin" {
		t.Fatalf("unexpected failure detail: %#v", failed[0])
	}
}

// 普通用户的失败请求不记；管理员走自助路由时 target_uid 记成本人。
func TestFallbackAuditSelfServiceTargetsSelfAndSkipsUserFailures(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AuditLogEnabled = true
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	user := registerAndLogin(t, app, "plain-user", "User123456")
	adminUser, _ := app.store().FindUserByUsername("admin")
	if _, err := app.store().ClearAuditLogs(); err != nil {
		t.Fatal(err)
	}

	if rr := doJSON(app, http.MethodPost, "/api/v2/tickets", `{}`, user); rr.Code < 400 {
		t.Fatalf("expected invalid ticket to fail, status=%d", rr.Code)
	}
	if rr := doJSON(app, http.MethodPost, "/api/v2/auth/logout", ``, admin); rr.Code != http.StatusOK {
		t.Fatalf("admin logout status=%d body=%s", rr.Code, rr.Body.String())
	}
	logs := app.store().ListAuditLogs()
	// 登出现在有明确审计 logout（不再落到 fallback 的 post_auth_logout），target_uid 同样是本人。
	if len(logs) != 1 || logs[0].Action != "logout" || logs[0].TargetUID != adminUser.UID {
		t.Fatalf("expected only admin logout with target=self (%d), got %#v", adminUser.UID, logs)
	}
}
