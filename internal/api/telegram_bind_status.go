package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/prejudice-studio/twilight/internal/store"
)

type telegramBindCodeState struct {
	Code             string
	Status           string
	ErrorCode        ErrCode
	HTTPStatus       int
	Message          string
	Bind             store.BindCode
	Confirmed        bool
	Invalid          bool
	Terminal         bool
	ExpiresIn        int64
	TelegramID       int64
	TelegramUsername string
	TelegramBound    bool
	Retryable        bool
}

// The resource ID is safe to use in a URL. Authorization is checked separately
// at the browser transport boundary; internal callers may use the Bot token.
func (a *App) telegramBindCodeStateContext(ctx context.Context, code string, uid int64, scene string, now int64) telegramBindCodeState {
	code = normalizeBindStatusCode(code)
	bad := func(status string, ec ErrCode, message string) telegramBindCodeState {
		return telegramBindCodeState{Status: status, ErrorCode: ec, HTTPStatus: 400, Message: message, Invalid: true, Terminal: true}
	}
	if !telegramBindCodePattern.MatchString(code) {
		return bad("invalid_format", ErrTGBindCodeFormat, "Telegram 绑定码格式不正确")
	}
	c, err := a.store().TelegramChallenge(ctx, code)
	if errors.Is(err, store.ErrNotFound) {
		return bad("not_found", ErrTGBindCodeNotFound, "绑定码不存在")
	}
	if err != nil {
		logTelegramChallengeFailure("status", err)
		return telegramBindCodeState{Status: "unavailable", HTTPStatus: 503, ErrorCode: ErrBindCodeSaveFailed, Message: "绑定服务暂不可用，请稍后重试"}
	}
	if uid != c.UID {
		return bad("not_found", ErrTGBindCodeNotFound, "绑定码不存在")
	}
	if scene != "" && scene != c.Scene {
		return bad("wrong_scene", ErrTGBindCodeSceneBad, "绑定码场景无效")
	}
	if c.ExpiresAt <= now {
		return bad("expired", ErrTGBindCodeExpired, "绑定码无效或已过期")
	}
	if c.State == "cancelled" {
		return bad("cancelled", ErrTGBindCodeExpired, "绑定码已被更新，请使用新绑定码")
	}
	state := telegramBindCodeState{Code: c.ID, Bind: c.BindCode, Status: "pending", ExpiresIn: c.ExpiresAt - now, Message: "等待 Telegram 确认"}
	if c.Scene == "register" && c.State == "consumed" {
		return bad("consumed", ErrTGBindCodeExpired, "绑定码已使用")
	}
	if c.Confirmed {
		if c.Scene == "register" {
			if _, taken := a.store().FindUserByTelegramID(c.TelegramID); taken {
				return bad("telegram_taken", ErrTGBindTargetTaken, "该 Telegram 已绑定其他账号")
			}
		} else {
			if c.CurrentTelegramID == nil || *c.CurrentTelegramID != c.TelegramID {
				return bad("telegram_taken", ErrTGBindTargetTaken, "绑定状态已变化")
			}
		}
		state.Status, state.Confirmed, state.Terminal = "confirmed", true, true
		state.TelegramBound = c.Scene == "user"
		state.TelegramID, state.TelegramUsername = c.TelegramID, c.TelegramUsername
		state.Message = "绑定码已确认"
	} else if c.ErrorCode != "" {
		state.ErrorCode, state.Message = ErrCode(c.ErrorCode), c.ErrorMessage
		state.Retryable = c.Retryable
		// Membership, rate and upstream failures remain retryable until TTL expires.
	}
	return state
}

func writeTelegramBindCodeState(w http.ResponseWriter, state telegramBindCodeState) {
	w.Header().Set("Cache-Control", "no-store")
	if state.HTTPStatus == http.StatusServiceUnavailable {
		failWithCode(w, state.HTTPStatus, state.ErrorCode, state.Message)
		return
	}
	if state.Invalid {
		writeJSONWithCode(w, http.StatusOK, false, state.ErrorCode, state.Message, state.response())
		return
	}
	ok(w, "OK", state.response())
}

func (a *App) cleanupExpiredBindCodes(now int64) int {
	n, err := a.store().CleanupTelegramChallenges(context.Background(), now, 0, 0)
	logTelegramChallengeFailure("cleanup_expired", err)
	return n
}

func (s telegramBindCodeState) response() map[string]any {
	data := map[string]any{
		"code":           s.Code,
		"status":         s.Status,
		"confirmed":      s.Confirmed,
		"invalid":        s.Invalid,
		"terminal":       s.Terminal,
		"message":        s.Message,
		"telegram_bound": s.TelegramBound,
		"retryable":      s.Retryable,
	}
	if s.ErrorCode != "" {
		data["error_code"] = s.ErrorCode
	}
	if s.ExpiresIn > 0 {
		data["expires_in"] = s.ExpiresIn
	}
	if s.TelegramID != 0 {
		data["telegram_id"] = s.TelegramID
	}
	if strings.TrimSpace(s.TelegramUsername) != "" {
		data["telegram_username"] = s.TelegramUsername
	}
	return data
}
