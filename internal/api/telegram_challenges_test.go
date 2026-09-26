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
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func issueRegisterChallenge(t *testing.T, app *App) (string, string, []*http.Cookie) {
	t.Helper()
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:challenge-test"
	rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/registration/telegram/bind-code", "", nil, bindCodeCreateTestHeaders())
	if rr.Code != 200 {
		t.Fatalf("issue: %d %s", rr.Code, rr.Body.String())
	}
	var response struct {
		Data struct {
			Token string `json:"bind_code"`
			ID    string `json:"challenge_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	token, id := response.Data.Token, response.Data.ID
	if len(token) != 32 || len(id) != 32 || token == id {
		t.Fatalf("invalid separated credentials")
	}
	cookie := findCookie(rr.Result().Cookies(), telegramBrowserCookie)
	if cookie == nil || !cookie.HttpOnly || cookie.Path != "/api" || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 600 {
		t.Fatalf("browser proof cookie missing protections")
	}
	if strings.Contains(rr.Body.String(), cookie.Value) {
		t.Fatal("proof leaked in body")
	}
	return token, id, rr.Result().Cookies()
}

func TestTelegramChallengeBrowserOwnershipAndIndependentBot(t *testing.T) {
	app := newTestApp(t)
	token, id, cookies := issueRegisterChallenge(t, app)
	bot, err := New(*app.cfg(), reopenTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	// Neither an observed resource ID nor a stolen Bot code authorizes browser reads.
	for _, key := range []string{id, token} {
		rr := doJSON(app, http.MethodGet, "/api/v2/registration/telegram/bind-code/status?code="+key, "", nil)
		if !strings.Contains(rr.Body.String(), `"invalid":true`) || strings.Contains(rr.Body.String(), `"telegram_id"`) {
			t.Fatalf("unauthorized status: %s", rr.Body.String())
		}
	}
	if result := bot.confirmTelegramChallenge(context.Background(), id, 321, "alice"); result.Success {
		t.Fatal("resource ID confirmed")
	}
	result := bot.confirmTelegramChallenge(context.Background(), token, 321, "alice")
	if !result.Success {
		t.Fatalf("separate Bot confirm: %+v", result)
	}
	if result := bot.confirmTelegramChallenge(context.Background(), token, 322, "attacker"); result.Success {
		t.Fatal("identity replay succeeded")
	}
	rr := doJSON(app, http.MethodGet, "/api/v2/registration/telegram/bind-code/status?code="+id, "", cookies)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"confirmed":true`) {
		t.Fatalf("shared status: %s", rr.Body.String())
	}
	payload := fmt.Sprintf(`{"username":"alice","password":"Alice123456","telegram_bind_code":%q}`, id)
	denied := doJSON(app, http.MethodPost, "/api/v2/registration", payload, nil)
	if denied.Code != 400 || app.store().UserCount() != 0 {
		t.Fatalf("registered without browser proof: %s", denied.Body.String())
	}
	registered := doJSON(app, http.MethodPost, "/api/v2/registration", payload, cookies)
	if registered.Code != 201 {
		t.Fatalf("registration: %s", registered.Body.String())
	}
	state, err := bot.store().TelegramChallenge(context.Background(), id)
	if err != nil || state.State != "consumed" {
		t.Fatalf("challenge not consumed: %v", err)
	}
}

func TestTelegramChallengeSeparateBotWakesBoundedLongPoll(t *testing.T) {
	app := newTestApp(t)
	token, id, cookies := issueRegisterChallenge(t, app)
	bot, err := New(*app.cfg(), reopenTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v2/registration/telegram/bind-code/status?code="+id+"&wait=60", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { app.handleBindCodeStatus(rr, req, nil); close(done) }()
	deadline := time.Now().Add(time.Second)
	for {
		app.bindStatus.mu.Lock()
		watching := len(app.bindStatus.watchers[id]) > 0
		app.bindStatus.mu.Unlock()
		if watching {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll did not subscribe")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if result := bot.confirmTelegramChallenge(context.Background(), token, 323, "bob"); !result.Success {
		t.Fatalf("confirm: %+v", result)
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("cross-process result waited on local-only hub")
	}
	if !strings.Contains(rr.Body.String(), `"confirmed":true`) {
		t.Fatalf("poll state: %s", rr.Body.String())
	}
}

func TestTelegramChallengeTemporaryMembershipFailureCanRetry(t *testing.T) {
	app := newTestApp(t)
	token, id, cookies := issueRegisterChallenge(t, app)
	app.cfg().TelegramGroupIDs = []string{"-1001"}
	app.cfg().TelegramForceBindGroup = true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	defer upstream.Close()
	app.cfg().TelegramAPIURL = upstream.URL
	result := app.confirmTelegramChallenge(context.Background(), token, 324, "retry")
	if result.Code != 502 {
		t.Fatalf("check: %+v", result)
	}
	rr := doJSON(app, http.MethodGet, "/api/v2/registration/telegram/bind-code/status?code="+id, "", cookies)
	if !strings.Contains(rr.Body.String(), `"terminal":false`) || !strings.Contains(rr.Body.String(), ErrTGBindGroupCheckFailed) {
		t.Fatalf("temporary failure terminal: %s", rr.Body.String())
	}
	app.cfg().TelegramForceBindGroup = false
	if result = app.confirmTelegramChallenge(context.Background(), token, 324, "retry"); !result.Success {
		t.Fatalf("retry: %+v", result)
	}
}

func TestTelegramChallengeStatusDatabaseFailureIsRetryable(t *testing.T) {
	app := newTestApp(t)
	_, id, cookies := issueRegisterChallenge(t, app)
	if err := app.store().Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v2/registration/telegram/bind-code/status?code="+id, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	app.handleBindCodeStatus(rr, req, nil)
	if rr.Code != 503 || strings.Contains(rr.Body.String(), ErrTGBindCodeExpired) || strings.Contains(rr.Body.String(), "database is closed") {
		t.Fatalf("database error misclassified or exposed: %s", rr.Body.String())
	}
}

func TestTelegramChallengeAccountOwnership(t *testing.T) {
	app := newTestApp(t)
	cookies := registerAndLogin(t, app, "alice", "Alice123456")
	otherCookies := registerAndLogin(t, app, "bob", "Bob123456")
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:challenge-test"
	rr := doJSONWithHeaders(app, http.MethodGet, "/api/v2/me/telegram/bind-code", "", cookies, bindCodeCreateTestHeaders())
	var result struct {
		Data struct {
			ID string `json:"challenge_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil || result.Data.ID == "" {
		t.Fatalf("issue: %s", rr.Body.String())
	}
	rr = doJSON(app, http.MethodGet, "/api/v2/me/telegram/bind-code/status?code="+result.Data.ID, "", otherCookies)
	if !strings.Contains(rr.Body.String(), `"invalid":true`) {
		t.Fatalf("other account observed challenge: %s", rr.Body.String())
	}
	c, err := app.store().TelegramChallenge(context.Background(), result.Data.ID)
	if err != nil || c.OwnerHash != "" || c.Scene != "user" {
		t.Fatalf("account challenge ownership: %+v %v", c, err)
	}
	if _, _, _, err = app.store().RegisterWithTelegramChallenge(context.Background(), store.User{Username: "bad"}, "", c.ID, c.OwnerHash, nil); err == nil {
		t.Fatal("account challenge used for registration")
	}
}

// This helper runs in a genuinely separate process with its own App, Store and
// hub. It never calls newTestApp (which would reset the shared test database).
func TestTelegramChallengeBotProcessHelper(t *testing.T) {
	if os.Getenv("TWILIGHT_CHALLENGE_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	var input struct {
		Config config.Config
		Token  string
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenPostgres(context.Background(), testDSN)
	if err != nil {
		t.Fatal("open child database failed")
	}
	defer st.Close()
	app, err := New(input.Config, st)
	if err != nil {
		t.Fatal("open child App failed")
	}
	app.telegramConfirmBindCode(context.Background(), 700, 700, "process_user", input.Token)
	c, err := st.TelegramChallenge(context.Background(), input.Token)
	if err != nil || c.State != "verified" || c.TelegramID != 700 {
		t.Fatal("child Bot failed to confirm shared challenge")
	}
}

func TestTelegramChallengeActualBotProcess(t *testing.T) {
	app := newTestApp(t)
	token, id, cookies := issueRegisterChallenge(t, app)
	messages := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			t.Error("unexpected Telegram call")
			w.WriteHeader(404)
			return
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		messages <- body.Text
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer upstream.Close()
	cfg := *app.cfg()
	cfg.TelegramAPIURL = upstream.URL
	cfg.Port = 1
	input, err := json.Marshal(struct {
		Config config.Config
		Token  string
	}{cfg, token})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestTelegramChallengeBotProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "TWILIGHT_CHALLENGE_TEST_CHILD=1")
	cmd.Stdin = bytes.NewReader(input)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child failed: %v %s", err, output)
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "绑定已确认") {
			t.Fatalf("Bot reply: %s", message)
		}
	default:
		t.Fatal("Bot did not reply")
	}
	rr := doJSON(app, http.MethodGet, "/api/v2/registration/telegram/bind-code/status?code="+id, "", cookies)
	if !strings.Contains(rr.Body.String(), `"confirmed":true`) {
		t.Fatalf("parent API cannot observe child confirmation: %s", rr.Body.String())
	}
}

func TestTelegramChallengeDiagnosticsExcludeRawErrors(t *testing.T) {
	core, entries := observer.New(zap.WarnLevel)
	restore := zap.ReplaceGlobals(zap.New(core))
	defer restore()
	logTelegramChallengeFailure("confirm", fmt.Errorf("postgres://private-user:private-password@private-host/db token=private-token"))
	logTelegramChallengeFailure("confirm", context.DeadlineExceeded)
	logTelegramChallengeFailure("confirm", context.Canceled)
	logs := entries.All()
	if len(logs) != 2 {
		t.Fatalf("unexpected entries: %d", len(logs))
	}
	for _, entry := range logs {
		encoded, _ := json.Marshal(entry.ContextMap())
		if strings.Contains(string(encoded), "private-") {
			t.Fatal("raw diagnostic escaped")
		}
		if entry.ContextMap()["operation"] != "confirm" {
			t.Fatal("missing operation")
		}
	}
	if logs[1].ContextMap()["failure_kind"] != "timeout" {
		t.Fatal("timeout not classified")
	}
}
