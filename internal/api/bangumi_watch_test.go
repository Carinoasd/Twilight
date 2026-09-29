package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

type bangumiWatchMock struct {
	writes    atomic.Int32
	searches  atomic.Int32
	auth      atomic.Int32
	fail      atomic.Bool
	ambiguous atomic.Bool
	account   atomic.Int64
	episode   atomic.Int64
}

func TestBangumiWatchWebhookCompletionEvidence(t *testing.T) {
	app := newTestApp(t)
	app.cfg().BangumiEnabled = true
	app.cfg().BangumiWebhookSecret = "webhook-test-secret"
	app.cfg().BangumiMinProgressPercent = 90
	u, err := app.store().CreateUser(store.User{Username: "webhook-watch", EmbyID: "media-user", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		position       int
		played         any
		flat, complete bool
	}{
		{89, false, false, false}, {90, false, false, true}, {0, false, false, false},
		{0, true, true, true}, {0, "true", true, false}, {90, false, true, true},
	} {
		id := fmt.Sprintf("evidence-%d", i)
		item := map[string]any{"Id": id, "Name": "Episode", "SeriesName": "Series", "Type": "Episode", "IndexNumber": 3, "ParentIndexNumber": 2, "RunTimeTicks": 1000000000}
		payload := map[string]any{"UserId": "media-user", "Item": item, "PlaybackPositionTicks": tc.position * 10000000, "PlayedToCompletion": tc.played}
		if tc.flat {
			delete(payload, "Item")
			payload["ItemId"] = id
			payload["Name"] = "Episode"
			payload["SeriesName"] = "Series"
			payload["ItemType"] = "Episode"
			payload["EpisodeNumber"] = 3
			payload["SeasonNumber"] = 2
			payload["RunTimeTicks"] = 1000000000
		}
		body, _ := json.Marshal(payload)
		ts := time.Now().Unix()
		r := httptest.NewRequest(http.MethodPost, "/api/v2/bangumi/webhook", strings.NewReader(string(body)))
		r.Header.Set(bangumiWebhookTimestampHeader, strconv.FormatInt(ts, 10))
		r.Header.Set(bangumiWebhookSignatureHeader, bangumiWebhookSignature(app.cfg().BangumiWebhookSecret, ts, body))
		w := httptest.NewRecorder()
		app.handleBangumiWebhook(w, r, nil)
		if w.Code != 200 {
			t.Fatalf("webhook %d: %s", w.Code, w.Body.String())
		}
		key := store.BangumiRecordKey(store.PlaybackRecord{ItemID: id, SeriesName: "Series", MediaType: "Episode", IndexNumber: 3})
		latest, _ := app.store().User(u.UID)
		if got := latest.BangumiWatch[key]; got.Completed != tc.complete || got.Season != 2 {
			t.Fatalf("evidence case %d: %+v", i, got)
		}
		if tc.position == 0 {
			for _, r := range app.store().PlaybackRecords(u.UID, 0, 50) {
				if r.ItemID == id && r.Duration != 0 {
					t.Fatal("runtime fabricated a playback position")
				}
			}
		}
	}
}

func TestBangumiWatchCollectionPolicyAndMetadata(t *testing.T) {
	app := newTestApp(t)
	var writes []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		writes = append(writes, body)
		w.WriteHeader(204)
	}))
	defer server.Close()
	app.cfg().BangumiAPIURL = server.URL
	app.cfg().BangumiAutoAddCollection = false
	if err := app.ensureBangumiCollection(context.Background(), "42", "12", "fake-token", false); err != errBangumiCollectionRequired || len(writes) != 0 {
		t.Fatal("auto-add switch ignored")
	}
	app.cfg().BangumiAutoAddCollection = true
	if err := app.ensureBangumiCollection(context.Background(), "42", "12", "fake-token", false); err != nil {
		t.Fatal(err)
	}
	if writes[0]["private"] != true || writes[0]["type"] != float64(3) {
		t.Fatal("new collection privacy/type not applied")
	}
	app.cfg().BangumiManageEnabled = true
	cookies := registerAndLogin(t, app, "metadata-user", "Password123456")
	u, _ := app.store().FindUserByUsername("metadata-user")
	_, err := app.store().UpdateUser(u.UID, func(v *store.User) error { v.BGMToken = "fake-token"; v.BGMManageMode = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	res := doJSON(app, http.MethodPatch, "/api/v2/bangumi/collections/42", `{"type":1,"rate":8,"comment":"我的短评","tags":["动画","收藏"],"private":false}`, cookies)
	if res.Code != 200 {
		t.Fatalf("metadata update: %s", res.Body.String())
	}
	last := writes[len(writes)-1]
	if last["comment"] != "我的短评" || last["private"] != false || len(last["tags"].([]any)) != 2 || last["rate"] != float64(8) {
		t.Fatalf("metadata lost: %+v", last)
	}
	app.cfg().BangumiBlockKeywords = []string{"  TrAiLeR ", ""}
	if !app.bangumiBlockedRecord(store.PlaybackRecord{Title: "Series Trailer"}) || app.bangumiBlockedRecord(store.PlaybackRecord{Title: "Series"}) {
		t.Fatal("keyword filtering failed")
	}
}

func TestBangumiWatchScheduledEligibilityAndGate(t *testing.T) {
	app := newTestApp(t)
	m := mockBangumiWatch(t, app)
	_, _ = seedBangumiWatch(t, app)
	for i := 0; i < 3; i++ {
		u := store.User{Username: fmt.Sprintf("ineligible-%d", i), Active: true, BGMMode: true, BGMToken: "fake-token"}
		switch i {
		case 0:
			u.Active = false
		case 1:
			u.BGMMode = false
		case 2:
			u.BGMToken = ""
		}
		created, err := app.store().CreateUser(u)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := app.store().UpdateUser(created.UID, func(v *store.User) error { v.Active = false; return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	result, _, err := app.runBangumiWatchSync(context.Background())
	if err != nil || result["users"] != 1 || result["synced"] != 1 || m.writes.Load() != 1 {
		t.Fatalf("scheduler eligibility: %v %v", result, err)
	}
	app.cfg().BangumiEnabled = false
	result, _, err = app.runBangumiWatchSync(context.Background())
	if err != nil || result["enabled"] != false || m.writes.Load() != 1 {
		t.Fatal("disabled scheduler wrote remotely")
	}
	for _, tc := range []struct{ method, path string }{{http.MethodGet, "/api/v2/bangumi/records"}, {http.MethodPut, "/api/v2/bangumi/records/unknown"}} {
		res := doJSON(app, tc.method, tc.path, `{"action":"retry"}`, nil)
		if res.Code != 401 {
			t.Fatalf("unauthenticated route: %d", res.Code)
		}
	}
}

func mockBangumiWatch(t *testing.T, app *App) *bangumiWatchMock {
	t.Helper()
	m := &bangumiWatchMock{}
	m.account.Store(12)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "" {
			m.auth.Add(1)
		}
		switch {
		case r.URL.Path == "/v0/me":
			fmt.Fprintf(w, `{"id":%d,"username":"watch-user"}`, m.account.Load())
		case r.URL.Path == "/v0/search/subjects":
			m.searches.Add(1)
			if m.ambiguous.Load() {
				fmt.Fprint(w, `{"data":[{"id":42,"name":"Series"},{"id":43,"name":"Series"}]}`)
			} else {
				fmt.Fprint(w, `{"data":[{"id":42,"name":"Series","type":2}]}`)
			}
		case r.URL.Path == "/v0/subjects/42":
			fmt.Fprint(w, `{"id":42,"name":"Series","type":2}`)
		case strings.HasPrefix(r.URL.Path, "/v0/users/") && r.Method == http.MethodGet && !strings.HasSuffix(r.URL.Path, "/episodes"):
			if r.URL.Path != "/v0/users/watch-user/collections/42" {
				t.Errorf("collection read must use the verified username: %s", r.URL.Path)
				w.WriteHeader(404)
				return
			}
			fmt.Fprint(w, `{"type":2,"rate":9,"private":true,"comment":"keep me"}`)
		case r.URL.Path == "/v0/episodes":
			fmt.Fprint(w, `{"total":3,"data":[{"id":101,"ep":1},{"id":102,"ep":2},{"id":103,"ep":3}]}`)
		case strings.HasSuffix(r.URL.Path, "/episodes") && r.Method == http.MethodPatch:
			var payload struct {
				IDs  []int `json:"episode_id"`
				Type int   `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			m.writes.Add(1)
			if len(payload.IDs) != 1 || payload.Type != 2 {
				m.episode.Store(-1)
			} else {
				m.episode.Store(int64(payload.IDs[0]))
			}
			if m.fail.Load() {
				w.WriteHeader(503)
				fmt.Fprint(w, `{"error":"unavailable"}`)
			} else {
				w.WriteHeader(204)
			}
		default:
			t.Errorf("unexpected Bangumi request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	app.cfg().BangumiEnabled = true
	app.cfg().BangumiManageEnabled = true
	app.cfg().BangumiAPIURL = server.URL + "/v0"
	return m
}
func seedBangumiWatch(t *testing.T, app *App) (store.User, store.PlaybackRecord) {
	t.Helper()
	u, err := app.store().CreateUser(store.User{Username: "watch-user", Active: true, BGMMode: true, BGMToken: "test-personal-token", BGMManageMode: true})
	if err != nil {
		t.Fatal(err)
	}
	r := store.PlaybackRecord{UID: u.UID, ItemID: "episode-3", Title: "Third", SeriesName: "Series", MediaType: "Episode", IndexNumber: 3, Duration: 1200, PlayedAt: time.Now().Unix()}
	if err := app.store().AddPlaybackRecord(r); err != nil {
		t.Fatal(err)
	}
	if err := app.store().UpdateBangumiWatch(u.UID, store.BangumiRecordKey(r), "", func(v *store.BangumiWatchRecord) { v.Completed = true }); err != nil {
		t.Fatal(err)
	}
	return u, r
}
func TestBangumiWatchExactEpisodeCheckpointAndTokenRotation(t *testing.T) {
	app := newTestApp(t)
	m := mockBangumiWatch(t, app)
	u, r := seedBangumiWatch(t, app)
	done, _, failed, logs := app.syncBangumiForUser(context.Background(), u.UID)
	if done != 1 || failed != 0 || m.episode.Load() != 103 || m.auth.Load() != 0 {
		t.Fatalf("sync=%d failed=%d episode=%d logs=%v", done, failed, m.episode.Load(), logs)
	}
	if err := app.store().ClearBangumiSyncLogs(u.UID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store().UpdateUser(u.UID, func(v *store.User) error { v.BGMToken = "rotated-test-token"; return nil }); err != nil {
		t.Fatal(err)
	}
	done, _, failed, _ = app.syncBangumiForUser(context.Background(), u.UID)
	if done != 0 || failed != 0 || m.writes.Load() != 1 {
		t.Fatal("log clearing/token rotation re-synced completed record")
	}
	m.account.Store(13)
	done, _, failed, _ = app.syncBangumiForUser(context.Background(), u.UID)
	if done != 1 || failed != 0 || m.writes.Load() != 2 {
		t.Fatal("different account did not get independent sync")
	}
	latest, _ := app.store().User(u.UID)
	key := store.BangumiRecordKey(r)
	if latest.BangumiWatch["12:"+key].Status != "success" || latest.BangumiWatch["13:"+key].Status != "success" {
		t.Fatal("historical account checkpoint lost")
	}
}
func TestBangumiWatchReviewFailureRetryAndCancel(t *testing.T) {
	app := newTestApp(t)
	m := mockBangumiWatch(t, app)
	u, r := seedBangumiWatch(t, app)
	m.ambiguous.Store(true)
	done, skip, fail, _ := app.syncBangumiForUser(context.Background(), u.UID)
	if done != 0 || skip != 1 || fail != 0 || m.writes.Load() != 0 {
		t.Fatal("ambiguous search mutated collection")
	}
	latest, _ := app.store().User(u.UID)
	key := "12:" + store.BangumiRecordKey(r)
	if latest.BangumiWatch[key].Status != "needs_review" {
		t.Fatal("review state missing")
	}
	if err := app.store().UpdateBangumiWatch(u.UID, key, u.BGMToken, func(v *store.BangumiWatchRecord) {
		v.Status = "pending"
		v.Manual = true
		v.SubjectID = "42"
		v.Episode = 3
		v.Completed = true
	}); err != nil {
		t.Fatal(err)
	}
	m.fail.Store(true)
	_, _, fail, _ = app.syncBangumiForUser(context.Background(), u.UID)
	if fail != 1 {
		t.Fatal("upstream failure reported as success")
	}
	m.fail.Store(false)
	done, _, fail, _ = app.syncBangumiForUser(context.Background(), u.UID)
	if done != 1 || fail != 0 {
		t.Fatal("retry failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, fail, _ = app.syncBangumiForUser(ctx, u.UID)
	if fail == 0 {
		t.Fatal("cancellation reported as success")
	}
}
func TestBangumiWatchIncompleteAndOlderThan100(t *testing.T) {
	app := newTestApp(t)
	m := mockBangumiWatch(t, app)
	u, r := seedBangumiWatch(t, app)
	// More recent successful checkpoints must not starve older pending records.
	for n := 0; n < 105; n++ {
		rec := r
		rec.ItemID = fmt.Sprintf("new-%d", n)
		rec.PlayedAt += int64(n + 1)
		if err := app.store().AddPlaybackRecord(rec); err != nil {
			t.Fatal(err)
		}
		if err := app.store().UpdateBangumiWatch(u.UID, "12:"+store.BangumiRecordKey(rec), u.BGMToken, func(v *store.BangumiWatchRecord) { v.Status = "success" }); err != nil {
			t.Fatal(err)
		}
	}
	done, _, fail, _ := app.syncBangumiForUser(context.Background(), u.UID)
	if done != 1 || fail != 0 || m.writes.Load() != 1 {
		t.Fatal("older pending record starved")
	}
	r.ItemID = "partial"
	if err := app.store().AddPlaybackRecord(r); err != nil {
		t.Fatal(err)
	}
	done, _, fail, _ = app.syncBangumiForUser(context.Background(), u.UID)
	if done != 0 || fail != 0 || m.writes.Load() != 1 {
		t.Fatal("unconfirmed completion sent")
	}
}
func TestBangumiWatchOwnershipAndStrictCollectionFields(t *testing.T) {
	app := newTestApp(t)
	mockBangumiWatch(t, app)
	u, r := seedBangumiWatch(t, app)
	cookies := registerAndLogin(t, app, "watch-other", "Password123456")
	other, _ := app.store().FindUserByUsername("watch-other")
	if _, err := app.store().UpdateUser(other.UID, func(v *store.User) error { v.BGMToken = u.BGMToken; return nil }); err != nil {
		t.Fatal(err)
	}
	response := doJSON(app, http.MethodPut, "/api/v2/bangumi/records/"+store.BangumiRecordKey(r), `{"action":"ignore"}`, cookies)
	if response.Code != 404 {
		t.Fatal("other user modified record")
	}
	for _, body := range []string{`{"type":"3"}`, `{"type":3.5}`, `{"type":3,"private":"false"}`, `{"type":3,"tags":["invalid tag"]}`, `{"type":3,"rate":8.5}`} {
		// Enable only the test user's personal management credentials.
		other, _ := app.store().FindUserByUsername("watch-other")
		_, err := app.store().UpdateUser(other.UID, func(v *store.User) error { v.BGMManageMode = true; v.BGMToken = u.BGMToken; return nil })
		if err != nil {
			t.Fatal(err)
		}
		response = doJSON(app, http.MethodPatch, "/api/v2/bangumi/collections/42", body, cookies)
		if response.Code != 400 {
			t.Fatalf("invalid fields accepted %s: %d %s", body, response.Code, response.Body.String())
		}
	}
}
func TestBangumiWatchCrossStoreLockAndRelease(t *testing.T) {
	app := newTestApp(t)
	m := mockBangumiWatch(t, app)
	u, _ := seedBangumiWatch(t, app)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second, err := store.OpenPostgres(ctx, os.Getenv("TWILIGHT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	release, acquired, err := second.LockBangumiSync(ctx, u.UID)
	if err != nil || !acquired {
		t.Fatal("lock failed", err)
	}
	done, _, failed, _ := app.syncBangumiForUser(context.Background(), u.UID)
	if done != 0 || failed == 0 || m.writes.Load() != 0 {
		t.Fatal("concurrent sync allowed")
	}
	release()
	done, _, failed, _ = app.syncBangumiForUser(context.Background(), u.UID)
	if done != 1 || failed != 0 {
		t.Fatal("lock not released")
	}
}
