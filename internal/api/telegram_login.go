package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prejudice-studio/twilight/internal/security"
	"github.com/prejudice-studio/twilight/internal/store"
	"go.uber.org/zap"
)

func (a *App) telegramLoginEnabled() bool {
	return a.cfg().TelegramLoginEnabled && a.telegramAvailable()
}

func (a *App) telegramLoginGuard(w http.ResponseWriter, r *http.Request, mutate bool) bool {
	w.Header().Set("Cache-Control", "no-store, private")
	if mutate && !requireWebUIIntent(w, r, twilightIntentCreateTelegramLink) {
		return false
	}
	if !a.telegramLoginEnabled() {
		failWithCode(w, http.StatusForbidden, ErrTelegramLoginUnavailable, "Telegram 扫码登录未启用")
		return false
	}
	return true
}

func (a *App) handleCreateTelegramLogin(w http.ResponseWriter, r *http.Request, _ Params) {
	if !a.telegramLoginGuard(w, r, true) {
		return
	}
	if !a.allowRate(r.Context(), rateKey("tg-login:create:", a.clientIP(r)), 5, time.Minute) {
		failWithCode(w, http.StatusTooManyRequests, ErrLoginRateLimited, "登录请求过于频繁")
		return
	}
	id, e1 := security.RandomHex(16)
	token, e2 := security.RandomHex(16)
	secret, e3 := security.RandomHex(32)
	code, e4 := security.RandomHex(3)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		a.telegramLoginError(w, errors.New("random source failed"))
		return
	}
	link, _ := a.telegramDeepLink(r.Context(), "login_"+token)
	if link == "" {
		failWithCode(w, http.StatusServiceUnavailable, ErrTelegramLoginUnavailable, "暂时无法取得 Bot 信息，请稍后重试")
		return
	}
	site := a.cfg().AppName
	// Origin is display context, never an authorization input. Do not render links
	// supplied by the browser as trusted bot buttons.
	if origin, err := url.Parse(r.Header.Get("Origin")); err == nil && (origin.Scheme == "https" || origin.Scheme == "http") && origin.Host != "" && origin.User == nil {
		site += " · 请求页面：" + truncateString(origin.Host, 120)
	}
	l := store.TelegramLogin{ID: id, StartHash: store.TelegramLoginHash("start", token), SecretHash: store.TelegramLoginHash("secret", secret), DeviceID: loginDeviceID(r.Header.Get("X-Twilight-Device"), r.UserAgent(), a.clientIP(r)), UserAgent: truncateString(r.UserAgent(), 240), Site: truncateString(site, 240), CheckCode: strings.ToUpper(code), ExpiresAt: time.Now().Add(store.TelegramLoginTTL).Unix()}
	if err := a.store().CreateTelegramLogin(r.Context(), l); err != nil {
		a.telegramLoginError(w, err)
		return
	}
	ok(w, "OK", map[string]any{"id": id, "secret": secret, "deep_link": link, "check_code": l.CheckCode, "expires_in": 180, "poll_interval": 3})
}

func telegramLoginSecret(r *http.Request) string {
	value := r.Header.Get(telegramLinkSecretHeader)
	if len(value) != 64 {
		return ""
	}
	return store.TelegramLoginHash("secret", value)
}

func (a *App) handleTelegramLoginStatus(w http.ResponseWriter, r *http.Request, p Params) {
	if !a.telegramLoginGuard(w, r, false) {
		return
	}
	if !telegramLinkIDPattern.MatchString(p["id"]) {
		a.telegramLoginError(w, store.ErrNotFound)
		return
	}
	if !a.allowRate(r.Context(), rateKey("tg-login:poll:", a.clientIP(r)), 120, time.Minute) {
		failWithCode(w, 429, ErrRateLimited, "请求过于频繁")
		return
	}
	l, err := a.store().TelegramLogin(r.Context(), p["id"], telegramLoginSecret(r))
	if err != nil {
		a.telegramLoginError(w, err)
		return
	}
	ok(w, "OK", map[string]any{"status": l.State})
}

func (a *App) handleCancelTelegramLogin(w http.ResponseWriter, r *http.Request, p Params) {
	if !a.telegramLoginGuard(w, r, true) {
		return
	}
	if err := a.store().CancelTelegramLogin(r.Context(), p["id"], telegramLoginSecret(r)); err != nil {
		a.telegramLoginError(w, err)
		return
	}
	ok(w, "已取消", nil)
}

func (a *App) handleConsumeTelegramLogin(w http.ResponseWriter, r *http.Request, p Params) {
	if !a.telegramLoginGuard(w, r, true) {
		return
	}
	if !a.allowRate(r.Context(), rateKey("login:", a.clientIP(r)), a.cfg().RateLimitLoginPerMinute, time.Minute) {
		failWithCode(w, 429, ErrLoginRateLimited, "登录过于频繁")
		return
	}
	input := loginInput{DeviceID: r.Header.Get("X-Twilight-Device"), UserAgent: r.UserAgent(), IP: a.clientIP(r)}
	user, err := a.store().ConsumeTelegramLogin(r.Context(), p["id"], telegramLoginSecret(r), loginDeviceID(input.DeviceID, input.UserAgent, input.IP))
	if err != nil {
		a.telegramLoginError(w, err)
		return
	}
	// The grant is already consumed. Session failures require a new QR; retries
	// must not produce two valid sessions for one Telegram approval.
	result, err := a.completeLogin(r, input, user)
	if err != nil {
		if f, ok := err.(*loginFailure); ok {
			failWithCode(w, f.Status, f.Code, f.Message)
		} else {
			a.telegramLoginError(w, err)
		}
		return
	}
	l, err := a.store().TelegramLogin(r.Context(), p["id"], telegramLoginSecret(r))
	if err != nil || l.State != "consumed" || !a.telegramLoginEnabled() {
		a.sessions().Delete(r.Context(), result.Token)
		a.telegramLoginError(w, store.ErrConflict)
		return
	}
	a.auditWithUser(r, user.UID, user.Username, "telegram_login", "user", user.UID, nil)
	a.issueSessionCookies(w, result.Token, result.Expiry)
	ok(w, "登录成功", map[string]any{"token": result.Token, "user": publicUser(result.User)})
}

func (a *App) telegramLoginError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTelegramLinkOwner) || errors.Is(err, store.ErrInvalid) {
		failWithCode(w, http.StatusConflict, ErrTelegramLoginInvalid, "登录请求无效或已失效，请重新生成二维码")
		return
	}
	if errors.Is(err, store.ErrTelegramLinkCapacity) {
		failWithCode(w, 429, ErrRateLimited, "登录请求过多，请稍后重试")
		return
	}
	// Never log request credentials or SQL argument dumps.
	zap.L().Warn("telegram login storage operation failed")
	failWithCode(w, http.StatusServiceUnavailable, ErrTelegramLoginFailed, "扫码登录暂不可用，请重新生成二维码")
}

func (a *App) telegramScanLogin(ctx context.Context, fromID int64, token string) {
	if !a.telegramLoginEnabled() || !telegramLinkIDPattern.MatchString(token) {
		_ = a.telegramSendMessage(ctx, fromID, "扫码登录未启用或请求无效。")
		return
	}
	if !a.allowRate(ctx, rateKey("tg-login:scan:", fromID), 10, time.Minute) {
		return
	}
	l, err := a.store().ScanTelegramLogin(ctx, token, fromID)
	if err != nil {
		_ = a.telegramSendMessage(ctx, fromID, "请求已失效或此 Telegram 未绑定可用账号，请重新生成二维码。")
		return
	}
	text := fmt.Sprintf("Twilight 登录确认\n站点：%s\n设备（浏览器提供）：%s\n核对码：%s\n\n请与原网页核对。仅确认自己发起的登录，不要扫描他人发来的登录二维码。", l.Site, l.UserAgent, l.CheckCode)
	markup := map[string]any{"inline_keyboard": [][]map[string]string{{{"text": "确认登录", "callback_data": "tgl:yes:" + l.ID}, {"text": "拒绝", "callback_data": "tgl:no:" + l.ID}}}}
	// Plain text avoids interpreting a browser-provided User-Agent as markup.
	_, _ = telegramPostResult[telegramMessageResult](a, ctx, "sendMessage", telegramSendMessageRequest{ChatID: fromID, Text: text, DisableWebPagePreview: true, ReplyMarkup: markup}, 20*time.Second)
}

func (a *App) telegramHandleLoginCallback(ctx context.Context, cb *telegramCallbackQuery) bool {
	if !strings.HasPrefix(cb.Data, "tgl:") {
		return false
	}
	message := "登录请求无效或已失效。"
	parts := strings.Split(cb.Data, ":")
	if len(parts) == 3 && (parts[1] == "yes" || parts[1] == "no") && telegramLinkIDPattern.MatchString(parts[2]) && a.telegramLoginEnabled() && !cb.From.IsBot && cb.From.ID > 0 && cb.Message != nil && cb.Message.Chat.ID == cb.From.ID && cb.Message.Chat.Type == "private" {
		if err := a.store().DecideTelegramLogin(ctx, parts[2], cb.From.ID, parts[1] == "yes"); err == nil {
			message = "已拒绝登录。"
			if parts[1] == "yes" {
				message = "已确认，请返回原网页。"
			}
		}
	}
	_ = a.telegramAnswerCallbackQuery(ctx, cb.ID, message, true)
	return true
}
