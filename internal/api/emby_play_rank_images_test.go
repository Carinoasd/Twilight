package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

func TestPlayRankPosterUsesSeriesArtwork(t *testing.T) {
	metadata := map[string]embyItemMetadata{
		"ep1":    {ID: "ep1", Type: "Episode", SeriesID: "series-1"},
		"series": {ID: "series", Type: "Series"},
		"broken": {ID: "broken", Type: "Episode", SeriesID: "../private"},
	}
	for _, tc := range []struct{ name, group, item, representative, want string }{
		{"series", "series", "", "ep1", "/api/v2/emby/items/series-1/image"},
		{"series item", "series", "", "series", "/api/v2/emby/items/series/image"},
		{"missing metadata", "series", "", "deleted", ""},
		{"invalid parent", "series", "", "broken", ""},
		{"movie without metadata", "movie", "movie-1", "", "/api/v2/emby/items/movie-1/image"},
		{"legacy item", "item", "ep1", "", "/api/v2/emby/items/ep1/image"},
		{"invalid item", "movie", "../secret", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := playRankPosterURL(store.PlaybackMediaRank{ItemID: tc.item, RepresentativeItemID: tc.representative}, tc.group, metadata)
			if got != tc.want {
				t.Fatalf("poster=%q want %q", got, tc.want)
			}
		})
	}
}

func TestPlayRankAvatarRejectsExternalAndIdentifyingURLs(t *testing.T) {
	const file = "0123456789abcdef.png"
	if got := playRankAvatarURL("/api/v1/users/assets/avatar/" + file); got != "/api/v2/emby/play-rank/avatars/"+file {
		t.Fatalf("unexpected safe avatar: %q", got)
	}
	for _, value := range []string{"", "https://example.com/alice.png", "//example.com/a.png", "/api/v2/users/1/avatar", "/api/v1/users/assets/avatar/../background/" + file, "/api/v1/users/assets/avatar/" + file + "?uid=1", "/api/v1/users/assets/avatar/0123456789abcdef.svg"} {
		if got := playRankAvatarURL(value); got != "" {
			t.Fatalf("accepted unsafe avatar %q", value)
		}
	}
}

func TestPlayRankImagesProjectionAndAvatarAccess(t *testing.T) {
	app := newTestApp(t)
	app.cfg().PlayRankEnabled = true
	app.cfg().PlayRankUserVisible = true
	adminCookies := registerAndLogin(t, app, "admin", "Admin123456")
	viewerCookies := registerAndLogin(t, app, "viewer", "Viewer123456")
	// Resolve the stored viewer by username rather than assuming its generated UID.
	var uid int64
	for _, u := range app.store().ListUsers() {
		if u.Username == "viewer" {
			uid = u.UID
		}
	}
	if uid == 0 {
		t.Fatal("viewer was not registered")
	}
	const filename = "0123456789abcdef.png"
	const personalURL = "/api/v1/users/assets/avatar/" + filename
	const rankURL = "/api/v2/emby/play-rank/avatars/" + filename
	if _, err := app.store().UpdateUser(uid, func(u *store.User) error { u.Avatar = personalURL; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(app.cfg().UploadDir, "avatar"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.cfg().UploadDir, "avatar", filename), []byte("test-image"), 0600); err != nil {
		t.Fatal(err)
	}
	if resp := doJSON(app, http.MethodGet, rankURL, "", adminCookies); resp.Code != http.StatusNotFound {
		t.Fatalf("unranked avatar status=%d", resp.Code)
	}
	if err := app.store().AddPlaybackRecord(store.PlaybackRecord{UID: uid, ItemID: "movie-1", MediaType: "Movie", Title: "Film", PlayedAt: 100, Duration: 60}); err != nil {
		t.Fatal(err)
	}
	if err := app.store().AddPlaybackRecord(store.PlaybackRecord{UID: uid, ItemID: "ep1", SeriesName: "Series", MediaType: "Episode", Title: "Pilot", PlayedAt: 100, Duration: 60}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/Items") || r.URL.Query().Get("Ids") != "ep1" {
			t.Errorf("unexpected metadata request %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Items":[{"Id":"ep1","Type":"Episode","SeriesId":"series-1"}]}`))
	}))
	defer server.Close()
	app.cfg().EmbyURL = server.URL
	app.cfg().EmbyToken = "test-only"
	for _, group := range []string{"series", "movie"} {
		resp := doJSON(app, http.MethodGet, "/api/v2/emby/play-rank?range=all&group_by="+group, "", viewerCookies)
		if resp.Code != http.StatusOK {
			t.Fatalf("ranking status=%d body=%s", resp.Code, resp.Body.String())
		}
		body := resp.Body.String()
		for _, forbidden := range []string{`"uid"`, `"username"`, personalURL, server.URL, "test-only"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("ranking disclosed %q", forbidden)
			}
		}
		var result struct {
			Data struct {
				Media []struct {
					Poster string `json:"poster_url"`
				}
				Users []struct {
					Avatar string `json:"avatar_url"`
				}
			}
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		wantPoster := "/api/v2/emby/items/" + group + "-1/image"
		if len(result.Data.Media) != 1 || result.Data.Media[0].Poster != wantPoster || len(result.Data.Users) != 1 || result.Data.Users[0].Avatar != rankURL {
			t.Fatalf("unexpected projection: %s", body)
		}
	}
	otherCookies := registerAndLogin(t, app, "other", "Other123456")
	if resp := doJSON(app, http.MethodGet, personalURL, "", otherCookies); resp.Code != http.StatusNotFound {
		t.Fatalf("personal asset restriction changed: %d", resp.Code)
	}
	if resp := doJSON(app, http.MethodGet, rankURL, "", otherCookies); resp.Code != http.StatusOK || resp.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("rank avatar status=%d headers=%v", resp.Code, resp.Header())
	}
	if resp := doJSON(app, http.MethodGet, rankURL, "", nil); resp.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous avatar status=%d", resp.Code)
	}
	for _, flag := range []*bool{&app.cfg().PlayRankEnabled, &app.cfg().PlayRankUserVisible} {
		*flag = false
		if resp := doJSON(app, http.MethodGet, rankURL, "", otherCookies); resp.Code != http.StatusForbidden {
			t.Fatalf("disabled avatar status=%d", resp.Code)
		}
		if resp := doJSON(app, http.MethodGet, rankURL, "", adminCookies); resp.Code != http.StatusOK {
			t.Fatalf("admin avatar status=%d", resp.Code)
		}
		*flag = true
	}
	if _, err := app.store().UpdateUser(uid, func(u *store.User) error { u.Avatar = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if resp := doJSON(app, http.MethodGet, rankURL, "", otherCookies); resp.Code != http.StatusNotFound {
		t.Fatalf("removed avatar status=%d", resp.Code)
	}
	app.cfg().EmbyURL = ""
	app.invalidatePlayRankCache()
	data := app.buildPlayRank(context.Background(), playRankRequest{rangeKey: "all", limit: 20, groupBy: "series", sortBy: "plays"}, false)
	media := data["media"].([]map[string]any)
	if len(media) != 1 || media[0]["poster_url"] != nil {
		t.Fatalf("offline Emby must preserve ranking without a false poster: %v", media)
	}
}
