package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// fakeTelegramServer 模拟 getMe / getChatMember / sendMessage 三个端点。
type fakeTelegramServer struct {
	*httptest.Server
	mu         sync.Mutex
	memberErr  bool
	membership string
	sent       []string
}

func newFakeTelegramServer(t *testing.T, app *App) *fakeTelegramServer {
	t.Helper()
	f := &fakeTelegramServer{membership: "member"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"username":"twilight_test_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/getChatMember"):
			if f.memberErr {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"ok":false,"description":"upstream down"}`))
				return
			}
			_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"status":%q,"user":{"id":1,"is_bot":false}}}`, f.membership)))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			f.sent = append(f.sent, asString(body["text"]))
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
		default:
			t.Errorf("unexpected telegram path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:link-test"
	app.cfg().TelegramAPIURL = f.URL
	return f
}

func (f *fakeTelegramServer) lastMessage() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

func newBotApp(t *testing.T, app *App) *App {
	t.Helper()
	bot, err := New(*app.cfg(), reopenTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	return bot
}

func TestTelegramLinkRegistrationFlowAcrossProcesses(t *testing.T) {
	app := newTestApp(t)
	newFakeTelegramServer(t, app)
	app.cfg().ForceBindTelegram = true
	link := issueRegisterTelegramLink(t, app)
	if link.DeepLink != "https://t.me/twilight_test_bot?start="+link.Token || link.Manual != "/bind "+link.Token {
		t.Fatalf("deep link not built from bot identity: %+v", link)
	}
	if strings.Contains(link.Response.Body.String(), `"link_secret":"`+link.ID) || strings.Contains(link.Response.Body.String(), "Set-Cookie") {
		t.Fatal("secret must be an independent value and never a cookie")
	}

	// 没有 secret / 错误 secret / 用 token 当 ID：一律 not_found，且不泄露身份。
	for _, secret := range []string{"", "0000", strings.Repeat("f", 64)} {
		rr := registerLinkStatus(app, link.ID, secret)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"not_found"`) || strings.Contains(rr.Body.String(), "telegram_id") {
			t.Fatalf("unauthorized status (%q): %d %s", secret, rr.Code, rr.Body.String())
		}
	}
	if rr := registerLinkStatus(app, link.Token, link.Secret); !strings.Contains(rr.Body.String(), `"status":"not_found"`) {
		t.Fatalf("token accepted as resource id: %s", rr.Body.String())
	}
	if rr := registerLinkStatus(app, link.ID, link.Secret); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"pending"`) {
		t.Fatalf("owner status: %d %s", rr.Code, rr.Body.String())
	}

	// 独立 Bot 进程确认：资源 ID 不能当 token；另一个 Telegram 不能重放。
	bot := newBotApp(t, app)
	if result := bot.confirmTelegramLink(context.Background(), link.ID, 321, "alice"); result.Success {
		t.Fatal("resource id confirmed")
	}
	if result := bot.confirmTelegramLink(context.Background(), link.Token, 321, "alice"); !result.Success || result.Scene != "register" {
		t.Fatalf("bot confirm: %+v", result)
	}
	if result := bot.confirmTelegramLink(context.Background(), link.Token, 322, "mallory"); result.Success || result.Code != http.StatusConflict {
		t.Fatalf("identity replay: %+v", result)
	}
	rr := registerLinkStatus(app, link.ID, link.Secret)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"confirmed"`) || !strings.Contains(rr.Body.String(), `"telegram_id":321`) || strings.Contains(rr.Body.String(), `"telegram_bound":true`) {
		t.Fatalf("confirmed status: %s", rr.Body.String())
	}

	// 注册：secret 错 → 拒绝；正确 → 创建；重放 → 拒绝。
	register := func(secret string, username string) *httptest.ResponseRecorder {
		return doJSON(app, http.MethodPost, "/api/v1/users/register", fmt.Sprintf(`{"username":%q,"password":"Alice123456","telegram_link_id":%q,"telegram_link_secret":%q}`, username, link.ID, secret), nil)
	}
	if rr := register("wrong", "alice"); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), ErrTGBindCodeNotFound) {
		t.Fatalf("foreign browser registered: %d %s", rr.Code, rr.Body.String())
	}
	if rr := register(link.Secret, "alice"); rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
	u, ok := app.store().FindUserByUsername("alice")
	if !ok || u.TelegramID != 321 || u.TelegramUsername != "alice" {
		t.Fatalf("registered user identity: %+v", u)
	}
	if rr := register(link.Secret, "alice2"); rr.Code != http.StatusBadRequest {
		t.Fatalf("consumed link reused: %d %s", rr.Code, rr.Body.String())
	}
	if rr := registerLinkStatus(app, link.ID, link.Secret); !strings.Contains(rr.Body.String(), `"status":"consumed"`) {
		t.Fatalf("consumed status: %s", rr.Body.String())
	}
}

func TestTelegramLinkAccountFlowWithSeparateBotProcess(t *testing.T) {
	app := newTestApp(t)
	tg := newFakeTelegramServer(t, app)
	cookies := registerAndLogin(t, app, "member", "User123456")
	link := issueUserTelegramLink(t, app, cookies)
	if rr := userLinkStatus(app, link.ID, cookies); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending status: %d %s", rr.Code, rr.Body.String())
	}
	// 其它账号看不到这条链接。
	other := registerAndLogin(t, app, "other", "User123456")
	if rr := userLinkStatus(app, link.ID, other); !strings.Contains(rr.Body.String(), `"status":"not_found"`) {
		t.Fatalf("foreign account observed link: %s", rr.Body.String())
	}

	// Bot 进程通过 /start <token>（deep link 落地形态）完成绑定。
	bot := newBotApp(t, app)
	bot.handleTelegramUpdate(context.Background(), &telegramUpdate{UpdateID: 1, Message: &telegramMessage{
		From: telegramUser{ID: 4242, Username: "member_tg"}, Chat: telegramChat{ID: 4242, Type: "private"}, Text: "/start " + link.Token,
	}})
	if msg := tg.lastMessage(); !strings.Contains(msg, "绑定完成") {
		t.Fatalf("bot reply: %q", msg)
	}
	rr := userLinkStatus(app, link.ID, cookies)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"confirmed"`) || !strings.Contains(rr.Body.String(), `"telegram_bound":true`) || !strings.Contains(rr.Body.String(), `"telegram_username":"member_tg"`) {
		t.Fatalf("confirmed status: %s", rr.Body.String())
	}
	status := doJSON(app, http.MethodGet, "/api/v2/telegram/status", "", cookies)
	if !strings.Contains(status.Body.String(), `"bound":true`) || !strings.Contains(status.Body.String(), `"telegram_id":4242`) {
		t.Fatalf("api process does not see bot-side binding: %s", status.Body.String())
	}
	// 已绑定后不能再签发；Bot 重放同一 token 幂等，其它 Telegram 被拒。
	if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/me/telegram/link", "", cookies, telegramLinkCreateTestHeaders()); rr.Code != http.StatusConflict {
		t.Fatalf("bound account issued link: %d %s", rr.Code, rr.Body.String())
	}
	if result := bot.confirmTelegramLink(context.Background(), link.Token, 4242, "member_tg"); !result.Success {
		t.Fatalf("idempotent replay: %+v", result)
	}
	if result := bot.confirmTelegramLink(context.Background(), link.Token, 4343, "intruder"); result.Success {
		t.Fatal("foreign identity replaced binding")
	}
	member, _ := app.store().FindUserByUsername("member")
	history, err := app.store().GetTelegramIdentityHistory(context.Background(), member.UID, 10)
	if err != nil || len(history) != 1 || history[0].ChangeType != "bind" {
		t.Fatalf("identity history: %v %v", history, err)
	}
}

func TestTelegramLinkBotAcceptsBindCommandAndBareToken(t *testing.T) {
	app := newTestApp(t)
	tg := newFakeTelegramServer(t, app)
	first := issueRegisterTelegramLink(t, app)
	app.handleTelegramUpdate(context.Background(), &telegramUpdate{UpdateID: 1, Message: &telegramMessage{
		From: telegramUser{ID: 11, Username: "first"}, Chat: telegramChat{ID: 11, Type: "private"}, Text: "/bind " + strings.ToUpper(first.Token),
	}})
	if link, _ := app.telegramLinkAlive(first.ID); !link.Confirmed() || link.TelegramID != 11 {
		t.Fatalf("/bind with upper-case token not confirmed: %#v (%q)", link, tg.lastMessage())
	}
	second := issueRegisterTelegramLink(t, app)
	app.handleTelegramUpdate(context.Background(), &telegramUpdate{UpdateID: 2, Message: &telegramMessage{
		From: telegramUser{ID: 12, Username: "second"}, Chat: telegramChat{ID: 12, Type: "private"}, Text: second.Token,
	}})
	if link, _ := app.telegramLinkAlive(second.ID); !link.Confirmed() || link.TelegramID != 12 {
		t.Fatalf("bare token not confirmed: %#v", link)
	}
	// 群聊里的 token 与私聊里的闲聊都不触发确认。
	third := issueRegisterTelegramLink(t, app)
	before := len(tg.sent)
	app.handleTelegramUpdate(context.Background(), &telegramUpdate{UpdateID: 3, Message: &telegramMessage{
		From: telegramUser{ID: 13}, Chat: telegramChat{ID: -100, Type: "supergroup"}, Text: third.Token,
	}})
	app.handleTelegramUpdate(context.Background(), &telegramUpdate{UpdateID: 4, Message: &telegramMessage{
		From: telegramUser{ID: 13}, Chat: telegramChat{ID: 13, Type: "private"}, Text: "hello there",
	}})
	if link, _ := app.telegramLinkAlive(third.ID); link.Confirmed() || len(tg.sent) != before {
		t.Fatalf("group token or chatter triggered confirmation: %#v sent=%d", link, len(tg.sent)-before)
	}
	// /start 不带参数仍是欢迎语。
	app.handleTelegramUpdate(context.Background(), &telegramUpdate{UpdateID: 5, Message: &telegramMessage{
		From: telegramUser{ID: 14}, Chat: telegramChat{ID: 14, Type: "private"}, Text: "/start",
	}})
	if msg := tg.lastMessage(); msg == "" || strings.Contains(msg, "绑定链接") {
		t.Fatalf("plain /start reply: %q", msg)
	}
}

func TestTelegramLinkMembershipFailureIsRetryable(t *testing.T) {
	app := newTestApp(t)
	tg := newFakeTelegramServer(t, app)
	app.cfg().TelegramForceBindGroup = true
	app.cfg().TelegramGroupIDs = []string{"-1001"}
	link := issueRegisterTelegramLink(t, app)

	tg.mu.Lock()
	tg.memberErr = true
	tg.mu.Unlock()
	if result := app.confirmTelegramLink(context.Background(), link.Token, 77, "late"); result.Success || result.Code != http.StatusBadGateway {
		t.Fatalf("upstream failure: %+v", result)
	}
	rr := registerLinkStatus(app, link.ID, link.Secret)
	if !strings.Contains(rr.Body.String(), `"status":"pending"`) || !strings.Contains(rr.Body.String(), `"retryable":true`) || !strings.Contains(rr.Body.String(), ErrTGBindGroupCheckFailed) {
		t.Fatalf("temporary failure should stay pending: %s", rr.Body.String())
	}

	tg.mu.Lock()
	tg.memberErr, tg.membership = false, "left"
	tg.mu.Unlock()
	if result := app.confirmTelegramLink(context.Background(), link.Token, 77, "late"); result.Success || result.Code != http.StatusForbidden {
		t.Fatalf("missing membership: %+v", result)
	}
	if rr := registerLinkStatus(app, link.ID, link.Secret); !strings.Contains(rr.Body.String(), ErrTGBindGroupMembershipRequired) || !strings.Contains(rr.Body.String(), `"status":"pending"`) {
		t.Fatalf("membership failure should stay pending: %s", rr.Body.String())
	}

	tg.mu.Lock()
	tg.membership = "member"
	tg.mu.Unlock()
	if result := app.confirmTelegramLink(context.Background(), link.Token, 77, "late"); !result.Success {
		t.Fatalf("retry after joining: %+v", result)
	}
	if rr := registerLinkStatus(app, link.ID, link.Secret); !strings.Contains(rr.Body.String(), `"status":"confirmed"`) || strings.Contains(rr.Body.String(), `"retryable":true`) {
		t.Fatalf("confirmed after retry: %s", rr.Body.String())
	}
}

func TestTelegramLinkReissueCancelsPreviousLink(t *testing.T) {
	app := newTestApp(t)
	newFakeTelegramServer(t, app)
	cookies := registerAndLogin(t, app, "reissue", "User123456")
	first := issueUserTelegramLink(t, app, cookies)
	second := issueUserTelegramLink(t, app, cookies)
	if rr := userLinkStatus(app, first.ID, cookies); !strings.Contains(rr.Body.String(), `"status":"cancelled"`) || !strings.Contains(rr.Body.String(), `"terminal":true`) {
		t.Fatalf("old link should be cancelled: %s", rr.Body.String())
	}
	if result := app.confirmTelegramLink(context.Background(), first.Token, 5, "old"); result.Success {
		t.Fatal("cancelled link confirmed")
	}
	if result := app.confirmTelegramLink(context.Background(), second.Token, 5, "new"); !result.Success {
		t.Fatalf("fresh link: %+v", result)
	}
	if rr := userLinkStatus(app, second.ID, cookies); !strings.Contains(rr.Body.String(), `"status":"confirmed"`) {
		t.Fatalf("fresh link status: %s", rr.Body.String())
	}
}

func TestTelegramLinkStatusDatabaseFailureIsNotMissing(t *testing.T) {
	app := newTestApp(t)
	link := issueRegisterTelegramLink(t, app)
	if err := app.store().Close(); err != nil {
		t.Fatal(err)
	}
	state := app.telegramLinkStatus(context.Background(), link.ID, 0, store.TelegramLinkSecretHash(link.Secret), "register", time.Now().Unix())
	if state.Status != "unavailable" || state.HTTPStatus != http.StatusServiceUnavailable || state.Terminal {
		t.Fatalf("database failure reported as %+v", state)
	}
	if result := app.confirmTelegramLink(context.Background(), link.Token, 1, "x"); result.Code != http.StatusServiceUnavailable {
		t.Fatalf("bot side database failure: %+v", result)
	}
}

// /system/info 刚好撞上一次 getMe 失败会留下 30 秒失败缓存；签发链接必须绕过它，
// 否则页面会同时出现“Bot 未配置”红字与可用的绑定流程。
func TestTelegramLinkIssueRetriesBotIdentityAfterCachedFailure(t *testing.T) {
	app := newTestApp(t)
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:link-test"
	app.cfg().TelegramAPIURL = "http://127.0.0.1:1"
	if info := app.publicTelegramBotInfo(context.Background()); info["ok"] != false {
		t.Fatalf("expected cached failure, got %+v", info)
	}
	newFakeTelegramServer(t, app)
	link := issueRegisterTelegramLink(t, app)
	if link.DeepLink != "https://t.me/twilight_test_bot?start="+link.Token {
		t.Fatalf("deep link should retry bot identity after a cached failure: %+v", link)
	}
	if info := app.publicTelegramBotInfo(context.Background()); info["username"] != "twilight_test_bot" {
		t.Fatalf("successful retry should refresh the shared cache: %+v", info)
	}
}

func TestTelegramLinkIssueWithoutBotIdentityFallsBackToManualCommand(t *testing.T) {
	app := newTestApp(t)
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:ABC"
	app.cfg().TelegramAPIURL = "http://127.0.0.1:1"
	link := issueRegisterTelegramLink(t, app)
	if link.DeepLink != "" || !strings.HasPrefix(link.Manual, "/bind ") {
		t.Fatalf("expected manual fallback without bot identity: %+v", link)
	}
}

// Bot 回复必须能区分“用户自己处理”与“需要管理员处理”的失败，并附错误码。
func TestTelegramLinkResultMessagesAreDistinguishable(t *testing.T) {
	cases := []struct {
		result telegramLinkResult
		want   []string
	}{
		{telegramLinkResult{Success: true, Scene: "register"}, []string{"完成注册"}},
		{telegramLinkResult{Success: true, Scene: "user"}, []string{"绑定完成"}},
		{telegramLinkResult{Code: http.StatusServiceUnavailable, ErrorCode: ErrTGNotConfigured}, []string{"未配置 Token", "错误码：TG_NOT_CONFIGURED"}},
		{telegramLinkResult{Code: http.StatusBadGateway, ErrorCode: ErrTGBindGroupCheckFailed}, []string{"资格校验失败", "权限", "错误码：TG_BIND_GROUP_CHECK_FAILED"}},
		{telegramLinkResult{Code: http.StatusForbidden, ErrorCode: ErrTGBindGroupMembershipRequired, Message: "绑定前需要先加入指定 Telegram 群组/频道: @g"}, []string{"@g", "再次点击", "错误码：TG_BIND_GROUP_MEMBERSHIP_REQUIRED"}},
		{telegramLinkResult{Code: http.StatusServiceUnavailable, ErrorCode: ErrBindCodeSaveFailed}, []string{"数据库", "telegram link operation failed", "错误码：BIND_CODE_SAVE_FAILED"}},
		{telegramLinkResult{Code: http.StatusConflict, ErrorCode: ErrTGAlreadyBound}, []string{"换绑审批", "错误码：TG_ALREADY_BOUND"}},
		{telegramLinkResult{Code: http.StatusConflict, ErrorCode: ErrTGBindTargetTaken}, []string{"其他账号", "重新获取", "错误码：TG_BIND_TARGET_TAKEN"}},
		{telegramLinkResult{Code: http.StatusNotFound, ErrorCode: ErrTGBindCodeNotFound}, []string{"无效或已过期", "错误码：TG_BIND_CODE_NOT_FOUND"}},
		{telegramLinkResult{Code: http.StatusTooManyRequests, ErrorCode: ErrUploadRateLimited}, []string{"频繁"}},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		got := telegramLinkResultMessage(tc.result)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Fatalf("%+v: reply %q lacks %q", tc.result, got, want)
			}
		}
		if seen[got] {
			t.Fatalf("duplicate reply for %+v: %q", tc.result, got)
		}
		seen[got] = true
	}
}
