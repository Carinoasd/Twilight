package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// 格式错误的 IP 被拒；CIDR 被接受并规范化；V2 审计的 expire_at 是时间戳而不是小时数。
func TestIPBlacklistHandlersValidateAndAuditRealExpiry(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AuditLogEnabled = true
	admin := registerAndLogin(t, app, "admin", "Admin123456")

	if rr := doJSON(app, http.MethodPost, "/api/v1/security/ip/blacklist", `{"ip":"1.2.3.300"}`, admin); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid ip status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(app, http.MethodPost, "/api/v2/admin/security/ip-blacklist", `{"ip":" 198.51.100.9/24 ","hours":2}`, admin); rr.Code != http.StatusOK {
		t.Fatalf("cidr add status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !app.store().IsIPBlacklisted("198.51.100.200") {
		t.Fatal("CIDR entry should block addresses in the prefix")
	}
	logs := app.store().QueryAuditLogs(store.AuditLogQuery{Action: "add_ip_blacklist", Limit: 1}).Logs
	if len(logs) != 1 || logs[0].Detail["ip"] != "198.51.100.0/24" {
		t.Fatalf("unexpected audit: %#v", logs)
	}
	if expireAt, _ := logs[0].Detail["expire_at"].(float64); int64(expireAt) < time.Now().Unix() {
		t.Fatalf("expire_at should be a unix timestamp, got %v", logs[0].Detail["expire_at"])
	}
}
