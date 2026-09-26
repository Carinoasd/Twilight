package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/prejudice-studio/twilight/internal/config"
	"github.com/prejudice-studio/twilight/internal/migration"
)

func TestConfigRevisionConcurrentEditorsPreserveOverrides(t *testing.T) {
	app := newTestApp(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	app.cfg().ConfigFile = path
	content := "[Global]\nserver_name = \"file\"\n[Unknown]\nretained = true\n[Telegram]\nbot_token = \"file-secret\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(path, ".toml")+".local.toml", []byte("[Global]\nserver_name = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWILIGHT_SERVER_NAME", "environment")
	t.Setenv("TWILIGHT_TELEGRAM_BOT_TOKEN", "environment-secret")
	snapshot, err := app.configEditSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.file.AppName != "file" || snapshot.effective.AppName != "environment" {
		t.Fatal("editable and effective sources confused")
	}
	other := &App{}
	other.runtime.Store(app.runtimeSnapshot())
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for _, editor := range []*App{app, other} {
		wg.Add(1)
		go func(editor *App) {
			defer wg.Done()
			_, status, _ := editor.patchConfigSections(snapshot.revision, map[string]any{"Global": map[string]any{"server_name": "saved"}, "Telegram": map[string]any{"bot_token": secretMaskValue}})
			statuses <- status
		}(editor)
	}
	wg.Wait()
	close(statuses)
	success, conflict := 0, 0
	for status := range statuses {
		if status == http.StatusOK {
			success++
		}
		if status == http.StatusConflict {
			conflict++
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "retained = true") || strings.Contains(string(data), "environment-secret") || !strings.Contains(string(data), "file-secret") {
		t.Fatal("save lost unknown fields or copied overrides")
	}
	file, err := config.LoadFileOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.AppName != "saved" {
		t.Fatal("editable value not saved")
	}
	if app.cfg().AppName != "environment" && other.cfg().AppName != "environment" {
		t.Fatal("effective override lost")
	}
}

func TestConfigRevisionExternalEditAndFailedActivation(t *testing.T) {
	app := newTestApp(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	app.cfg().ConfigFile = path
	content := "[Global]\nserver_name = \"original\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := app.configEditSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	_, status, _ := app.editConfig(snapshot.revision, func(configEditSnapshot) (string, error) {
		if err := os.WriteFile(path, []byte(content+"# external edit\n"), 0o600); err != nil {
			return "", err
		}
		return "[Global]\nserver_name=\"bad overwrite\"", nil
	})
	if status != http.StatusConflict {
		t.Fatalf("external edit overwritten: %d", status)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "external edit") {
		t.Fatal("external content lost")
	}
	old := app.runtimeSnapshot()
	// Invalid Redis dependency fails before runtime publication and must restore file.
	_, status, _ = app.patchConfigSections("", map[string]any{"Global": map[string]any{"redis_url": "bad://invalid"}})
	if status == http.StatusOK {
		t.Fatal("invalid dependency accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(data) {
		t.Fatal("failed save did not restore file")
	}
	if app.runtimeSnapshot() != old && app.cfg().RedisURL != old.cfg.RedisURL {
		t.Fatal("failed save polluted runtime")
	}
}

func TestConfigRevisionHTTPConflictAndMaskedOverride(t *testing.T) {
	app := newTestApp(t)
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	path := filepath.Join(t.TempDir(), "config.toml")
	app.cfg().ConfigFile = path
	if err := os.WriteFile(path, []byte("[Global]\nserver_name=\"file\"\n[Unknown]\nretained=true\n[Telegram]\nbot_token=\"file-secret\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWILIGHT_TELEGRAM_BOT_TOKEN", "environment-secret")
	response := doJSON(app, http.MethodGet, "/api/v2/admin/config/schema", "", admin)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	if strings.Contains(response.Body.String(), "file-secret") || strings.Contains(response.Body.String(), "environment-secret") {
		t.Fatal("secret leaked")
	}
	var body struct {
		Data struct {
			Revision string `json:"revision"`
			Sections []struct {
				Key    string `json:"key"`
				Fields []struct {
					Key        string `json:"key"`
					Overridden bool   `json:"overridden"`
				} `json:"fields"`
			} `json:"sections"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, section := range body.Data.Sections {
		for _, field := range section.Fields {
			if section.Key == "Telegram" && field.Key == "bot_token" {
				found = field.Overridden
			}
		}
	}
	if !found {
		t.Fatal("override not identified before masking")
	}
	content := "[Global]\nserver_name=\"saved\"\n[Unknown]\nretained=true\n"
	payload, _ := json.Marshal(map[string]any{"content": content, "expected_revision": body.Data.Revision})
	response = doJSON(app, http.MethodPut, "/api/v2/admin/config/toml", string(payload), admin)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	response = doJSON(app, http.MethodPut, "/api/v2/admin/config/toml", string(payload), admin)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "CONFIG_REVISION_CONFLICT") {
		t.Fatal(response.Body.String())
	}
	stored, _ := os.ReadFile(path)
	if !strings.Contains(string(stored), "retained = true") {
		t.Fatal("unknown field lost")
	}
}

func TestConfigHotReloadRetriesAfterWorkerDrain(t *testing.T) {
	app := newTestApp(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	app.cfg().ConfigFile = path
	content := "[Global]\nserver_name=\"after-drain\"\nsession_ttl=7201\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, finish, started := app.startSchedulerRun(context.Background(), "daily_stats")
	if !started {
		t.Fatal("worker lock unavailable")
	}
	defer finish()
	before := app.runtimeSnapshot()
	app.reloadConfigIfChanged()
	if app.runtimeSnapshot() != before {
		t.Fatal("active worker runtime replaced")
	}
	finish()
	app.configSignatureCheckedAt.Store(0)
	app.reloadConfigIfChanged()
	if app.cfg().AppName != "after-drain" {
		t.Fatal("pending config not retried")
	}
}

func TestTicketTypePersistencePreservesExtrasAndReportsFailure(t *testing.T) {
	app := newTestApp(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	app.cfg().ConfigFile = path
	if err := os.WriteFile(path, []byte("[Unknown]\nretained=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.store().AddTicketType("support"); err != nil {
		t.Fatal(err)
	}
	if status := app.persistTicketTypesFromStore(); status != http.StatusOK {
		t.Fatal(status)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "retained = true") {
		t.Fatal("extras lost")
	}
	if err := os.WriteFile(path, []byte("invalid TOML ["), 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/types/support", strings.NewReader(`{"name":"renamed"}`))
	rr := httptest.NewRecorder()
	app.handleV2AdminRenameTicketType(rr, req, Params{"ticket_type": "support"})
	if rr.Code == http.StatusOK || !strings.Contains(rr.Body.String(), "CONFIG_SAVE_FAILED") {
		t.Fatal("false success", rr.Body.String())
	}
}

func TestMigrationConfigRollbackAndPrimaryValues(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	uploads := filepath.Join(dir, "uploads")
	app.cfg().ConfigFile = path
	content := "[Global]\nserver_name=\"before-import\"\n[API]\nupload_folder=" + tomlValue(filepath.ToSlash(uploads)) + "\n[Unknown]\nretained=true\n[Telegram]\nbot_token=\"file-secret\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TWILIGHT_TELEGRAM_BOT_TOKEN", "environment-secret")
	if _, err := app.reloadConfig(); err != nil {
		t.Fatal(err)
	}
	archive := migration.Archive{Files: map[string][]byte{"config/effective.toml": []byte("[Global]\nserver_name=\"imported\"\n[Telegram]\nbot_token=" + tomlValue(secretMaskValue) + "\n")}}
	prepared, _, err := app.prepareMigrationConfig(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prepared, "retained = true") || strings.Contains(prepared, "environment-secret") || !strings.Contains(prepared, "file-secret") {
		t.Fatal("migration copied override or lost extras")
	}
	target := filepath.Join(uploads, "avatar", "sample.png")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old-resource"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := migrationResourcePlan{Entries: []migrationResourcePlanEntry{{TargetPath: target, Exists: true, Data: []byte("new-resource")}}}
	request := httptest.NewRequest(http.MethodPost, "/migration/import", nil)
	// Missing business files force database import to fail after resource writes.
	status, _ := app.applyMigrationImport(request, archive, plan, migrationImportOptions{ApplyConfig: true, ResourceMode: migrationResourceModeReplace})
	if status == http.StatusOK {
		t.Fatal("invalid archive imported")
	}
	current, _ := os.ReadFile(path)
	resource, _ := os.ReadFile(target)
	if string(current) != content || string(resource) != "old-resource" || app.cfg().AppName != "before-import" {
		t.Fatal("failed import not rolled back")
	}
	if err := os.WriteFile(path, []byte("external editor"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rollbackConfigCandidate(path, "our candidate", []byte(content), true); err == nil {
		t.Fatal("rollback accepted newer external edit")
	}
	current, _ = os.ReadFile(path)
	if string(current) != "external editor" {
		t.Fatal("rollback overwrote newer edit")
	}
}
