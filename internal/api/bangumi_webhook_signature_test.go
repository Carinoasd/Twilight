package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

func signedBangumiWebhook(app *App, secret string, ts int64, body string, signature string) int {
	if signature == "" {
		signature = bangumiWebhookSignature(secret, ts, []byte(body))
	}
	headers := map[string]string{bangumiWebhookSignatureHeader: signature}
	if ts != 0 {
		headers[bangumiWebhookTimestampHeader] = strconv.FormatInt(ts, 10)
	}
	return doJSONWithHeaders(app, http.MethodPost, "/api/v2/emby/bangumi/webhook", body, nil, headers).Code
}

// 签名覆盖 body+timestamp：改 body、缺时间戳、过期、重放都要拒绝。
func TestBangumiWebhookSignedModeRejectsTamperingAndReplay(t *testing.T) {
	app := newTestApp(t)
	app.cfg().BangumiEnabled = true
	app.cfg().BangumiWebhookSecret = "webhook-secret"
	created, err := app.store().CreateUser(store.User{Username: "viewer", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().UpdateUser(created.UID, func(u *store.User) error { u.EmbyID = "emby-signed"; return nil }); err != nil {
		t.Fatal(err)
	}
	body := `{"Event":"PlaybackStopped","UserId":"emby-signed","Item":{"Id":"item-signed","Name":"Signed","Type":"Episode","RunTimeTicks":600000000}}`
	now := time.Now().Unix()

	if code := signedBangumiWebhook(app, "webhook-secret", now, body, ""); code != http.StatusOK {
		t.Fatalf("valid signed webhook status=%d", code)
	}
	if code := signedBangumiWebhook(app, "webhook-secret", now, body, ""); code != http.StatusConflict {
		t.Fatalf("byte-for-byte replay status=%d, want 409", code)
	}
	tampered := `{"Event":"PlaybackStopped","UserId":"someone-else","Item":{"Id":"x"}}`
	if code := signedBangumiWebhook(app, "webhook-secret", now+1, tampered, bangumiWebhookSignature("webhook-secret", now+1, []byte(body))); code != http.StatusForbidden {
		t.Fatalf("tampered body status=%d, want 403", code)
	}
	if code := signedBangumiWebhook(app, "wrong-secret", now+2, body, ""); code != http.StatusForbidden {
		t.Fatalf("wrong secret status=%d, want 403", code)
	}
	if code := signedBangumiWebhook(app, "webhook-secret", 0, body, bangumiWebhookSignature("webhook-secret", 0, []byte(body))); code != http.StatusUnauthorized {
		t.Fatalf("missing timestamp status=%d, want 401", code)
	}
	if code := signedBangumiWebhook(app, "webhook-secret", now-3600, body, ""); code != http.StatusGone {
		t.Fatalf("stale timestamp status=%d, want 410", code)
	}
}

// 兼容期关闭后，旧的共享 token（头或 ?token=）不再被接受。
func TestBangumiWebhookLegacyTokenCanBeDisabled(t *testing.T) {
	app := newTestApp(t)
	app.cfg().BangumiEnabled = true
	app.cfg().BangumiWebhookSecret = "webhook-secret"
	app.cfg().BangumiWebhookAllowLegacyToken = false
	if rr := doJSON(app, http.MethodPost, "/api/v1/emby/bangumi/webhook?token=webhook-secret", `{"Event":"PlaybackStopped"}`, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("query token with legacy disabled status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr := doJSONWithHeaders(app, http.MethodPost, "/api/v1/emby/bangumi/webhook", `{"Event":"PlaybackStopped"}`, nil, map[string]string{"X-Twilight-Bangumi-Token": "webhook-secret"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("header token with legacy disabled status=%d body=%s", rr.Code, rr.Body.String())
	}
}
