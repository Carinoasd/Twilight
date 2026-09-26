package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	telegramBindResponseLimit     = 64 << 10
	telegramBindHTTPTimeout       = 30 * time.Second
	telegramBindMembershipTimeout = 20 * time.Second
	telegramBindSignatureWindow   = 60 * time.Second
	telegramBindTimestampHeader   = "X-Twilight-Bind-Timestamp"
	telegramBindSignatureHeader   = "X-Twilight-Bind-Signature"
)

type telegramBindHTTPResult struct {
	Success   bool   `json:"success"`
	Code      int    `json:"code"`
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

// Only the internal binding protocol decodes failed HTTP envelopes. Other
// upstream clients retain their existing error semantics.
func requestTelegramBindConfirmation(req *http.Request) (telegramBindHTTPResult, error) {
	ctx, cancel := context.WithTimeout(req.Context(), telegramBindHTTPTimeout)
	defer cancel()
	// Reuse the shared connection pool, but never forward a signed request to
	// another path (including same-host redirects) before validating its result.
	client := *sharedHTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req.WithContext(ctx))
	if err != nil {
		// Do not carry URLs, request bodies, or remote diagnostics into logs.
		if ctx.Err() != nil {
			return telegramBindHTTPResult{}, ctx.Err()
		}
		return telegramBindHTTPResult{}, errors.New("binding confirmation transport failed")
	}
	defer response.Body.Close()
	result := telegramBindHTTPResult{Code: response.StatusCode}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return result, errors.New("binding confirmation redirect refused")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, telegramBindResponseLimit+1))
	if err != nil {
		return result, errors.New("binding confirmation response read failed")
	}
	if len(body) > telegramBindResponseLimit {
		return result, errors.New("binding confirmation response too large")
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return telegramBindHTTPResult{Code: response.StatusCode}, errors.New("invalid binding confirmation response")
	}
	// HTTP status is authoritative. A failed request cannot claim success in JSON.
	if response.StatusCode >= 400 {
		result.Code = response.StatusCode
		result.Success = false
	}
	return result, nil
}

// Use a domain-separated MAC; the raw Bot token never crosses the loopback API.
// An explicit internal secret overrides the shared Bot token for deployments
// that already configure it. Both processes must use the same effective config.
func (a *App) telegramBindSigningKey() string {
	return firstNonEmpty(strings.TrimSpace(a.cfg().BotInternalSecret), strings.TrimSpace(a.cfg().TelegramBotToken))
}

func telegramBindSignature(key, timestamp, method, path string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = io.WriteString(mac, "twilight:telegram-bind:v1\n"+timestamp+"\n"+method+"\n"+path+"\n")
	_, _ = mac.Write(body)
	return mac.Sum(nil)
}

func signTelegramBindRequest(req *http.Request, body []byte, key string, now time.Time) {
	timestamp := strconv.FormatInt(now.Unix(), 10)
	req.Header.Set(telegramBindTimestampHeader, timestamp)
	req.Header.Set(telegramBindSignatureHeader, hex.EncodeToString(telegramBindSignature(key, timestamp, req.Method, req.URL.RequestURI(), body)))
}

func authenticateTelegramBindRequest(req *http.Request, key string, now time.Time) bool {
	if key == "" || !requestIsLoopbackInternal(req) {
		return false
	}
	timestamp := req.Header.Get(telegramBindTimestampHeader)
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	window := int64(telegramBindSignatureWindow / time.Second)
	if err != nil || seconds < now.Unix()-window || seconds > now.Unix()+window {
		return false
	}
	provided, err := hex.DecodeString(req.Header.Get(telegramBindSignatureHeader))
	if err != nil || len(provided) != sha256.Size || req.Body == nil {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxJSONBodyBytes+1))
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) > maxJSONBodyBytes {
		return false
	}
	expected := telegramBindSignature(key, timestamp, req.Method, req.URL.RequestURI(), body)
	return hmac.Equal(provided, expected)
}

func telegramBindResultMessage(result telegramBindHTTPResult) string {
	if result.Success {
		return "Telegram 绑定已确认，可以回到网页继续。"
	}
	if result.ErrorCode == ErrInternalSecretInvalid {
		return "绑定服务认证失败，请联系管理员检查 API 与 Bot 的配置是否一致。"
	}
	if result.ErrorCode == ErrTGAlreadyBound {
		return "当前账号已绑定 Telegram，请先在网页完成换绑审批和解绑。"
	}
	switch result.Code {
	case http.StatusNotFound, http.StatusGone:
		return "绑定码无效或已过期，请在网页重新获取。"
	case http.StatusConflict:
		return "Telegram 已被占用或绑定状态已变化，请回到网页检查。"
	case http.StatusTooManyRequests:
		return "操作过于频繁，请稍后再试。"
	case http.StatusForbidden:
		if result.ErrorCode == ErrTGBindGroupMembershipRequired {
			return firstNonEmpty(result.Message, "绑定前需要先加入指定 Telegram 群组/频道。")
		}
		return "绑定请求被拒绝，请联系管理员检查绑定权限。"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "绑定服务暂时不可用，请稍后重试。"
	default:
		return "绑定失败，请回到网页检查绑定状态后重试。"
	}
}
