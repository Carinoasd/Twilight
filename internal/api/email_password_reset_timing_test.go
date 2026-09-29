package api

import (
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// 重设密码请求不能因为「信箱存在要同步寄信」而明显变慢，否则可用响应时间枚举信箱。
func TestEmailPasswordResetRequestDoesNotWaitForSMTP(t *testing.T) {
	app := newEmailTestApp(t, false)
	// 只接受连接、从不回应的 SMTP，同步寄信会卡到 SMTP 超时。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan net.Conn, 4)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			held <- conn
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	app.cfg().SMTPHost = "127.0.0.1"
	app.cfg().SMTPPort = addr.Port
	app.cfg().SMTPEncryption = "none"
	app.cfg().SMTPTimeoutSeconds = 3
	app.cfg().EmailResendCooldownSeconds = 0
	app.cfg().ForgotPasswordEnabled = true
	app.cfg().ForgotPasswordEmailEnabled = true

	var wg sync.WaitGroup
	previous := emailPasswordResetDispatch
	emailPasswordResetDispatch = func(task func()) {
		wg.Add(1)
		go func() { defer wg.Done(); task() }()
	}
	t.Cleanup(func() {
		emailPasswordResetDispatch = previous
		_ = listener.Close()
		wg.Wait()
		close(held)
		for conn := range held {
			_ = conn.Close()
		}
	})

	if _, err := app.store().CreateUser(store.User{Username: "reset-user", Active: true, Email: "reset@example.com", EmailVerified: true}); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"reset@example.com", "nobody@example.com"} {
		started := time.Now()
		rr := doJSON(app, http.MethodPost, "/api/v2/auth/password/email/request", `{"email":"`+email+`"}`, nil)
		elapsed := time.Since(started)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", email, rr.Code, rr.Body.String())
		}
		if elapsed > time.Second {
			t.Fatalf("%s response took %s; SMTP must not run in the request path", email, elapsed)
		}
	}
}
