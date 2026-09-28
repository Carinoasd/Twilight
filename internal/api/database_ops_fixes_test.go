package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// 数据库迁移不得接受请求体指定的 PostgreSQL DSN（防外连 / SSRF / 整库外送）。
func TestDatabaseMigrateRejectsRequestSuppliedDSN(t *testing.T) {
	dsn := os.Getenv("TWILIGHT_TEST_DSN")
	if dsn == "" {
		t.Skip("TWILIGHT_TEST_DSN not set")
	}
	app := newTestApp(t)
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	app.cfg().DatabaseMigrationPanelEnabled = true
	for _, key := range []string{"database_url", "postgres_dsn"} {
		body, _ := json.Marshal(map[string]any{"target_driver": "postgres", "dry_run": true, key: dsn})
		for _, path := range []string{"/api/v2/admin/database/migrate", "/api/v1/system/admin/database/migrate"} {
			rr := doJSON(app, http.MethodPost, path, string(body), admin)
			if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "INVALID_PAYLOAD") {
				t.Fatalf("%s with %s must be rejected: status=%d body=%s", path, key, rr.Code, rr.Body.String())
			}
		}
	}
}
