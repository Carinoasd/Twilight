package api

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// V2 迁移导入不得被默认上传上限（测试中 1MB）截断：导出一个 >1MB 的迁移包，
// 再经 V2 路由预览导入必须成功。
func TestV2MigrationImportAcceptsArchiveLargerThanUploadLimit(t *testing.T) {
	app := newTestApp(t)
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	app.cfg().DatabaseMigrationPanelEnabled = true
	avatarDir := filepath.Join(app.cfg().UploadDir, "avatar")
	if err := os.MkdirAll(avatarDir, 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 2<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(avatarDir, "0123456789abcdef.png"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	export := doJSON(app, http.MethodPost, "/api/v2/admin/migration/export", `{}`, admin)
	if export.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%.300s", export.Code, export.Body.String())
	}
	archive := export.Body.Bytes()
	if int64(len(archive)) <= app.cfg().MaxUploadSize {
		t.Fatalf("test archive too small: %d", len(archive))
	}
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("archive", "twilight.zip")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(archive)
	_ = writer.WriteField("preview", "true")
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/admin/migration/import", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Twilight-Client", "webui")
	for _, cookie := range admin {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("v2 import preview of %d-byte archive status=%d body=%.300s", len(archive), rr.Code, rr.Body.String())
	}
}
