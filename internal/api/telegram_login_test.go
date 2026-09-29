package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

type issuedTelegramLogin struct{ ID, Secret, Token, Code string }

func loginQRRequest(app *App, method, path, secret string) *httptest.ResponseRecorder {
	headers := telegramLinkCreateTestHeaders()
	headers[telegramLinkSecretHeader] = secret
	headers["X-Twilight-Device"] = "qr-browser-device-123456"
	headers["User-Agent"] = "Firefox QR Test"
	return doJSONWithHeaders(app, method, path, "", nil, headers)
}

func TestTelegramQRLoginSessionFailureCannotRetry(t *testing.T) {
	app := newTestApp(t)
	newFakeTelegramServer(t, app)
	app.cfg().TelegramLoginEnabled = true
	if _, err := app.store().CreateUser(store.User{Username: "qr-user", Active: true, TelegramID: 789}); err != nil {
		t.Fatal(err)
	}
	l := issueLoginQR(t, app)
	approveLoginQR(t, app, l)
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`ALTER TABLE twilight_sessions ADD CONSTRAINT qr_deny_session CHECK (uid < 0)`); err != nil {
		t.Fatal(err)
	}
	defer db.Exec(`ALTER TABLE twilight_sessions DROP CONSTRAINT IF EXISTS qr_deny_session`)
	path := "/api/v2/auth/telegram/requests/" + l.ID + "/consume"
	rr := loginQRRequest(app, "POST", path, l.Secret)
	if rr.Code != 500 || len(rr.Result().Cookies()) != 0 {
		t.Fatalf("session failure did not fail closed: %d", rr.Code)
	}
	if _, err = db.Exec(`ALTER TABLE twilight_sessions DROP CONSTRAINT qr_deny_session`); err != nil {
		t.Fatal(err)
	}
	rr = loginQRRequest(app, "POST", path, l.Secret)
	if rr.Code != 409 {
		t.Fatalf("failed login retried: %d", rr.Code)
	}
}

func TestTelegramQRLoginLogoutAllRevokesApproval(t *testing.T) {
	app := newTestApp(t)
	newFakeTelegramServer(t, app)
	app.cfg().TelegramLoginEnabled = true
	u, err := app.store().CreateUser(store.User{Username: "qr-user", Active: true, TelegramID: 789})
	if err != nil {
		t.Fatal(err)
	}
	l := issueLoginQR(t, app)
	approveLoginQR(t, app, l)
	app.revokeAllSessions(context.Background(), u.UID)
	rr := loginQRRequest(app, "POST", "/api/v2/auth/telegram/requests/"+l.ID+"/consume", l.Secret)
	if rr.Code != 409 {
		t.Fatalf("logout-all retained approval: %d", rr.Code)
	}
}
func issueLoginQR(t *testing.T, app *App) issuedTelegramLogin {
	t.Helper()
	rr := loginQRRequest(app, http.MethodPost, "/api/v2/auth/telegram/requests", "")
	if rr.Code != 200 {
		t.Fatalf("create %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Data struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
			Link   string `json:"deep_link"`
			Code   string `json:"check_code"`
		}
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(body.Data.Link)
	token := strings.TrimPrefix(u.Query().Get("start"), "login_")
	if len(body.Data.Secret) != 64 || len(token) != 32 || len(body.Data.ID) != 32 || body.Data.ID == token || len(body.Data.Code) != 6 {
		t.Fatal("invalid independent credentials")
	}
	if !strings.Contains(rr.Header().Get("Cache-Control"), "no-store") || len(rr.Result().Cookies()) != 0 {
		t.Fatal("issuance must be private and must not create session")
	}
	return issuedTelegramLogin{body.Data.ID, body.Data.Secret, token, body.Data.Code}
}
func approveLoginQR(t *testing.T, bot *App, l issuedTelegramLogin) {
	t.Helper()
	bot.handleTelegramUpdate(context.Background(), &telegramUpdate{Message: &telegramMessage{From: telegramUser{ID: 789}, Chat: telegramChat{ID: 789, Type: "private"}, Text: "/start login_" + l.Token}})
	bot.handleTelegramUpdate(context.Background(), &telegramUpdate{CallbackQuery: &telegramCallbackQuery{ID: "callback", From: telegramUser{ID: 789}, Message: &telegramMessage{Chat: telegramChat{ID: 789, Type: "private"}}, Data: "tgl:yes:" + l.ID}})
}
func TestTelegramQRLoginFullFlow(t *testing.T) {
	app := newTestApp(t)
	tg := newFakeTelegramServer(t, app)
	app.cfg().TelegramLoginEnabled = true
	user, err := app.store().CreateUser(store.User{Username: "qr-user", Active: true, TelegramID: 789})
	if err != nil {
		t.Fatal(err)
	}
	bot := newBotApp(t, app)
	l := issueLoginQR(t, app)
	path := "/api/v2/auth/telegram/requests/" + l.ID
	if rr := loginQRRequest(app, "GET", path, strings.Repeat("0", 64)); rr.Code == 200 {
		t.Fatal("foreign browser observed request")
	}
	if rr := loginQRRequest(app, "POST", path+"/consume", l.Secret); rr.Code == 200 {
		t.Fatal("unconfirmed request consumed")
	}
	approveLoginQR(t, bot, l)
	if !strings.Contains(tg.lastMessage(), l.Code) || !strings.Contains(tg.lastMessage(), "Firefox QR Test") {
		t.Fatal("missing confirmation context")
	}
	rr := loginQRRequest(app, "GET", path, l.Secret)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"status":"approved"`) || strings.Contains(rr.Body.String(), "uid") || len(rr.Result().Cookies()) != 0 {
		t.Fatalf("poll projection: %s", rr.Body.String())
	}
	rr = loginQRRequest(app, "POST", path+"/consume", l.Secret)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "qr-user") {
		t.Fatalf("consume %d %s", rr.Code, rr.Body.String())
	}
	me := doJSON(app, "GET", "/api/v2/auth/me", "", rr.Result().Cookies())
	if me.Code != 200 {
		t.Fatalf("session unusable: %s", me.Body.String())
	}
	if d, ok := app.store().Device(user.UID, "qr-browser-device-123456"); !ok || d.LastSeen == 0 {
		t.Fatal("device not recorded")
	}
	if rr = loginQRRequest(app, "POST", path+"/consume", l.Secret); rr.Code == 200 {
		t.Fatal("replay issued another session")
	}
}
func TestTelegramQRLoginFeatureGateIntentAndBlockedDevice(t *testing.T) {
	app := newTestApp(t)
	newFakeTelegramServer(t, app)
	user, err := app.store().CreateUser(store.User{Username: "qr-user", Active: true, TelegramID: 789})
	if err != nil {
		t.Fatal(err)
	}
	if rr := loginQRRequest(app, "POST", "/api/v2/auth/telegram/requests", ""); rr.Code != 403 {
		t.Fatalf("default off: %d", rr.Code)
	}
	app.cfg().TelegramLoginEnabled = true
	if rr := doJSON(app, "POST", "/api/v2/auth/telegram/requests", "", nil); rr.Code != 400 {
		t.Fatalf("missing intent accepted: %d", rr.Code)
	}
	l := issueLoginQR(t, app)
	approveLoginQR(t, app, l)
	if err = app.store().UpsertDevice(store.Device{UID: user.UID, DeviceID: "qr-browser-device-123456", Blocked: true}); err != nil {
		t.Fatal(err)
	}
	rr := loginQRRequest(app, "POST", "/api/v2/auth/telegram/requests/"+l.ID+"/consume", l.Secret)
	if rr.Code == 200 || len(rr.Result().Cookies()) != 0 {
		t.Fatalf("blocked device: %d %s", rr.Code, rr.Body.String())
	}
	if err = app.store().UpdateDevice(user.UID, "qr-browser-device-123456", func(d *store.Device) { d.Blocked = false }); err != nil {
		t.Fatal(err)
	}
	rr = loginQRRequest(app, "POST", "/api/v2/auth/telegram/requests/"+l.ID+"/consume", l.Secret)
	if rr.Code == 200 {
		t.Fatal("failure restored consumed grant")
	}
}
func TestTelegramQRLoginCallbackIdentityAndPrivateChat(t *testing.T) {
	app := newTestApp(t)
	newFakeTelegramServer(t, app)
	app.cfg().TelegramLoginEnabled = true
	if _, err := app.store().CreateUser(store.User{Username: "qr-user", Active: true, TelegramID: 789}); err != nil {
		t.Fatal(err)
	}
	l := issueLoginQR(t, app)
	app.telegramScanLogin(context.Background(), 789, l.Token)
	for _, cb := range []telegramCallbackQuery{
		{ID: "other", From: telegramUser{ID: 790}, Message: &telegramMessage{Chat: telegramChat{ID: 790, Type: "private"}}},
		{ID: "group", From: telegramUser{ID: 789}, Message: &telegramMessage{Chat: telegramChat{ID: -123, Type: "group"}}},
		{ID: "inline", From: telegramUser{ID: 789}},
	} {
		cb.Data = "tgl:yes:" + l.ID
		app.telegramHandleLoginCallback(context.Background(), &cb)
	}
	rr := loginQRRequest(app, "GET", "/api/v2/auth/telegram/requests/"+l.ID, l.Secret)
	if !strings.Contains(rr.Body.String(), `"status":"scanned"`) {
		t.Fatal("invalid callback approved login")
	}
	app.cfg().TelegramLoginEnabled = false
	if rr = loginQRRequest(app, "GET", "/api/v2/auth/telegram/requests/"+l.ID, l.Secret); rr.Code != 403 {
		t.Fatal("gate bypassed on poll")
	}
	if rr = loginQRRequest(app, "POST", "/api/v2/auth/telegram/requests/"+l.ID+"/consume", l.Secret); rr.Code != 403 {
		t.Fatal("gate bypassed on consume")
	}
}
