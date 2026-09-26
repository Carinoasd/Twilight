package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramBindHTTPPreservesBusinessFailures(t *testing.T) {
	for _, tc := range []struct {
		status        int
		code, message string
	}{
		{200, "", "绑定已确认"},
		{403, ErrTGBindGroupMembershipRequired, "绑定前需要先加入指定 Telegram 群组/频道: example"},
		{404, ErrTGBindCodeNotFound, "expired"},
		{409, ErrTGBindTargetTaken, "conflict"},
		{429, ErrRateLimited, "limited"},
		{502, ErrTGBindGroupCheckFailed, "upstream"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSONWithCode(w, tc.status, tc.status == 200, tc.code, tc.message, nil)
			}))
			defer server.Close()
			req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{}`))
			result, err := requestTelegramBindConfirmation(req)
			if err != nil {
				t.Fatal(err)
			}
			if result.Code != tc.status || result.Success != (tc.status == 200) || result.Message != tc.message {
				t.Fatalf("unexpected result: %#v", result)
			}
			message := telegramBindResultMessage(result)
			if message == "" || strings.Contains(message, "绑定请求异常") {
				t.Fatalf("lost business result: %q", message)
			}
			if tc.status == 403 && message != tc.message {
				t.Fatalf("lost required group: %q", message)
			}
		})
	}
}

func TestTelegramBindHTTPRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"malformed", "private-diagnostic", 500},
		{"trailing", `{"success":true} {}`, 200},
		{"oversized", strings.Repeat("x", telegramBindResponseLimit+1), 200},
		{"redirect", "", 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			req, _ := http.NewRequest(http.MethodPost, server.URL, nil)
			result, err := requestTelegramBindConfirmation(req)
			if err == nil || result.Success {
				t.Fatalf("accepted invalid response: %#v / %v", result, err)
			}
			if strings.Contains(err.Error(), "private-diagnostic") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("diagnostics leaked: %v", err)
			}
		})
	}
}

func TestTelegramBindHTTPStatusOverridesEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(telegramBindHTTPResult{Success: true, Code: 200})
	}))
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL, nil)
	result, err := requestTelegramBindConfirmation(req)
	if err != nil || result.Success || result.Code != http.StatusForbidden {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestTelegramBindHTTPDoesNotFollowRedirects(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var forwarded atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					forwarded.Store(true)
					_, _ = io.WriteString(w, `{"success":true,"code":200}`)
					return
				}
				http.Redirect(w, r, "/redirected", status)
			}))
			defer server.Close()
			body := []byte(`{"code":"ABCDEF12","telegram_id":42}`)
			req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(string(body)))
			signTelegramBindRequest(req, body, "test-key", time.Now())
			result, err := requestTelegramBindConfirmation(req)
			if err == nil || result.Success || forwarded.Load() {
				t.Fatalf("redirect followed: result=%#v err=%v forwarded=%v", result, err, forwarded.Load())
			}
		})
	}
}

func TestTelegramBindHTTPHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:1", nil)
	_, err := requestTelegramBindConfirmation(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestTelegramBindSignedRequest(t *testing.T) {
	now := time.Unix(1800000000, 0)
	body := []byte(`{"code":"ABCDEF12","telegram_id":42}`)
	const key = "test-only-signing-key"
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		key    string
		valid  bool
	}{
		{"valid", func(*http.Request) {}, key, true},
		{"missing-key", func(*http.Request) {}, "", false},
		{"wrong-key", func(*http.Request) {}, "other-key", false},
		{"missing-signature", func(r *http.Request) { r.Header.Del(telegramBindSignatureHeader) }, key, false},
		{"tampered-body", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(`{"code":"ABCDEF12","telegram_id":43}`))
		}, key, false},
		{"tampered-path", func(r *http.Request) { r.URL.Path = "/other" }, key, false},
		{"tampered-method", func(r *http.Request) { r.Method = http.MethodGet }, key, false},
		{"external", func(r *http.Request) { r.RemoteAddr = "203.0.113.1:123" }, key, false},
		{"proxy", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "203.0.113.1") }, key, false},
		{"expired", func(r *http.Request) { signTelegramBindRequest(r, body, key, now.Add(-61*time.Second)) }, key, false},
		{"future", func(r *http.Request) { signTelegramBindRequest(r, body, key, now.Add(61*time.Second)) }, key, false},
		{"oversized", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(strings.Repeat("x", maxJSONBodyBytes+1)))
		}, key, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/telegram/bind-confirm", strings.NewReader(string(body)))
			req.RemoteAddr = "127.0.0.1:123"
			signTelegramBindRequest(req, body, key, now)
			tc.change(req)
			if got := authenticateTelegramBindRequest(req, tc.key, now); got != tc.valid {
				t.Fatalf("authenticated=%v want=%v", got, tc.valid)
			}
			if tc.valid {
				read, _ := io.ReadAll(req.Body)
				if string(read) != string(body) {
					t.Fatal("authentication consumed request body")
				}
			}
		})
	}
}

func TestTelegramBindLogsRedactCapabilities(t *testing.T) {
	for _, key := range []string{"bind_code", "telegramBindCode", "X-Twilight-Bind-Signature"} {
		if !sensitiveLogKey(key) {
			t.Fatalf("sensitive key not redacted: %s", key)
		}
	}
	if sensitiveLogKey("error_code") {
		t.Fatal("non-secret error codes must remain diagnosable")
	}
}
