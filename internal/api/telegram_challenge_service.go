package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/prejudice-studio/twilight/internal/security"
	"github.com/prejudice-studio/twilight/internal/store"
)

const telegramBrowserCookie = "twilight_telegram_browser"

func telegramBrowserProof(r *http.Request) string {
	c, err := r.Cookie(telegramBrowserCookie)
	if err != nil || len(c.Value) != 64 {
		return ""
	}
	return c.Value
}

func (a *App) createBindCode(w http.ResponseWriter, r *http.Request, uid int64, scene string) {
	w.Header().Set("Cache-Control", "no-store, private")
	token, e1 := security.RandomHex(16)
	id, e2 := security.RandomHex(16)
	proof := telegramBrowserProof(r)
	var e3 error
	if scene == "register" && proof == "" {
		proof, e3 = security.RandomHex(32)
	}
	if e1 != nil || e2 != nil || e3 != nil {
		failWithCode(w, 500, ErrBindCodeSaveFailed, "绑定码生成失败")
		return
	}
	token, id = strings.ToUpper(token), strings.ToUpper(id)
	now := time.Now().Unix()
	c := store.TelegramChallenge{ID: id, TokenHash: store.TelegramChallengeHash(token), BindCode: store.BindCode{UID: uid, Scene: scene, CreatedAt: now, ExpiresAt: now + 300}}
	if scene == "register" {
		c.OwnerHash = store.TelegramBrowserHash(proof)
	}
	if err := a.store().CreateTelegramChallenge(r.Context(), c); err != nil {
		if errors.Is(err, store.ErrTelegramChallengeCapacity) {
			failWithCode(w, 429, ErrRateLimited, "绑定请求过多，请稍后重试")
			return
		}
		if errors.Is(err, store.ErrTelegramAlreadyBound) {
			failWithCode(w, 409, ErrTGAlreadyBound, "当前账号已绑定 Telegram")
			return
		}
		logTelegramChallengeFailure("issue", err)
		failWithCode(w, 503, ErrBindCodeSaveFailed, "绑定码保存失败，请稍后重试")
		return
	}
	if scene == "register" {
		http.SetCookie(w, &http.Cookie{Name: telegramBrowserCookie, Value: proof, Path: "/api", HttpOnly: true, Secure: a.cfg().CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	}
	ok(w, "OK", map[string]any{"bind_code": token, "challenge_id": id, "expires_in": 300})
}

// Authorize before subscribing or returning any identity details. The Bot token
// alone never permits registration from another browser.
func (a *App) authorizeTelegramChallenge(w http.ResponseWriter, r *http.Request, key string, uid int64) bool {
	c, err := a.store().TelegramChallenge(r.Context(), key)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		logTelegramChallengeFailure("authorize", err)
		failWithCode(w, 503, ErrBindCodeSaveFailed, "绑定服务暂不可用，请稍后重试")
		return false
	}
	if err == nil && c.OwnedBy(uid, store.TelegramBrowserHash(telegramBrowserProof(r))) {
		return true
	}
	writeTelegramBindCodeState(w, telegramBindCodeState{Status: "not_found", Invalid: true, Terminal: true, ErrorCode: ErrTGBindCodeNotFound, Message: "绑定码不存在"})
	return false
}

// confirmTelegramChallenge is shared by standalone Bot and the signed HTTP
// compatibility adapter. All network checks precede the final Store transaction.
func (a *App) confirmTelegramChallenge(ctx context.Context, code string, telegramID int64, username string) telegramBindHTTPResult {
	fail := func(status int, ec ErrCode, msg string) telegramBindHTTPResult {
		return telegramBindHTTPResult{Code: status, ErrorCode: ec, Message: msg}
	}
	if !a.telegramAvailable() {
		return fail(503, ErrTGNotConfigured, "Telegram Bot 未配置")
	}
	if !telegramBindCodePattern.MatchString(code) {
		return fail(400, ErrTGBindCodeFormat, "绑定码格式无效")
	}
	if telegramID <= 0 {
		return fail(400, ErrTGBindTGIDInvalid, "Telegram ID 无效")
	}
	if !a.allowRate(ctx, rateKey("tg-bind-confirm:", telegramID), a.cfg().RateLimitLoginPerMinute, time.Minute) {
		return fail(429, ErrUploadRateLimited, "操作过于频繁，请稍后再试")
	}
	c, err := a.store().TelegramChallenge(ctx, code)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (c.ExpiresAt <= time.Now().Unix() || c.State == "cancelled" || c.TokenHash != store.TelegramChallengeHash(code))) {
		return fail(404, ErrTGBindCodeNotFound, "绑定码不存在或已过期")
	}
	if err != nil {
		logTelegramChallengeFailure("confirm", err)
		return fail(503, ErrBindCodeSaveFailed, "绑定服务暂不可用，请稍后重试")
	}
	if c.Confirmed && c.TelegramID != telegramID {
		return fail(409, ErrTGBindTargetTaken, "绑定码已由其他 Telegram 确认")
	}
	if !c.Confirmed {
		var result telegramBindHTTPResult
		if missing, err := a.telegramBindRequirementMissing(ctx, telegramID); err != nil {
			logTelegramChallengeFailure("membership", err)
			result = fail(502, ErrTGBindGroupCheckFailed, "Telegram 加群/频道校验失败，请稍后重试")
		} else if len(missing) > 0 {
			result = fail(403, ErrTGBindGroupMembershipRequired, "绑定前需要先加入指定 Telegram 群组/频道: "+strings.Join(missing, ", "))
		}
		if result.Code != 0 {
			if err := a.store().SetTelegramChallengeFailure(ctx, c, string(result.ErrorCode), result.Message, result.Code, true); err != nil {
				logTelegramChallengeFailure("record_failure", err)
			}
			if a.bindStatus != nil {
				a.bindStatus.notify(c.ID)
			}
			return result
		}
	}
	confirmed, user, changed, err := a.store().ConfirmTelegramChallenge(ctx, code, telegramID, username)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExpired) {
		return fail(404, ErrTGBindCodeNotFound, "绑定码不存在或已过期")
	}
	if errors.Is(err, store.ErrTelegramAlreadyBound) {
		return fail(409, ErrTGAlreadyBound, "当前账号已绑定 Telegram，请先完成换绑审批和解绑")
	}
	if errors.Is(err, store.ErrConflict) {
		return fail(409, ErrTGBindTargetTaken, "该 Telegram 已被占用或绑定状态已变化")
	}
	if err != nil {
		logTelegramChallengeFailure("confirm", err)
		return fail(503, ErrBindCodeSaveFailed, "绑定服务暂不可用，请稍后重试")
	}
	if a.bindStatus != nil {
		a.bindStatus.notify(confirmed.ID)
		a.bindStatus.notify(code)
	}
	if changed {
		a.auditTelegramAction(telegramID, "bind_telegram_via_telegram", "user", user.UID, map[string]any{"scene": confirmed.Scene})
	}
	return telegramBindHTTPResult{Code: 200, Success: true, Message: "绑定已确认"}
}
