package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

func telegramLinkCreateTestHeaders() map[string]string {
	return map[string]string{
		"X-Twilight-Client": "webui",
		"X-Twilight-Intent": "create-telegram-link",
	}
}

// linkIDFor 把可读的测试名映射成合法的 32 位十六进制资源 ID。
func linkIDFor(name string) string {
	sum := sha256.Sum256([]byte("test-link:" + name))
	return hex.EncodeToString(sum[:16])
}

// seedTelegramLink 直接写一行链接用于构造过期 / 已确认 / 孤儿等状态。
// 约定：start token 与 browser secret 都等于链接 ID，测试可用 ID 直接确认或查询。
// State 留空时按 TelegramID 推导：注册场景 confirmed，已登录场景 consumed。
func (a *App) seedTelegramLink(t *testing.T, l store.TelegramLink) {
	t.Helper()
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if l.CreatedAt == 0 {
		l.CreatedAt = time.Now().Unix()
	}
	if l.State == "" {
		l.State = "pending"
		if l.TelegramID != 0 {
			l.State = "confirmed"
			if l.Scene == "user" {
				l.State, l.ResultUID = "consumed", l.UID
			}
		}
	}
	secretHash := ""
	if l.Scene == "register" {
		secretHash = store.TelegramLinkSecretHash(l.ID)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO twilight_telegram_links (id,start_hash,secret_hash,scene,uid,state,telegram_id,telegram_username,created_at,expires_at,result_uid,expected_rebind_since)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, l.ID, store.TelegramLinkStartHash(l.ID), secretHash, l.Scene, l.UID, l.State, l.TelegramID, l.TelegramUsername, l.CreatedAt, l.ExpiresAt, l.ResultUID, l.ExpectedRebindSince); err != nil {
		t.Fatal(err)
	}
}

// telegramLinkAlive 报告链接是否仍存在且未被取代。
func (a *App) telegramLinkAlive(id string) (store.TelegramLink, bool) {
	l, err := a.store().TelegramLink(context.Background(), id)
	return l, err == nil && l.State != "cancelled"
}

type issuedTelegramLink struct {
	ID       string
	Token    string
	Secret   string
	DeepLink string
	Manual   string
	Response *httptest.ResponseRecorder
}

func decodeIssuedTelegramLink(t *testing.T, rr *httptest.ResponseRecorder) issuedTelegramLink {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("issue link: %d %s", rr.Code, rr.Body.String())
	}
	var response struct {
		Data struct {
			ID       string `json:"link_id"`
			Token    string `json:"start_token"`
			Secret   string `json:"link_secret"`
			DeepLink string `json:"deep_link"`
			Manual   string `json:"manual_command"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	d := response.Data
	if len(d.ID) != 32 || len(d.Token) != 32 || d.ID == d.Token {
		t.Fatalf("issued link missing separated credentials: %s", rr.Body.String())
	}
	return issuedTelegramLink{ID: d.ID, Token: d.Token, Secret: d.Secret, DeepLink: d.DeepLink, Manual: d.Manual, Response: rr}
}

func issueRegisterTelegramLink(t *testing.T, app *App) issuedTelegramLink {
	t.Helper()
	app.cfg().TelegramMode = true
	if app.cfg().TelegramBotToken == "" {
		app.cfg().TelegramBotToken = "123:link-test"
	}
	link := decodeIssuedTelegramLink(t, doJSONWithHeaders(app, http.MethodPost, "/api/v2/registration/telegram/link", "", nil, telegramLinkCreateTestHeaders()))
	if len(link.Secret) != 64 {
		t.Fatalf("register link must carry a browser secret: %s", link.Response.Body.String())
	}
	return link
}

func issueUserTelegramLink(t *testing.T, app *App, cookies []*http.Cookie) issuedTelegramLink {
	t.Helper()
	app.cfg().TelegramMode = true
	if app.cfg().TelegramBotToken == "" {
		app.cfg().TelegramBotToken = "123:link-test"
	}
	link := decodeIssuedTelegramLink(t, doJSONWithHeaders(app, http.MethodPost, "/api/v2/me/telegram/link", "", cookies, telegramLinkCreateTestHeaders()))
	if link.Secret != "" {
		t.Fatalf("account link must not expose a browser secret: %s", link.Response.Body.String())
	}
	return link
}

func linkSecretHeaders(secret string) map[string]string {
	if secret == "" {
		return nil
	}
	return map[string]string{telegramLinkSecretHeader: secret}
}

func registerLinkStatus(app *App, id, secret string) *httptest.ResponseRecorder {
	return doJSONWithHeaders(app, http.MethodGet, "/api/v2/registration/telegram/link/"+id+"/status", "", nil, linkSecretHeaders(secret))
}

func userLinkStatus(app *App, id string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	return doJSON(app, http.MethodGet, "/api/v2/me/telegram/link/"+id+"/status", "", cookies)
}
