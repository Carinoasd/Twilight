package api

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

// 管理员绑定邮箱 / 改验证状态要写明确审计（原来只有 fallback，看不出改了什么）。
func TestAdminEmailOperationsWriteExplicitAudit(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AuditLogEnabled = true
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	_ = registerAndLogin(t, app, "mail-target", "User123456")
	target, _ := app.store().FindUserByUsername("mail-target")
	base := "/api/v2/admin/users/" + strconv.FormatInt(target.UID, 10)

	if rr := doJSON(app, http.MethodPost, base+"/bind-email", `{"email":"target@example.com","mark_verified":false}`, admin); rr.Code != http.StatusOK {
		t.Fatalf("bind email status=%d body=%s", rr.Code, rr.Body.String())
	}
	bind := app.store().QueryAuditLogs(store.AuditLogQuery{Action: "admin_bind_email", Limit: 1}).Logs
	if len(bind) != 1 || bind[0].TargetUID != target.UID || bind[0].Detail["new_email_masked"] != maskEmail("target@example.com") || bind[0].Detail["fallback"] != nil {
		t.Fatalf("admin_bind_email audit missing or wrong: %#v", bind)
	}
	if rr := doJSON(app, http.MethodPost, base+"/email/verified", `{"verified":true}`, admin); rr.Code != http.StatusOK {
		t.Fatalf("set verified status=%d body=%s", rr.Code, rr.Body.String())
	}
	verified := app.store().QueryAuditLogs(store.AuditLogQuery{Action: "admin_set_email_verified", Limit: 1}).Logs
	if len(verified) != 1 || verified[0].Detail["verified"] != true || verified[0].Detail["old_verified"] != false {
		t.Fatalf("admin_set_email_verified audit missing or wrong: %#v", verified)
	}
}
