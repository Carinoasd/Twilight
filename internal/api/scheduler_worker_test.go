package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/config"
	"github.com/prejudice-studio/twilight/internal/store"
)

func TestSchedulerWorkerProcessHelper(t *testing.T) {
	if os.Getenv("TWILIGHT_SCHEDULER_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	var cfg config.Config
	if err := json.NewDecoder(os.Stdin).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := store.OpenPostgres(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	app, err := New(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.RunScheduler(ctx) }()
	for ctx.Err() == nil {
		view, err := st.ReadSchedulerHistory(ctx, "emby_sync", 1)
		if err != nil {
			t.Fatal(err)
		}
		if runs := view.Runs["emby_sync"].Runs; len(runs) > 0 && !store.SchedulerRunActive(runs[0].Status) {
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("worker did not finish")
}

func startSchedulerChild(t *testing.T, cfg config.Config) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestSchedulerWorkerProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "TWILIGHT_SCHEDULER_TEST_CHILD=1")
	cmd.Stdin = bytes.NewReader(data)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, &output
}

func TestSchedulerWorkerCrossProcessCancellationAndCrash(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprint("crash=", crash), func(t *testing.T) {
			app := newTestApp(t)
			admin := registerAndLogin(t, app, "admin", "Admin123456")
			entered := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-r.Context().Done()
			}))
			defer upstream.Close()
			cfg := *app.cfg()
			cfg.SchedulerEnabled = false
			cfg.EmbyURL = upstream.URL
			cfg.EmbyToken = "test-only"
			cfg.ConfigFile = "" // no files or secrets in child
			response := doJSON(app, http.MethodPost, "/api/v2/admin/scheduler/jobs/emby_sync/run", `{"runtime_params":{"max_users":1}}`, admin)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"queued"`) {
				t.Fatalf("enqueue: %d %s", response.Code, response.Body.String())
			}
			child, output := startSchedulerChild(t, cfg)
			select {
			case <-entered:
			case <-time.After(8 * time.Second):
				_ = child.Process.Kill()
				_ = child.Wait()
				t.Fatalf("child did not execute: %s", output.String())
			}
			if crash {
				_ = child.Process.Kill()
				_ = child.Wait()
				if _, err := app.store().DB().Exec(`UPDATE twilight_job_runs SET lease_until=1 WHERE job_id='emby_sync'`); err != nil {
					t.Fatal(err)
				}
				child, output = startSchedulerChild(t, cfg)
			} else {
				response = doJSON(app, http.MethodPost, "/api/v2/admin/scheduler/jobs/emby_sync/terminate", `{}`, admin)
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cancel_requested":true`) || !strings.Contains(response.Body.String(), `"terminated":false`) {
					t.Fatalf("cancel response: %s", response.Body.String())
				}
				response = doJSON(app, http.MethodPost, "/api/v2/admin/scheduler/jobs/emby_sync/run", `{}`, admin)
				if response.Code != http.StatusConflict {
					t.Fatalf("cancel released exclusion: %d", response.Code)
				}
			}
			if err := child.Wait(); err != nil {
				t.Fatalf("worker: %v %s", err, output.String())
			}
			runs := schedulerHistoryForTest(t, app, "emby_sync")
			expected := "cancelled"
			if crash {
				expected = "interrupted"
			}
			if len(runs) != 1 || runs[0].Status != expected {
				t.Fatalf("unexpected completion: %#v", runs)
			}
			if runs[0].ConfigRevision == "" {
				t.Fatal("run lost execution config fingerprint")
			}
		})
	}
}

func TestSchedulerScheduleHTTPRevisionAndManualInvariant(t *testing.T) {
	app := newTestApp(t)
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	path := "/api/v2/admin/scheduler/jobs/cleanup_emby_devices/schedule"
	payload := `{"type":"interval","seconds":120,"expected_revision":0,"runtime_params":{"dry_run":true,"max_workers":2}}`
	response := doJSON(app, http.MethodPut, path, payload, admin)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"manual"`) {
		t.Fatal(response.Body.String())
	}
	response = doJSON(app, http.MethodPut, path, payload, admin)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "SCHEDULER_REVISION_CONFLICT") {
		t.Fatal(response.Body.String())
	}
	response = doJSON(app, http.MethodGet, "/api/v2/admin/scheduler/jobs", "", admin)
	var result struct {
		Data struct {
			Jobs []struct {
				ID     string         `json:"id"`
				Params map[string]any `json:"runtime_params"`
				Custom bool           `json:"is_custom"`
			} `json:"jobs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, job := range result.Data.Jobs {
		if job.ID == "cleanup_emby_devices" {
			found = job.Custom && numeric(job.Params["max_workers"]) == 2
		}
	}
	if !found {
		t.Fatal("manual job custom params not displayed")
	}
	response = doJSON(app, http.MethodDelete, path+"?expected_revision=0", "", admin)
	if response.Code != http.StatusConflict {
		t.Fatal(response.Body.String())
	}
	response = doJSON(app, http.MethodDelete, path+"?expected_revision=9007199254740992", "", admin)
	if response.Code != http.StatusBadRequest {
		t.Fatal(response.Body.String())
	}
	response = doJSON(app, http.MethodPut, path, `{"expected_revision":"1"}`, admin)
	if response.Code != http.StatusBadRequest {
		t.Fatal(response.Body.String())
	}
}

func TestSchedulerQueuedParametersAndRuntimeSnapshot(t *testing.T) {
	app := newTestApp(t)
	_, err := app.store().SetSchedulerScheduleWithParams("cleanup_ticket_images", map[string]any{"type": "interval", "seconds": 120}, map[string]any{"retention_days": 7}, true)
	if err != nil {
		t.Fatal(err)
	}
	run, created, err := app.enqueueSchedulerJob(context.Background(), "cleanup_ticket_images", nil, "manual")
	if err != nil || !created {
		t.Fatal(err)
	}
	_, err = app.store().SetSchedulerScheduleWithParams("cleanup_ticket_images", nil, map[string]any{"retention_days": 99}, true)
	if err != nil {
		t.Fatal(err)
	}
	frozen := app.schedulerExecutionApp()
	oldName := frozen.cfg().AppName
	next := *app.runtimeSnapshot()
	next.cfg.AppName = "updated runtime"
	app.runtime.Store(&next)
	if frozen.cfg().AppName != oldName {
		t.Fatal("running snapshot changed")
	}
	req := httptest.NewRequest(http.MethodPost, "/scheduler/internal", nil).WithContext(context.WithValue(context.Background(), schedulerFrozenParamsKey{}, run.Params))
	if numeric(frozen.schedulerEffectiveParams(req, run.JobID)["retention_days"]) != 7 {
		t.Fatal("queued parameters changed")
	}
}
