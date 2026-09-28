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
