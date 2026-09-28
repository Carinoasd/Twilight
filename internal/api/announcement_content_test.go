package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

// 更新公告时没带 force_read_seconds 不能被归零；content 可以显式清空。
func TestUpdateAnnouncementKeepsUnsentFieldsAndAllowsClearingContent(t *testing.T) {
	app := newTestApp(t)
	admin := registerAndLogin(t, app, "admin", "Admin123456")
	ann, err := app.store().UpsertAnnouncement(store.Announcement{Title: "t", Content: "body", Visible: true, ForceRead: true, ForceReadSeconds: 15})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/admin/announcements/" + strconv.FormatInt(ann.ID, 10)
	if rr := doJSON(app, http.MethodPut, path, `{"title":"renamed"}`, admin); rr.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rr.Code, rr.Body.String())
	}
	updated := findAnnouncementForTest(t, app, ann.ID)
	if updated.ForceReadSeconds != 15 || updated.Content != "body" || updated.Title != "renamed" {
		t.Fatalf("partial update clobbered fields: %#v", updated)
	}
	if rr := doJSON(app, http.MethodPut, path, `{"content":""}`, admin); rr.Code != http.StatusOK {
		t.Fatalf("clear content status=%d body=%s", rr.Code, rr.Body.String())
	}
	if cleared := findAnnouncementForTest(t, app, ann.ID); cleared.Content != "" || cleared.ForceReadSeconds != 15 {
		t.Fatalf("content should be cleared and seconds kept: %#v", cleared)
	}
}

// 公开公告列表不能暴露发布者 UID。
func TestPublicAnnouncementsHideCreatorUID(t *testing.T) {
	app := newTestApp(t)
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	if _, err := app.store().UpsertAnnouncement(store.Announcement{Title: "t", Content: "c", Visible: true, CreatedByUID: 1}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v2/announcements", "/api/v1/announcements"} {
		rr := doJSON(app, http.MethodGet, path, ``, nil)
		if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "created_by_uid") {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func findAnnouncementForTest(t *testing.T, app *App, id int64) store.Announcement {
	t.Helper()
	for _, ann := range app.store().ListAnnouncements(true) {
		if ann.ID == id {
			return ann
		}
	}
	t.Fatalf("announcement %d not found", id)
	return store.Announcement{}
}
