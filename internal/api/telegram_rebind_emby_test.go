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

	"github.com/prejudice-studio/twilight/internal/store"
)

// fakeEmbyPolicyServer 记录每个 Emby 用户当前的 IsDisabled。
type fakeEmbyPolicyServer struct {
	*httptest.Server
	mu       sync.Mutex
	disabled map[string]bool
	posts    int
}

func newFakeEmbyPolicyServer(t *testing.T, app *App, ids ...string) *fakeEmbyPolicyServer {
	t.Helper()
	f := &fakeEmbyPolicyServer{disabled: map[string]bool{}}
	for _, id := range ids {
		f.disabled[id] = false
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		for id := range f.disabled {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/Users/"+id:
				_, _ = fmt.Fprintf(w, `{"Id":%q,"Name":%q,"Policy":{"IsDisabled":%t}}`, id, id, f.disabled[id])
				return
			case r.Method == http.MethodPost && r.URL.Path == "/Users/"+id+"/Policy":
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				f.disabled[id] = body["IsDisabled"] == true
				f.posts++
				_, _ = w.Write([]byte(`{}`))
				return
			}
		}
		t.Errorf("unexpected Emby request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(f.Close)
	app.cfg().EmbyURL = f.URL
	app.cfg().EmbyToken = "emby-token"
	return f
}

func (f *fakeEmbyPolicyServer) isDisabled(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disabled[id]
}

func (f *fakeEmbyPolicyServer) setDisabled(id string, disabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disabled[id] = disabled
}

// approvedRebindUser 建一个已绑 Telegram 与 Emby、且换绑申请已获批的普通用户。
func approvedRebindUser(t *testing.T, app *App, username, embyID string, embyDisabled bool) (store.User, []*http.Cookie) {
	t.Helper()
	cookies := registerAndLogin(t, app, username, "User123456")
	u, _ := app.store().FindUserByUsername(username)
	u, err := app.store().UpdateUser(u.UID, func(u *store.User) error {
		u.TelegramID, u.TelegramUsername = 111, "old"
		u.EmbyID, u.EmbyUsername, u.EmbyDisabled = embyID, username, embyDisabled
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := app.store().CreateRebindRequest(store.RebindRequest{UID: u.UID, OldTelegramID: 111})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().ReviewRebindRequest(request.ID, 1, "approved", "ok"); err != nil {
		t.Fatal(err)
	}
	return u, cookies
}

// 需求：解绑后立即禁用 Emby；换绑完成前任何路径都不能放开；在 Bot 里确认新
// Telegram 后，服务端直接结束换绑并恢复 Emby（不依赖网页再调用 rebind-complete）。
func TestTelegramRebindSuspendsEmbyUntilNewTelegramConfirmed(t *testing.T) {
	app := newTestApp(t)
	adminCookies := registerAndLogin(t, app, "admin", "Admin123456")
	emby := newFakeEmbyPolicyServer(t, app, "emby-rebind")
	newFakeTelegramServer(t, app)
	u, cookies := approvedRebindUser(t, app, "rebinder", "emby-rebind", false)
	headers := map[string]string{"X-Twilight-Client": "webui"}

	rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/telegram/unbind", "", cookies, headers)
	if rr.Code != http.StatusOK {
		t.Fatalf("unbind: %d %s", rr.Code, rr.Body.String())
	}
	if !emby.isDisabled("emby-rebind") {
		t.Fatal("emby must be disabled immediately after unbind")
	}
	got, _ := app.store().User(u.UID)
	if !got.RebindingInProgress || !got.RebindEmbySuspended || !got.EmbyDisabled {
		t.Fatalf("rebind state not recorded: %+v", got)
	}

	// 换绑期间管理员启用 Emby 也会被拒绝。
	enable := doJSONWithHeaders(app, http.MethodPost, fmt.Sprintf("/api/v1/admin/users/%d/emby/enable", u.UID), "", adminCookies, headers)
	if enable.Code == http.StatusOK || !emby.isDisabled("emby-rebind") {
		t.Fatalf("admin enable must be blocked during rebind: %d %s", enable.Code, enable.Body.String())
	}

	// 用户签发链接，Bot 进程确认新 Telegram。
	link := issueUserTelegramLink(t, app, cookies)
	bot := newBotApp(t, app)
	if result := bot.confirmTelegramLink(context.Background(), link.Token, 222, "new"); !result.Success {
		t.Fatalf("bot confirm: %+v", result)
	}
	if emby.isDisabled("emby-rebind") {
		t.Fatal("emby must be re-enabled once the new Telegram is confirmed")
	}
	status := doJSON(app, http.MethodGet, "/api/v1/users/me", "", cookies)
	if !strings.Contains(status.Body.String(), `"rebinding_in_progress":false`) || !strings.Contains(status.Body.String(), `"telegram_id":222`) {
		t.Fatalf("rebind not finished server-side: %s", status.Body.String())
	}
	final, _ := app.store().User(u.UID)
	if final.RebindEmbySuspended || final.EmbyDisabled {
		t.Fatalf("suspension markers not cleared: %+v", final)
	}
	// 网页随后再调用 rebind-complete 仍是幂等成功。
	if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/me/telegram/rebind-complete", "", cookies, headers); rr.Code != http.StatusOK {
		t.Fatalf("rebind-complete after bot finish: %d %s", rr.Code, rr.Body.String())
	}
}

// 换绑前就被管理员单独封禁的 Emby，换绑结束后不能被顺手解封。
func TestTelegramRebindKeepsPreviouslyDisabledEmbyDisabled(t *testing.T) {
	app := newTestApp(t)
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	emby := newFakeEmbyPolicyServer(t, app, "emby-banned")
	emby.setDisabled("emby-banned", true)
	newFakeTelegramServer(t, app)
	u, cookies := approvedRebindUser(t, app, "banned", "emby-banned", true)

	rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/telegram/unbind", "", cookies, map[string]string{"X-Twilight-Client": "webui"})
	if rr.Code != http.StatusOK {
		t.Fatalf("unbind: %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := app.store().User(u.UID); got.RebindEmbySuspended {
		t.Fatalf("pre-disabled emby must not be marked as rebind-suspended: %+v", got)
	}
	link := issueUserTelegramLink(t, app, cookies)
	if result := app.confirmTelegramLink(context.Background(), link.Token, 333, "new"); !result.Success {
		t.Fatalf("confirm: %+v", result)
	}
	if !emby.isDisabled("emby-banned") {
		t.Fatal("rebind completion re-enabled an Emby account that was banned before the rebind")
	}
	if got, _ := app.store().User(u.UID); got.RebindingInProgress {
		t.Fatalf("rebind should still finish: %+v", got)
	}
}

// 用户在 Bot 里确认后直接关掉网页、之后才回来：守卫页调用 rebind-complete 必须成功，
// 不能因为“已绑定”无法再签发链接而卡死。这里模拟服务端自动结束失败（群组资格暂时
// 校验不过）的情况，由网页补完。
func TestTelegramRebindCompleteEndpointFinishesAfterMembershipRecovers(t *testing.T) {
	app := newTestApp(t)
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	emby := newFakeEmbyPolicyServer(t, app, "emby-late")
	tg := newFakeTelegramServer(t, app)
	u, cookies := approvedRebindUser(t, app, "latecomer", "emby-late", false)
	headers := map[string]string{"X-Twilight-Client": "webui"}
	if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/telegram/unbind", "", cookies, headers); rr.Code != http.StatusOK {
		t.Fatalf("unbind: %d %s", rr.Code, rr.Body.String())
	}
	// 直接写入新身份，模拟“绑定已写入但换绑尚未结束”。
	if _, err := app.store().UpdateUser(u.UID, func(u *store.User) error { u.TelegramID = 444; return nil }); err != nil {
		t.Fatal(err)
	}
	app.cfg().TelegramForceBindGroup = true
	app.cfg().TelegramGroupIDs = []string{"-1001"}
	tg.mu.Lock()
	tg.membership = "left"
	tg.mu.Unlock()
	if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/me/telegram/rebind-complete", "", cookies, headers); rr.Code != http.StatusForbidden {
		t.Fatalf("missing membership must block completion: %d %s", rr.Code, rr.Body.String())
	}
	if !emby.isDisabled("emby-late") {
		t.Fatal("emby must stay disabled while rebind is unfinished")
	}
	tg.mu.Lock()
	tg.membership = "member"
	tg.mu.Unlock()
	if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/me/telegram/rebind-complete", "", cookies, headers); rr.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", rr.Code, rr.Body.String())
	}
	if emby.isDisabled("emby-late") {
		t.Fatal("emby must be restored after rebind completes")
	}
}

// 系统强制绑定 Telegram 时，管理员解绑普通用户等同发起换绑。
func TestAdminUnbindStartsRebindWhenTelegramBindingForced(t *testing.T) {
	app := newTestApp(t)
	adminCookies := registerAndLogin(t, app, "admin", "Admin123456")
	emby := newFakeEmbyPolicyServer(t, app, "emby-admin-unbind")
	_ = registerAndLogin(t, app, "member", "User123456")
	app.cfg().ForceBindTelegram = true
	u, _ := app.store().FindUserByUsername("member")
	if _, err := app.store().UpdateUser(u.UID, func(u *store.User) error {
		u.TelegramID, u.EmbyID = 555, "emby-admin-unbind"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rr := doJSONWithHeaders(app, http.MethodPost, fmt.Sprintf("/api/v1/admin/users/%d/unbind-telegram", u.UID), "", adminCookies, map[string]string{"X-Twilight-Client": "webui"})
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"rebind_required":true`) {
		t.Fatalf("admin unbind: %d %s", rr.Code, rr.Body.String())
	}
	got, _ := app.store().User(u.UID)
	if got.TelegramID != 0 || !got.RebindingInProgress || !got.RebindEmbySuspended || !emby.isDisabled("emby-admin-unbind") {
		t.Fatalf("admin unbind did not start a forced rebind: %+v", got)
	}
	history, _ := app.store().GetTelegramIdentityHistory(context.Background(), u.UID, 10)
	if len(history) != 1 || history[0].ChangeType != "admin_unbind" {
		t.Fatalf("identity history: %+v", history)
	}
}
