package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

type notificationTestMail struct{ To, Body string }

func notificationSMTP(t *testing.T, app *App) <-chan notificationTestMail {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	messages := make(chan notificationTestMail, 32)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				c := textproto.NewConn(conn)
				_ = c.PrintfLine("220 localhost test SMTP")
				for {
					line, err := c.ReadLine()
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "DATA"):
						_ = c.PrintfLine("354 send message")
						data, err := c.ReadDotBytes()
						if err != nil {
							return
						}
						msg, err := mail.ReadMessage(strings.NewReader(string(data)))
						if err != nil {
							return
						}
						var body io.Reader = msg.Body
						switch strings.ToLower(msg.Header.Get("Content-Transfer-Encoding")) {
						case "quoted-printable":
							body = quotedprintable.NewReader(body)
						case "base64":
							body = base64.NewDecoder(base64.StdEncoding, body)
						}
						decoded, _ := io.ReadAll(body)
						messages <- notificationTestMail{To: msg.Header.Get("To"), Body: string(decoded)}
						_ = c.PrintfLine("250 accepted")
					case strings.HasPrefix(line, "QUIT"):
						_ = c.PrintfLine("221 bye")
						return
					default:
						_ = c.PrintfLine("250 OK")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); workers.Wait() })
	app.cfg().EmailEnabled = true
	app.cfg().SMTPHost = "127.0.0.1"
	app.cfg().SMTPPort = listener.Addr().(*net.TCPAddr).Port
	app.cfg().SMTPFromAddress = "test@example.invalid"
	app.cfg().SMTPEncryption = "none"
	app.cfg().SMTPTimeoutSeconds = 2
	return messages
}

func expectNotificationMail(t *testing.T, messages <-chan notificationTestMail) notificationTestMail {
	t.Helper()
	select {
	case m := <-messages:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("expected notification email")
		return notificationTestMail{}
	}
}

func noNotificationMail(t *testing.T, messages <-chan notificationTestMail) {
	t.Helper()
	select {
	case m := <-messages:
		t.Fatalf("unexpected email: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNotificationExpiryChannelsIndependentAndVerified(t *testing.T) {
	app := newTestApp(t)
	messages := notificationSMTP(t, app)
	app.cfg().NotificationEnabled = true
	app.cfg().ExpiryNotifyEmailEnabled = true
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:ABC"
	var tgCalls atomic.Int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tgCalls.Add(1)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"blocked"}`))
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL
	for _, name := range []string{"eligible", "unverified", "optout", "inactive"} {
		user := store.User{Username: name, Role: store.RoleNormal, Active: name != "inactive", Email: name + "@example.invalid", EmailVerified: name != "unverified", NotifyOnExpiryEmail: name != "optout", ExpiredAt: time.Now().Add(time.Hour).Unix()}
		if name == "eligible" {
			user.TelegramID = 123
		}
		created, err := app.store().CreateUser(user)
		if err != nil {
			t.Fatal(err)
		}
		if name == "inactive" {
			if _, err := app.store().SetUserActiveAtomic(created.UID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	result := app.sendExpiryReminders(context.Background(), 3)
	if numeric(result["email_sent"]) != 1 || numeric(result["telegram_sent"]) != 0 || numeric(result["failed_count"]) != 1 || boolish(result["success"]) || !boolish(result["partial"]) {
		t.Fatalf("channel failure suppressed email: %#v", result)
	}
	if m := expectNotificationMail(t, messages); !strings.Contains(m.To, "eligible@example.invalid") {
		t.Fatalf("wrong recipient: %+v", m)
	}
	noNotificationMail(t, messages)
	app.cfg().ExpiryNotifyTelegramEnabled = false
	result = app.sendExpiryReminders(context.Background(), 3)
	if !boolish(result["success"]) || tgCalls.Load() != 1 || numeric(result["email_sent"]) != 1 {
		t.Fatalf("email-only failed: %#v", result)
	}
	expectNotificationMail(t, messages)
	app.cfg().ExpiryNotifyEmailEnabled = false
	result = app.sendExpiryReminders(context.Background(), 3)
	if numeric(result["sent"]) != 0 {
		t.Fatalf("disabled channels still sent: %#v", result)
	}
	noNotificationMail(t, messages)
}

func TestNotificationTicketEmailPrivacyAndOptOut(t *testing.T) {
	app := newTestApp(t)
	messages := notificationSMTP(t, app)
	app.cfg().TicketNotifyEmailEnabled = true
	user, err := app.store().CreateUser(store.User{Username: "ticket-owner", Role: store.RoleNormal, Active: true, Email: "owner@example.invalid", EmailVerified: true, NotifyOnTicketEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	existing := store.Ticket{ID: 1, UID: user.UID, Title: "private ticket", Status: store.TicketStatusOpen}
	updated := existing
	updated.AdminNote = "INTERNAL_SECRET_NEVER_SEND"
	updated.Replies = []store.TicketReply{{Role: store.RoleAdmin, Content: "Public reply"}}
	app.notifyTicketOwner(context.Background(), updated, existing)
	m := expectNotificationMail(t, messages)
	if strings.Contains(m.Body, updated.AdminNote) || !strings.Contains(m.Body, "Public reply") {
		t.Fatalf("owner email privacy: %q", m.Body)
	}
	if _, err := app.store().UpdateUser(user.UID, func(u *store.User) error { u.NotifyOnTicketEmail = false; return nil }); err != nil {
		t.Fatal(err)
	}
	app.notifyTicketOwner(context.Background(), updated, existing)
	noNotificationMail(t, messages)
	if _, err := app.store().UpdateUser(user.UID, func(u *store.User) error { u.NotifyOnTicketEmail = true; u.EmailVerified = false; return nil }); err != nil {
		t.Fatal(err)
	}
	app.notifyTicketOwner(context.Background(), updated, existing)
	noNotificationMail(t, messages)
}

func TestNotificationSchedulerEmailWithoutTelegram(t *testing.T) {
	app := newTestApp(t)
	messages := notificationSMTP(t, app)
	app.cfg().SchedulerNotifyEmailEnabled = true
	app.cfg().SchedulerFailureNotify = true
	for _, name := range []string{"admin-one", "unverified", "inactive", "normal"} {
		role := store.RoleAdmin
		if name == "normal" {
			role = store.RoleNormal
		}
		created, err := app.store().CreateUser(store.User{Username: name, Role: role, Active: name != "inactive", Email: name + "@example.invalid", EmailVerified: name != "unverified"})
		if err != nil {
			t.Fatal(err)
		}
		if name == "inactive" {
			if _, err := app.store().SetUserActiveAtomic(created.UID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, status := range []string{"failed", "failed", "success", "success"} {
		run, err := app.store().AddSchedulerRunReturning(store.SchedulerRun{JobID: "daily_stats", Type: "auto", Status: status, StartedAt: time.Now().Unix(), FinishedAt: time.Now().Unix()})
		if err != nil {
			t.Fatal(err)
		}
		app.notifySchedulerOutcome(app.store(), run)
		if i == 0 || i == 2 {
			if m := expectNotificationMail(t, messages); !strings.Contains(m.To, "admin-one@example.invalid") {
				t.Fatalf("wrong admin: %+v", m)
			}
		} else {
			noNotificationMail(t, messages)
		}
	}
}

func TestNotificationPreferencesStrictAndPersistent(t *testing.T) {
	app := newTestApp(t)
	cookies := registerAndLogin(t, app, "notify-user", "Password123456")
	for _, key := range []string{"notify_on_expiry_telegram", "notify_on_expiry_email", "notify_on_ticket_email"} {
		r := doJSON(app, http.MethodPut, "/api/v2/settings/preferences", fmt.Sprintf(`{%q:"false"}`, key), cookies)
		if r.Code != http.StatusBadRequest {
			t.Fatalf("nonboolean %s accepted: %d", key, r.Code)
		}
	}
	r := doJSON(app, http.MethodPut, "/api/v2/settings/preferences", `{"notify_on_expiry_telegram":false,"notify_on_expiry_email":true,"notify_on_ticket_email":true}`, cookies)
	if r.Code != http.StatusOK {
		t.Fatalf("save: %s", r.Body.String())
	}
	r = doJSON(app, http.MethodGet, "/api/v2/settings", "", cookies)
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data["notify_on_expiry_telegram"] != false || response.Data["notify_on_expiry_email"] != true || response.Data["notify_on_ticket_email"] != true {
		t.Fatalf("preferences lost: %#v", response.Data)
	}
	u, _ := app.store().FindUserByUsername("notify-user")
	encoded, _ := json.Marshal(u)
	var restored store.User
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.ExpiryTelegramNotificationsEnabled() {
		t.Fatal("persisted explicit opt-out lost")
	}
	if !(store.User{}).ExpiryTelegramNotificationsEnabled() {
		t.Fatal("legacy default changed")
	}
}

func TestNotificationLoginChannelSwitches(t *testing.T) {
	app := newTestApp(t)
	messages := notificationSMTP(t, app)
	app.cfg().TelegramMode = true
	app.cfg().TelegramBotToken = "123:ABC"
	var calls atomic.Int32
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer tg.Close()
	app.cfg().TelegramAPIURL = tg.URL
	u, err := app.store().CreateUser(store.User{Username: "login-notify", Role: store.RoleNormal, Active: true, Email: "login@example.invalid", EmailVerified: true, TelegramID: 123, NotifyOnLoginEmail: true, NotifyOnLoginTelegram: true})
	if err != nil {
		t.Fatal(err)
	}
	login := func() {
		t.Helper()
		if _, err := app.completeLogin(httptest.NewRequest(http.MethodPost, "/login", nil), loginInput{IP: "127.0.0.1", UserAgent: "test"}, u); err != nil {
			t.Fatal(err)
		}
	}
	app.cfg().LoginNotifyTelegramEnabled = false
	login()
	expectNotificationMail(t, messages)
	if calls.Load() != 0 {
		t.Fatal("disabled login TG channel sent")
	}
	app.cfg().LoginNotifyTelegramEnabled = true
	app.cfg().LoginNotifyEmailEnabled = false
	login()
	noNotificationMail(t, messages)
	if calls.Load() != 1 {
		t.Fatal("TG-only login notification missing")
	}
}

func TestNotificationTicketAdminEmailExcludesActorAndDisabled(t *testing.T) {
	app := newTestApp(t)
	messages := notificationSMTP(t, app)
	app.cfg().TicketNotifyEmailEnabled = true
	var actor store.User
	for _, name := range []string{"actor", "other", "optout"} {
		u, err := app.store().CreateUser(store.User{Username: name, Role: store.RoleAdmin, Active: true, Email: name + "@example.invalid", EmailVerified: true, NotifyOnTicketEmail: name != "optout"})
		if err != nil {
			t.Fatal(err)
		}
		if name == "actor" {
			actor = u
		}
	}
	app.notifyTicketAdmins(context.Background(), "updated", store.Ticket{ID: 1, UID: 99, Title: "Test"}, actor)
	if m := expectNotificationMail(t, messages); !strings.Contains(m.To, "other@example.invalid") {
		t.Fatalf("wrong admin recipient: %+v", m)
	}
	noNotificationMail(t, messages)
	app.cfg().TicketNotifyEmailEnabled = false
	app.notifyTicketAdmins(context.Background(), "updated", store.Ticket{ID: 1, UID: 99, Title: "Test"}, actor)
	noNotificationMail(t, messages)
}
