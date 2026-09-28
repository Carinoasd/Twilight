package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// SSE 日志流在管理员被禁用后必须停止推送，而不是只在连接开始时验权。
func TestRuntimeLogStreamStopsAfterAdminIsDisabled(t *testing.T) {
	app := newTestApp(t)
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	server := httptest.NewServer(app)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v2/admin/runtime/logs/stream", nil)
	for _, cookie := range admin {
		req.AddCookie(cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status=%d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	readEvent := func() string {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return "EOF"
			}
			if strings.HasPrefix(line, "event: ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
		}
	}
	if event := readEvent(); event != "snapshot" {
		t.Fatalf("first event=%q, want snapshot", event)
	}
	user, _ := app.store().FindUserByUsername("admin")
	if _, err := app.store().UpdateUser(user.UID, func(u *store.User) error { u.Active = false; return nil }); err != nil {
		t.Fatal(err)
	}
	runtimeLogs.append(RuntimeLogEntry{Level: "info", Message: "runtime log stream auth test trigger"})
	for i := 0; i < 5; i++ {
		switch event := readEvent(); event {
		case "revoked":
			return
		case "EOF":
			t.Fatal("stream ended without a revoked event (client timeout?)")
		case "logs", "ping":
			t.Fatalf("disabled admin still received %q event", event)
		}
	}
	t.Fatal("stream did not terminate after admin was disabled")
}
