package api

import (
	"net"
	"net/http"
	"strings"
	"time"
)

// Loopback and proxy headers restrict the transport surface, but are not
// authentication: a misconfigured local proxy can omit forwarding headers.
// The binding endpoint additionally verifies a timestamped body signature.
func requestIsLoopbackInternal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	for _, header := range []string{"X-Forwarded-For", "X-Real-IP", "CF-Connecting-IP", "Forwarded"} {
		if strings.TrimSpace(r.Header.Get(header)) != "" {
			return false
		}
	}
	return true
}

func (a *App) handleBindConfirmSecure(w http.ResponseWriter, r *http.Request, _ Params) {
	if !authenticateTelegramBindRequest(r, a.telegramBindSigningKey(), time.Now()) {
		failWithCode(w, http.StatusForbidden, ErrInternalSecretInvalid, "绑定服务认证失败")
		return
	}
	payload := decodeMap(r)
	result := a.confirmTelegramChallenge(r.Context(), normalizeBindStatusCode(stringValue(payload, "code")), int64(intValue(payload, "telegram_id", 0)), stringValue(payload, "telegram_username"))
	if !result.Success {
		failWithCode(w, result.Code, result.ErrorCode, result.Message)
		return
	}
	ok(w, "绑定已确认", map[string]any{"confirmed": true})
}
