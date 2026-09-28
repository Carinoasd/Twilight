package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

func auditActionCount(app *App, action string) int {
	return app.store().QueryAuditLogs(store.AuditLogQuery{Action: action, Limit: 100}).Total
}

func auditPathTemplateCount(app *App, fragment string) int {
	count := 0
	for _, entry := range app.store().ListAuditLogs() {
		if tpl, _ := entry.Detail["path_template"].(string); strings.Contains(tpl, fragment) {
			count++
		}
	}
	return count
}

func jsonBackupNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") && !strings.HasSuffix(entry.Name(), ".meta.json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// 运维高风险操作必须写明确审计（不是 fallback），预览 / dry-run 不写。
func TestOpsHighRiskActionsWriteExplicitAudit(t *testing.T) {
	app := newTestApp(t)
	app.cfg().ConfigFile = filepath.Join(app.cfg().DatabaseDir, "config.toml")
	cfgText := "[Global]\nserver_name = \"before\"\ndatabases_dir = " + strconv.Quote(app.cfg().DatabaseDir) + "\n\n" +
		"[Database]\ndriver = " + strconv.Quote(app.cfg().DatabaseDriver) + "\nstate_file = " + strconv.Quote(app.cfg().StateFile) + "\nbackup_dir = " + strconv.Quote(app.cfg().DatabaseBackupDir) + "\n\n" +
		"[API]\nupload_folder = " + strconv.Quote(app.cfg().UploadDir) + "\n\n[Admin]\nusernames = [\"admin\"]\n"
	if err := os.WriteFile(app.cfg().ConfigFile, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.reloadConfig(); err != nil {
		t.Fatal(err)
	}
	admin := registerAndLogin(t, app, "admin", "Admin123456")

	// 配置修改：审计带 changed_keys（只有键名）。
	put, _ := json.Marshal(map[string]any{"content": strings.Replace(cfgText, `"before"`, `"after"`, 1)})
	if rr := doJSON(app, http.MethodPut, "/api/v2/admin/config/toml", string(put), admin); rr.Code != http.StatusOK {
		t.Fatalf("config put status=%d body=%s", rr.Code, rr.Body.String())
	}
	logs := app.store().QueryAuditLogs(store.AuditLogQuery{Action: "update_config_toml", Limit: 10}).Logs
	if len(logs) != 1 || !strings.Contains(mustJSON(logs[0].Detail), "Global.server_name") {
		t.Fatalf("update_config_toml audit missing changed_keys: %#v", logs)
	}
	if rr := doJSON(app, http.MethodPost, "/api/v2/admin/config/sweep", `{}`, admin); rr.Code != http.StatusOK || auditActionCount(app, "config_sweep") != 1 {
		t.Fatalf("config sweep audit missing: status=%d body=%s", rr.Code, rr.Body.String())
	}

	// 系统更新 dry-run 不写审计。
	_ = doJSON(app, http.MethodPost, "/api/v2/admin/system/update", `{"dry_run":true}`, admin)
	if auditActionCount(app, "system_update") != 0 || auditPathTemplateCount(app, "system/update") != 0 {
		t.Fatal("system update dry-run must not be audited")
	}

	// 数据库备份创建 / 删除。
	for i := 0; i < 2; i++ {
		if rr := doJSON(app, http.MethodPost, "/api/v2/admin/database/backup", `{"note":"n"}`, admin); rr.Code != http.StatusOK {
			t.Fatalf("backup status=%d body=%s", rr.Code, rr.Body.String())
		}
	}
	if got := auditActionCount(app, "create_database_backup"); got != 2 {
		t.Fatalf("create_database_backup audits=%d", got)
	}
	names := jsonBackupNames(t, app.cfg().DatabaseBackupDir)
	if len(names) < 2 {
		t.Fatalf("expected two backups, got %v", names)
	}
	if rr := doJSON(app, http.MethodDelete, "/api/v2/admin/database/backups/"+names[1], ``, admin); rr.Code != http.StatusOK {
		t.Fatalf("delete backup status=%d body=%s", rr.Code, rr.Body.String())
	}
	if auditActionCount(app, "delete_database_backup") != 1 {
		t.Fatal("delete_database_backup audit missing")
	}

	// 恢复预览不写审计（也不走 fallback）。
	preview, _ := json.Marshal(map[string]any{"name": names[0], "dry_run": true})
	if rr := doJSON(app, http.MethodPost, "/api/v2/admin/database/restore", string(preview), admin); rr.Code != http.StatusOK {
		t.Fatalf("restore preview status=%d body=%s", rr.Code, rr.Body.String())
	}
	if auditActionCount(app, "restore_database") != 0 || auditPathTemplateCount(app, "database/restore") != 0 {
		t.Fatal("restore preview must not be audited")
	}
	// 真恢复写 restore_database（恢复会替换审计表，所以必须在恢复之后写入）。
	confirm, _ := json.Marshal(map[string]any{"name": names[0], "confirm": databaseRestoreConfirmPhrase})
	if rr := doJSON(app, http.MethodPost, "/api/v2/admin/database/restore", string(confirm), admin); rr.Code != http.StatusOK {
		t.Fatalf("restore status=%d body=%s", rr.Code, rr.Body.String())
	}
	if auditActionCount(app, "restore_database") != 1 {
		t.Fatal("restore_database audit missing after restore")
	}
}
