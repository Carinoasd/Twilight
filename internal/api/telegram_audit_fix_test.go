package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/security"
	"github.com/prejudice-studio/twilight/internal/store"
)

// recordingTelegramCall 记录假 Telegram 服务器收到的一次调用。
type recordingTelegramCall struct {
	Method string
	Body   map[string]any
}

// recordingTelegramServer 是宽松版假 Telegram：所有方法都回 ok，并记录调用，
// 便于断言 Bot 发了/没发什么。getMe 的 id 可改，用于 offset 身份测试。
type recordingTelegramServer struct {
	*httptest.Server
	mu    sync.Mutex
	calls []recordingTelegramCall
	botID int64
	// updates 非空时 getUpdates 返回它一次后清空
	updates string
}

func newRecordingTelegramServer(t *testing.T, app *App) *recordingTelegramServer {
	t.Helper()
	f := &recordingTelegramServer{botID: 1}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		f.calls = append(f.calls, recordingTelegramCall{Method: method, Body: body})
		botID := f.botID
		updates := f.updates
		if method == "getUpdates" {
			f.updates = ""
		}
		f.mu.Unlock()
		switch method {
		case "getMe":
			_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"id":%d,"is_bot":true,"username":"bot%d"}}`, botID, botID)
		case "getUpdates":
			if updates == "" {
				updates = "[]"
			}
			_, _ = fmt.Fprintf(w, `{"ok":true,"result":%s}`, updates)
		case "getChatMember":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"status":"member","user":{"id":1,"is_bot":false}}}`))
		case "sendMessage":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777,"chat":{"id":1}}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(f.Close)
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:audit-fix"
	app.cfg().TelegramAPIURL = f.URL
	app.cfg().AuditLogEnabled = true
	return f
}

func (f *recordingTelegramServer) callsOf(method string) []recordingTelegramCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []recordingTelegramCall{}
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *recordingTelegramServer) sentTexts() []string {
	out := []string{}
	for _, c := range f.callsOf("sendMessage") {
		out = append(out, asString(c.Body["text"]))
	}
	return out
}

func (f *recordingTelegramServer) lastSent() string {
	texts := f.sentTexts()
	if len(texts) == 0 {
		return ""
	}
	return texts[len(texts)-1]
}

func tgTextUpdate(chatID, fromID int64, text string) *telegramUpdate {
	chatType := "private"
	if chatID != fromID {
		chatType = "supergroup"
	}
	return &telegramUpdate{Message: &telegramMessage{
		MessageID: 10,
		Chat:      telegramChat{ID: chatID, Type: chatType},
		From:      telegramUser{ID: fromID, Username: fmt.Sprintf("u%d", fromID)},
		Text:      text,
	}}
}

func mustCreateTGUser(t *testing.T, app *App, u store.User) store.User {
	t.Helper()
	created, err := app.store().CreateUser(u)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// 429 带 retry_after 时必须真的退避：用已取消的 ctx 观察函数是否进入了等待分支。
// 修复前变量遮蔽导致 d 恒为 0，直接返回 true（没有等待）。
func TestTelegramRateLimitPauseHonorsRetryAfter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := fmt.Errorf("telegram sendMessage failed: Too Many Requests [%s3]", telegramRetryAfterSentinel)
	start := time.Now()
	if telegramRateLimitPauseContext(ctx, err) {
		t.Fatal("retry_after error did not enter backoff wait")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancelled ctx should interrupt backoff promptly")
	}
}

// enable_tg_panel=false 时 /twguser 不得开出面板，已有面板按钮也不得执行。
func TestTelegramPanelDisabledBlocksTwguserAndCallbacks(t *testing.T) {
	app := newTestApp(t)
	tg := newRecordingTelegramServer(t, app)
	app.cfg().TelegramAdminIDs = []int64{42}
	app.cfg().TelegramEnablePanel = false
	target := mustCreateTGUser(t, app, store.User{Username: "panel-target", Role: store.RoleNormal, Active: true})

	app.handleTelegramUpdate(context.Background(), tgTextUpdate(-100, 42, "/twguser panel-target"))
	app.telegramPanelMu.Lock()
	panels := len(app.telegramPanels)
	app.telegramPanelMu.Unlock()
	if panels != 0 || len(tg.callsOf("sendMessage")) != 0 {
		t.Fatalf("disabled panel still opened: panels=%d sent=%v", panels, tg.sentTexts())
	}

	panel := app.telegramCreatePanel(-100, 10, target)
	panel.MessageID = 777
	app.telegramSavePanel(panel)
	app.handleTelegramUpdate(context.Background(), &telegramUpdate{CallbackQuery: &telegramCallbackQuery{
		ID:      "cb1",
		From:    telegramUser{ID: 42},
		Message: &telegramMessage{MessageID: 777, Chat: telegramChat{ID: -100, Type: "supergroup"}},
		Data:    "gadm:act:disable:" + panel.Token,
	}})
	if got, _ := app.store().User(target.UID); !got.Active {
		t.Fatal("disabled panel callback still disabled the target user")
	}
}

func auditLogsByAction(app *App, action string) []store.AuditLog {
	out := []store.AuditLog{}
	for _, entry := range app.store().ListAuditLogs() {
		if entry.Action == action {
			out = append(out, entry)
		}
	}
	return out
}

func mustHashPassword(t *testing.T, password string) string {
	t.Helper()
	hash, err := security.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// 不认得的参数只回说明，不能把无邮箱无 Emby 的账号直接删掉；动词不分大小写。
func TestTelegramDelAccountUnknownArgOnlyShowsHelp(t *testing.T) {
	app := newTestApp(t)
	tg := newRecordingTelegramServer(t, app)
	u := mustCreateTGUser(t, app, store.User{Username: "del-unknown", PasswordHash: mustHashPassword(t, "Password123456"), Role: store.RoleNormal, Active: true, TelegramID: 5101})
	ctx := context.Background()
	for _, args := range [][]string{{"help"}, {"?"}, {"我想看看"}} {
		app.telegramHandleDelAccount(ctx, 5101, 5101, args)
		if _, ok := app.store().User(u.UID); !ok {
			t.Fatalf("/delAccount %v deleted the account", args)
		}
		if app.peekDelAccountPending(5101, 5101) != nil {
			t.Fatalf("/delAccount %v started a pending deletion", args)
		}
	}
	app.telegramHandleDelAccount(ctx, 5101, 5101, []string{"Cancel"})
	if !strings.Contains(tg.lastSent(), "已取消") {
		t.Fatalf("/delAccount Cancel should be treated as cancel, got %q", tg.lastSent())
	}
}

// confirm 必须二次确认（Web 密码），原因要带到审计里。
func TestTelegramDelAccountConfirmRequiresPassword(t *testing.T) {
	app := newTestApp(t)
	newRecordingTelegramServer(t, app)
	u := mustCreateTGUser(t, app, store.User{Username: "del-confirm", PasswordHash: mustHashPassword(t, "Password123456"), Role: store.RoleNormal, Active: true, TelegramID: 5102})
	ctx := context.Background()
	app.telegramHandleDelAccount(ctx, 5102, 5102, []string{"CONFIRM", "不用了"})
	if _, ok := app.store().User(u.UID); !ok {
		t.Fatal("/delAccount confirm deleted the account without a second confirmation")
	}
	if !app.telegramConsumeDelAccountPending(ctx, 5102, 5102, "wrong-password") {
		t.Fatal("pending confirm should consume the password message")
	}
	if _, ok := app.store().User(u.UID); !ok {
		t.Fatal("wrong password deleted the account")
	}
	if !app.telegramConsumeDelAccountPending(ctx, 5102, 5102, "Password123456") {
		t.Fatal("pending confirm should consume the password message")
	}
	if _, ok := app.store().User(u.UID); ok {
		t.Fatal("correct password should delete the account")
	}
	logs := auditLogsByAction(app, "self_delete_via_telegram")
	if len(logs) != 1 || logs[0].Detail["reason"] != "不用了" {
		t.Fatalf("delete audit missing reason: %#v", logs)
	}
}

// /delAccount email <原因> 不能被当成验证码消耗；发码时的原因要带到验证后。
func TestTelegramDelAccountEmailReasonIsNotCode(t *testing.T) {
	app := newTestApp(t)
	newRecordingTelegramServer(t, app)
	cfg := app.cfg()
	cfg.EmailEnabled = true
	cfg.SMTPHost = "127.0.0.1"
	cfg.SMTPPort = 1 // 连接立即被拒，发码失败但不会卡住
	cfg.SMTPFromAddress = "noreply@example.com"
	u := mustCreateTGUser(t, app, store.User{Username: "del-email", Email: "del@example.com", EmailVerified: true, EmailVerifiedAt: time.Now().Unix(), Role: store.RoleNormal, Active: true, TelegramID: 5103})
	now := time.Now().Unix()
	rec := store.EmailVerification{ID: "delacct-rec", Purpose: emailPurposeDelAccount, Email: u.Email, UID: u.UID, MaxAttempts: 1, CreatedAt: now, ExpiresAt: now + 600, LastSentAt: now}
	rec.CodeHash = app.hashEmailCode(rec.ID, "135790")
	if err := app.store().PutEmailVerification(rec); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 原因只有一个词：修复前会被当成验证码，MaxAttempts=1 直接把真码烧掉。
	app.telegramHandleDelAccount(ctx, 5103, 5103, []string{"email", "不想用了"})
	// 模拟发码成功后留下的 pending 原因（真实发码依赖 SMTP，这里直接写入）。
	app.saveDelAccountPending(&delAccountPendingState{TelegramID: 5103, ChatID: 5103, UserUID: u.UID, Stage: delAccountStageEmail, ExpiresAt: now + 600, Reason: "不想用了"})
	app.telegramHandleDelAccount(ctx, 5103, 5103, []string{"email", "135790"})
	if _, ok := app.store().User(u.UID); ok {
		t.Fatal("valid email code should delete the account (reason must not burn the code)")
	}
	logs := auditLogsByAction(app, "self_delete_via_telegram")
	if len(logs) != 1 || logs[0].Detail["reason"] != "不想用了" {
		t.Fatalf("delete audit missing email-flow reason: %#v", logs)
	}
}

// 灾难性回溯的正则必须被限时；修复前 regexp2 不检查中断，这类调用要跑一分钟以上。
func TestDeveloperJSRegexpBacktrackingIsBounded(t *testing.T) {
	app := newTestApp(t)
	user := mustCreateTGUser(t, app, store.User{Username: "js-regex", Role: store.RoleNormal, Active: true, TelegramID: 5201, PasswordHash: "unused"})
	code := `var r = new RegExp("^(?=a)(a+)+$"); reply(String(r.test("a".repeat(34) + "!")));`
	done := make(chan string, 1)
	start := time.Now()
	go func() {
		out, _, _ := app.telegramRunJSCustomCommand(code, telegramCommandCtx{FromID: user.TelegramID}, true)
		done <- out
	}()
	select {
	case out := <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("regex took %s", elapsed)
		}
		if strings.TrimSpace(out) != "false" {
			t.Fatalf("timed-out regex should behave as no match, got %q", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("catastrophic regex was not bounded by the sandbox")
	}
}

// 单条 update 卡住时，批处理必须在上限后放行，不能拖住其它聊天室。
func TestTelegramUpdateBatchBoundsSingleUpdate(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	handle := telegramBoundedUpdateHandler(50*time.Millisecond, func(ctx context.Context, _ *telegramUpdate) {
		select {
		case <-release:
		case <-time.After(time.Minute):
		}
	})
	updates := []telegramUpdate{telegramTestMessageUpdate(1, 100), telegramTestMessageUpdate(2, 200)}
	done := make(chan struct{})
	go func() {
		processTelegramUpdateBatch(context.Background(), updates, 2, handle)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stuck update blocked the whole batch")
	}
}

// fetch 私网判定要覆盖 CGNAT、benchmark、保留网段，并解开 NAT64/6to4/Teredo 内嵌 IPv4。
func TestDeveloperJSPrivateIPCoversReservedAndTranslatedRanges(t *testing.T) {
	blocked := []string{
		"100.101.102.103",                      // CGNAT / Tailscale
		"198.18.0.1",                           // benchmark
		"192.0.0.8",                            // IETF 协议分配
		"240.0.0.1",                            // 保留
		"0.1.2.3",                              // 0.0.0.0/8
		"64:ff9b::a00:1",                       // NAT64 -> 10.0.0.1
		"64:ff9b::7f00:1",                      // NAT64 -> 127.0.0.1
		"2002:a00:1::1",                        // 6to4 -> 10.0.0.1
		"2002:a9fe:a9fe::1",                    // 6to4 -> 169.254.169.254
		"2001:0:4136:e378:8000:63bf:f5ff:fffe", // Teredo 客户端 -> 10.0.0.1
		"::a00:1",                              // IPv4-compatible
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("cannot parse %q", s)
		}
		if !developerJSPrivateIP(ip) {
			t.Errorf("expected %s to be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "64:ff9b::808:808", "2002:808:808::1", "2606:4700:4700::1111", "100.128.0.1"}
	for _, s := range allowed {
		if developerJSPrivateIP(net.ParseIP(s)) {
			t.Errorf("expected %s to be allowed", s)
		}
	}
}

// 走环境代理时，拨号层只看得到代理 IP；必须在选代理时校验真实目标主机。
func TestSharedTransportProxyGuardsTargetHost(t *testing.T) {
	proxyURL, _ := url.Parse("http://127.0.0.1:3128")
	original := sharedHTTPProxyFromEnvironment
	sharedHTTPProxyFromEnvironment = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	defer func() { sharedHTTPProxyFromEnvironment = original }()

	blocked, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data", nil)
	if got, err := sharedHTTPTransport.Proxy(blocked); err == nil {
		t.Fatalf("metadata target via proxy should be refused, got proxy %v", got)
	}
	allowed, _ := http.NewRequest(http.MethodGet, "https://93.184.216.34/", nil)
	if got, err := sharedHTTPTransport.Proxy(allowed); err != nil || got == nil || got.Host != "127.0.0.1:3128" {
		t.Fatalf("public target should use proxy, got %v err=%v", got, err)
	}
}

// 停机换 Bot（新 Token、新 getMe.id）后重启，必须从 offset 0 开始拉取。
func TestTelegramOffsetResetsWhenBotIdentityChangesAcrossRestart(t *testing.T) {
	app := newTestApp(t)
	tg := newRecordingTelegramServer(t, app)
	// 旧 Bot（id=1）已经确认到 500。
	if _, _, err := app.store().BindTelegramBotOffset(1); err != nil {
		t.Fatal(err)
	}
	if err := app.store().SetTelegramBotOffset(500); err != nil {
		t.Fatal(err)
	}
	tg.mu.Lock()
	tg.botID = 2
	tg.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = app.RunTelegramBot(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(tg.callsOf("getUpdates")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	calls := tg.callsOf("getUpdates")
	if len(calls) == 0 {
		t.Fatal("bot never polled")
	}
	if offset, _ := calls[0].Body["offset"].(float64); offset != 0 {
		t.Fatalf("new bot resumed stale offset %v, want 0", calls[0].Body["offset"])
	}
	// 同一 Bot 再次启动要沿用 offset。
	if offset, reset, err := app.store().BindTelegramBotOffset(2); err != nil || reset {
		t.Fatalf("same bot should not reset: offset=%d reset=%v err=%v", offset, reset, err)
	}
}
