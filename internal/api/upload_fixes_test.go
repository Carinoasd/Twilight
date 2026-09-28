package api

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/prejudice-studio/twilight/internal/config"
	"github.com/prejudice-studio/twilight/internal/migration"
	"github.com/prejudice-studio/twilight/internal/store"
)

var testPNGBytes = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0, 'b', 'g'}

func uploadAuthBackgroundForTest(t *testing.T, app *App, cookie *http.Cookie, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", "bg.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = writer.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/system/admin/config/upload-auth-background", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Twilight-Client", "webui")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	return rr
}

// 上传登录页背景必须真的写盘，公开端点能取回同一份字节；换扩展名时旧档在成功后才删除。
func TestAuthBackgroundUploadWritesFile(t *testing.T) {
	app := newTestApp(t)
	app.cfg().ConfigFile = filepath.Join(app.cfg().DatabaseDir, "config.toml")
	// 配置里显式写上传目录与数据库路径，热重载后仍指向本测试的临时目录。
	cfgText := "[Global]\ndatabases_dir = " + strconv.Quote(app.cfg().DatabaseDir) + "\n\n" +
		"[Database]\ndriver = " + strconv.Quote(app.cfg().DatabaseDriver) + "\nstate_file = " + strconv.Quote(app.cfg().StateFile) + "\nbackup_dir = " + strconv.Quote(app.cfg().DatabaseBackupDir) + "\n\n" +
		"[API]\nupload_folder = " + strconv.Quote(app.cfg().UploadDir) + "\n\n[Admin]\nusernames = [\"admin\"]\n"
	if err := os.WriteFile(app.cfg().ConfigFile, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	cookies := registerAndLogin(t, app, "admin", "Admin123456")
	dir := filepath.Join(app.cfg().UploadDir, "auth-background")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "background.jpg"), []byte("old-jpg"), 0o600); err != nil {
		t.Fatal(err)
	}
	rr := uploadAuthBackgroundForTest(t, app, findCookie(cookies, "twilight_session"), testPNGBytes)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", rr.Code, rr.Body.String())
	}
	saved, err := os.ReadFile(filepath.Join(dir, "background.png"))
	if err != nil || !bytes.Equal(saved, testPNGBytes) {
		t.Fatalf("uploaded background was not written: err=%v data=%q", err, saved)
	}
	if _, err := os.Stat(filepath.Join(dir, "background.jpg")); !os.IsNotExist(err) {
		t.Fatalf("old background with other extension should be removed after success, err=%v", err)
	}
	get := doJSON(app, http.MethodGet, "/api/v2/system/auth-background", "", nil)
	if get.Code != http.StatusOK || !bytes.Equal(get.Body.Bytes(), testPNGBytes) {
		t.Fatalf("public background status=%d body=%q", get.Code, get.Body.Bytes())
	}
}

// 公开端点只回 background.*，不得回退到目录里的任意文件。
func TestAuthBackgroundPublicEndpointOnlyServesBackgroundFile(t *testing.T) {
	app := newTestApp(t)
	dir := filepath.Join(app.cfg().UploadDir, "auth-background")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zzz.html"), []byte("<h1>phish</h1>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "0123456789abcdef.png"), testPNGBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v2/system/auth-background", "/api/v1/system/auth-background", "/api/v1/system/auth-background?file=0123456789abcdef.png"} {
		rr := doJSON(app, http.MethodGet, path, "", nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s must not serve arbitrary files: status=%d type=%q body=%q", path, rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
		}
	}
}

// 迁移导入资源：不符合命名空间白名单的文件名不写盘并在计划里列出。
func TestPlanMigrationResourcesSkipsDisallowedNames(t *testing.T) {
	root := t.TempDir()
	app := &App{}
	app.runtime.Store(&runtimeState{cfg: config.Config{UploadDir: root}})
	paths := map[string]bool{
		"resources/auth-background/zzz.html":             false,
		"resources/auth-background/background.png":       true,
		"resources/avatars/evil.svg":                     false,
		"resources/avatars/0123456789abcdef.webp":        true,
		"resources/tickets/42/0123456789abcdef.png":      true,
		"resources/tickets/42/reply/index.html":          false,
		"resources/bangumi/123.jpg":                      true,
		"resources/server-icon/0123456789abcdef.png.txt": false,
	}
	archive := migration.Archive{Files: map[string][]byte{}}
	for p := range paths {
		archive.Manifest.Files = append(archive.Manifest.Files, migration.FileEntry{Path: p, Kind: "resource", Size: 1})
		archive.Files[p] = []byte("x")
	}
	plan, err := app.planMigrationResources(archive)
	if err != nil {
		t.Fatal(err)
	}
	planned := map[string]bool{}
	for _, entry := range plan.Entries {
		planned[entry.LogicalPath] = true
	}
	for p, allowed := range paths {
		if planned[p] != allowed {
			t.Fatalf("resource %q planned=%v, want %v", p, planned[p], allowed)
		}
	}
}

// handleAsset 背景归属必须精确比对图片字段，不能用子串包含。
func TestUserBackgroundAssetOwnershipIsExact(t *testing.T) {
	app := newTestApp(t)
	cookies := registerAndLogin(t, app, "alice", "Alice123456")
	u, ok := app.store().FindUserByUsername("alice")
	if !ok {
		t.Fatal("user missing")
	}
	const name = "0123456789abcdef.png"
	dir := filepath.Join(app.cfg().UploadDir, "background")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), testPNGBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// 背景配置里只在非图片字段出现了该文件名。
	if _, err := app.store().UpdateUser(u.UID, func(user *store.User) error {
		user.Background = `{"lightBg":"","note":"` + name + `"}`
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rr := doJSON(app, http.MethodGet, "/api/v1/users/assets/background/"+name, "", cookies)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("substring match must not grant access: status=%d", rr.Code)
	}
	if _, err := app.store().UpdateUser(u.UID, func(user *store.User) error {
		user.Background = `{"lightBgImage":"url(\"/api/v1/users/assets/background/` + name + `\")"}`
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rr = doJSON(app, http.MethodGet, "/api/v1/users/assets/background/"+name, "", cookies)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner should read referenced background: status=%d body=%s", rr.Code, rr.Body.String())
	}
}
