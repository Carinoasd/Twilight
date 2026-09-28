package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// TestSchedulerCleanupNoEmbyKeepsRecentlyUnboundUser 回归审查 M7：注册很久、刚解绑 Emby
// 的老用户不能被 cleanup_no_emby 当成「注册后长期未开通」直接删掉。
func TestSchedulerCleanupNoEmbyKeepsRecentlyUnboundUser(t *testing.T) {
	app := newTestApp(t)
	app.cfg().AutoCleanupNoEmby = true
	app.cfg().AutoCleanupNoEmbyDays = 7
	old := time.Now().AddDate(-1, 0, 0).Unix()
	veteran, err := app.store().CreateUser(store.User{Username: "veteran", Role: store.RoleNormal, Active: true, EmbyID: "emby-veteran", RegisterTime: old, CreatedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().UpdateUser(veteran.UID, func(u *store.User) error { u.EmbyID = ""; u.EmbyUsername = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	stale, err := app.store().CreateUser(store.User{Username: "never-emby", Role: store.RoleNormal, Active: true, RegisterTime: old, CreatedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	summary, _, err := app.runSchedulerJob(httptest.NewRequest(http.MethodPost, "/scheduler", nil), "cleanup_no_emby")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := app.store().User(veteran.UID); !ok {
		t.Fatalf("recently unbound veteran was deleted: %#v", summary)
	}
	if _, ok := app.store().User(stale.UID); ok {
		t.Fatalf("long-time no-Emby user should still be cleaned: %#v", summary)
	}
}
