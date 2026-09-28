package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 多行字符串藏哨兵：PUT 时不得把普通字段里的哨兵回填成真值。
func TestRestoreTOMLSecretsIgnoresSentinelInsideMultilineString(t *testing.T) {
	disk := "[Emby]\nemby_url = \"http://emby\"\nemby_token = \"REALTOKEN123\"\n"
	submitted := "[Emby]\nemby_url = \"http://emby\"\nemby_token = \"" + secretMaskValue + "\"\nnote = \"\"\"\nemby_token = \"" + secretMaskValue + "\"\n\"\"\"\n"
	out, err := restoreTOMLSecrets(submitted, disk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "REALTOKEN123") != 1 {
		t.Fatalf("real token must be restored exactly once (at emby_token), got:\n%s", out)
	}
	if !strings.Contains(out, "emby_token = \"REALTOKEN123\"\nnote") {
		t.Fatalf("emby_token not restored in place:\n%s", out)
	}
}

// section 头带注释、”' 多行字面量、点号键、内联表、别名位置都必须被遮蔽。
func TestMaskTOMLSecretsStructuralBypasses(t *testing.T) {
	content := strings.Join([]string{
		"[Telegram] # bot",
		"bot_token = \"TG_SECRET_1\"",
		"[Emby]",
		"emby_token = '''",
		"EMBY_SECRET_LINE1",
		"EMBY_SECRET_LINE2'''",
		"[Global]",
		"Emby.emby_password = \"EMBY_PASS_DOTTED\"",
		"inline = { smtp_password = \"SMTP_INLINE\" }",
		"[PostgreSQL]",
		"password = \"PG_ALIAS_SECRET\"",
		"[Database]",
		"url = \"postgres://u:DSN_SECRET@h/db\"",
		"",
	}, "\n")
	// 根层裸键别名必须写在任何表头之前。
	content = "bot_internal_secret = \"ROOT_ALIAS_SECRET\"\n" + content
	masked, err := maskTOMLSecrets(content)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"TG_SECRET_1", "EMBY_SECRET_LINE1", "EMBY_SECRET_LINE2", "EMBY_PASS_DOTTED", "SMTP_INLINE", "PG_ALIAS_SECRET", "DSN_SECRET", "ROOT_ALIAS_SECRET"} {
		if strings.Contains(masked, secret) {
			t.Fatalf("secret %q leaked after mask:\n%s", secret, masked)
		}
	}
	if !strings.Contains(masked, "[Telegram] # bot") {
		t.Fatalf("comments/layout should be preserved:\n%s", masked)
	}
	// 遮蔽后再回填必须还原成与原文等价的值。
	restored, err := restoreTOMLSecrets(masked, content, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"TG_SECRET_1", "EMBY_PASS_DOTTED", "SMTP_INLINE", "PG_ALIAS_SECRET", "DSN_SECRET", "ROOT_ALIAS_SECRET", `EMBY_SECRET_LINE1\nEMBY_SECRET_LINE2`} {
		if !strings.Contains(restored, secret) {
			t.Fatalf("secret %q not restored:\n%s", secret, restored)
		}
	}
}

func TestMaskTOMLSecretsRejectsUnparseableContent(t *testing.T) {
	if _, err := maskTOMLSecrets("[Emby]\nemby_token = \"unterminated\n"); err == nil {
		t.Fatal("unparseable content must not be returned unmasked")
	}
}

// 端到端：审查报告的 PoC——PUT 一个藏哨兵的多行 note，再 GET，不得读出明文。
func TestConfigTOMLMultilineSentinelDoesNotLeakSecret(t *testing.T) {
	app := newTestApp(t)
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	app.cfg().ConfigFile = filepath.Join(app.cfg().DatabaseDir, "config.toml")
	const token = "REALTOKEN_POC_123456"
	raw := "[Global]\ndatabases_dir = " + strconv.Quote(app.cfg().DatabaseDir) + "\n\n" +
		"[Database]\ndriver = " + strconv.Quote(app.cfg().DatabaseDriver) + "\nstate_file = " + strconv.Quote(app.cfg().StateFile) + "\nbackup_dir = " + strconv.Quote(app.cfg().DatabaseBackupDir) + "\n\n" +
		"[Emby]\nemby_url = \"http://127.0.0.1:8096/\"\nemby_token = " + strconv.Quote(token) + "\n\n[Admin]\nusernames = [\"admin\"]\n"
	if err := os.WriteFile(app.cfg().ConfigFile, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.reloadConfig(); err != nil {
		t.Fatal(err)
	}
	attack := strings.Replace(raw, "emby_token = "+strconv.Quote(token)+"\n",
		"emby_token = \""+secretMaskValue+"\"\nnote = \"\"\"\nemby_token = \""+secretMaskValue+"\"\n\"\"\"\n", 1)
	payload, _ := json.Marshal(map[string]any{"content": attack})
	put := doJSON(app, http.MethodPut, "/api/v2/admin/config/toml", string(payload), admin)
	if put.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", put.Code, put.Body.String())
	}
	for _, path := range []string{"/api/v2/admin/config/toml", "/api/v1/system/admin/config/toml"} {
		get := doJSON(app, http.MethodGet, path, "", admin)
		if get.Code != http.StatusOK {
			t.Fatalf("get %s status=%d body=%s", path, get.Code, get.Body.String())
		}
		if strings.Contains(get.Body.String(), token) {
			t.Fatalf("SECURITY: %s leaked secret via multiline sentinel: %s", path, get.Body.String())
		}
	}
	saved, _ := os.ReadFile(app.cfg().ConfigFile)
	if !strings.Contains(string(saved), token) {
		t.Fatalf("real token should still be on disk at emby_token:\n%s", saved)
	}
}

// section 头带注释时，GET raw_content 不得回传其下的密钥。
func TestConfigTOMLGetMasksSecretUnderCommentedSectionHeader(t *testing.T) {
	app := newTestApp(t)
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	app.cfg().ConfigFile = filepath.Join(app.cfg().DatabaseDir, "config.toml")
	raw := "[Global]\ndatabases_dir = " + strconv.Quote(app.cfg().DatabaseDir) + "\n\n" +
		"[Database]\ndriver = " + strconv.Quote(app.cfg().DatabaseDriver) + "\nstate_file = " + strconv.Quote(app.cfg().StateFile) + "\nbackup_dir = " + strconv.Quote(app.cfg().DatabaseBackupDir) + "\n\n" +
		"[Telegram] # bot\nbot_token = \"TG_COMMENT_HEADER_SECRET\"\n[Emby]\nemby_password = '''\nML_SECRET_A\nML_SECRET_B'''\n\n[Admin]\nusernames = [\"admin\"]\n"
	if err := os.WriteFile(app.cfg().ConfigFile, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	get := doJSON(app, http.MethodGet, "/api/v2/admin/config/toml", "", admin)
	if get.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", get.Code, get.Body.String())
	}
	for _, secret := range []string{"TG_COMMENT_HEADER_SECRET", "ML_SECRET_A", "ML_SECRET_B"} {
		if strings.Contains(get.Body.String(), secret) {
			t.Fatalf("secret %q leaked: %s", secret, get.Body.String())
		}
	}
}
