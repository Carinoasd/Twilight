package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

func latestAuditByAction(t *testing.T, app *App, action string) store.AuditLog {
	t.Helper()
	logs := app.store().ListAuditLogs()
	for i := len(logs) - 1; i >= 0; i-- {
		if logs[i].Action == action {
			return logs[i]
		}
	}
	// ListAuditLogs 顺序不保证时再全量找一次（取 ID 最大者）。
	var best store.AuditLog
	for _, l := range logs {
		if l.Action == action && l.ID > best.ID {
			best = l
		}
	}
	if best.ID == 0 {
		t.Fatalf("missing audit action %q; have %d logs", action, len(logs))
	}
	return best
}

func auditDetailJSON(l store.AuditLog) string {
	b, _ := json.Marshal(l.Detail)
	return string(b)
}

// 本区卡码 / 邀请码 / 续期 / 注册资格的写操作必须留下可辨识（非 [REDACTED]）、
// 带改前改后的审计记录，且不得泄露完整码值。
func TestRegistrationAreaAuditDetails(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AuditLogEnabled = true
	adminCookies := registerAndLogin(t, app, "admin", "AdminPassw0rd123")
	if l := latestAuditByAction(t, app, "register"); l.Detail["promoted_admin"] != true {
		t.Fatalf("register audit missing detail: %s", auditDetailJSON(l))
	}

	// 创建 / 删除注册码：记 code_hints / code_hint，不被遮罩，也不含完整码。
	created := doJSON(app, http.MethodPost, "/api/v1/admin/regcodes", `{"type":1,"days":30,"count":1}`, adminCookies)
	if created.Code != http.StatusOK && created.Code != http.StatusCreated {
		t.Fatalf("create regcode status=%d body=%s", created.Code, created.Body.String())
	}
	codes := app.store().ListRegCodes()
	if len(codes) != 1 {
		t.Fatalf("expected one regcode, got %d", len(codes))
	}
	full := codes[0].Code
	createLog := latestAuditByAction(t, app, "create_regcode")
	createJSON := auditDetailJSON(createLog)
	if strings.Contains(createJSON, "REDACTED") || strings.Contains(createJSON, full) || !strings.Contains(createJSON, regcodeAuditHint(full)) {
		t.Fatalf("create_regcode detail should carry hint only: %s (code %s)", createJSON, full)
	}

	// 普通用户兑换：use_code 带 code_hint 与改前改后到期。
	userCookies := registerAndLogin(t, app, "audit-user", "UserPassw0rd123")
	used := doJSON(app, http.MethodPost, "/api/v1/users/me/use-code", `{"code":"`+full+`"}`, userCookies)
	if used.Code != http.StatusOK {
		t.Fatalf("use-code status=%d body=%s", used.Code, used.Body.String())
	}
	useLog := latestAuditByAction(t, app, "use_code")
	useJSON := auditDetailJSON(useLog)
	if strings.Contains(useJSON, full) || useLog.Detail["code_hint"] != regcodeAuditHint(full) || useLog.Detail["expired_at_after"] == nil {
		t.Fatalf("use_code detail incomplete: %s", useJSON)
	}

	user, _ := app.store().FindUserByUsername("audit-user")
	// 管理员续期：before / after。
	renew := doJSON(app, http.MethodPost, "/api/v1/admin/users/"+strconv.FormatInt(user.UID, 10)+"/renew", `{"days":10}`, adminCookies)
	if renew.Code != http.StatusOK {
		t.Fatalf("admin renew status=%d body=%s", renew.Code, renew.Body.String())
	}
	renewLog := latestAuditByAction(t, app, "admin_renew_user")
	if _, ok := renewLog.Detail["before"].(map[string]any); !ok {
		t.Fatalf("admin_renew_user missing before/after: %s", auditDetailJSON(renewLog))
	}

	// 清注册队列 / 发放注册资格：显式审计。
	if resp := doJSON(app, http.MethodPost, "/api/v1/admin/users/"+strconv.FormatInt(user.UID, 10)+"/registration-queue/clear", `{}`, adminCookies); resp.Code != http.StatusOK {
		t.Fatalf("queue clear status=%d body=%s", resp.Code, resp.Body.String())
	}
	if l := latestAuditByAction(t, app, "clear_registration_queue"); l.Detail["old_pending"] != true {
		t.Fatalf("clear_registration_queue detail: %s", auditDetailJSON(l))
	}
	if resp := doJSON(app, http.MethodPost, "/api/v1/admin/users/"+strconv.FormatInt(user.UID, 10)+"/registration-entitlement", `{"days":15}`, adminCookies); resp.Code != http.StatusOK {
		t.Fatalf("grant entitlement status=%d body=%s", resp.Code, resp.Body.String())
	}
	if l := latestAuditByAction(t, app, "grant_registration_entitlement"); l.TargetUID != user.UID {
		t.Fatalf("grant_registration_entitlement detail: %+v", l)
	}

	// 生成 / 删除邀请码：create_invite_code 与 delete_invite_code 都只记提示。
	if resp := doJSON(app, http.MethodPost, "/api/v1/invite/codes", `{"days":7}`, adminCookies); resp.Code != http.StatusCreated {
		t.Fatalf("create invite status=%d body=%s", resp.Code, resp.Body.String())
	}
	admin, _ := app.store().FindUserByUsername("admin")
	invites := app.store().ListInviteCodes(admin.UID)
	if len(invites) != 1 {
		t.Fatalf("expected one invite code, got %d", len(invites))
	}
	inv := invites[0].Code
	if resp := doJSON(app, http.MethodDelete, "/api/v1/invite/codes/"+inv, ``, adminCookies); resp.Code != http.StatusOK {
		t.Fatalf("delete invite status=%d body=%s", resp.Code, resp.Body.String())
	}
	for _, action := range []string{"create_invite_code", "delete_invite_code"} {
		l := latestAuditByAction(t, app, action)
		if l.Detail["code_hint"] != regcodeAuditHint(inv) || strings.Contains(auditDetailJSON(l), inv) {
			t.Fatalf("%s detail: %s", action, auditDetailJSON(l))
		}
	}

	// 删除注册码。
	if resp := doJSON(app, http.MethodDelete, "/api/v1/admin/regcodes/"+full, ``, adminCookies); resp.Code != http.StatusOK {
		t.Fatalf("delete regcode status=%d body=%s", resp.Code, resp.Body.String())
	}
	if l := latestAuditByAction(t, app, "delete_regcode"); l.Detail["code_hint"] != regcodeAuditHint(full) {
		t.Fatalf("delete_regcode detail: %s", auditDetailJSON(l))
	}
}

func TestRegcodeAuditHintKeepsOnlyEdges(t *testing.T) {
	if got := regcodeAuditHint("ABCD-1234-5678-WXYZ"); got != "ABCD…WXYZ" {
		t.Fatalf("hint=%q", got)
	}
	if auditSensitiveDetailKey("code_hint") || auditSensitiveDetailKey("code_hints") || auditSensitiveDetailKey("deleted_hints") {
		t.Fatal("hint keys must not be masked by the sensitive-key filter")
	}
}
