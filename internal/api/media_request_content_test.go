package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

func mediaRequesterForTest(t *testing.T, app *App, username string, telegramID int64) []*http.Cookie {
	t.Helper()
	cookies := registerAndLogin(t, app, username, "User123456")
	user, ok := app.store().FindUserByUsername(username)
	if !ok {
		t.Fatalf("%s not found", username)
	}
	if _, err := app.store().UpdateUser(user.UID, func(u *store.User) error { u.TelegramID = telegramID; return nil }); err != nil {
		t.Fatal(err)
	}
	return cookies
}

// 客户端伪造的 inventory_issue / 任意键 / 外站 poster 不能进入 media_info，也不能跳过去重。
func TestMediaRequestMediaInfoIsWhitelisted(t *testing.T) {
	app := newTestApp(t)
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	first := mediaRequesterForTest(t, app, "requester-a", 1001)
	second := mediaRequesterForTest(t, app, "requester-b", 1002)

	body := `{"source":"tmdb","media_id":"123","media_type":"movie","title":"` + strings.Repeat("T", 400) + `","inventory_issue":true,"inventory_exists":true,"poster":"https://attacker.example/p.png","evil":"x","overview":"ok","vote_average":7.25}`
	rr := doJSON(app, http.MethodPost, "/api/v2/media/requests", body, first)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	user, _ := app.store().FindUserByUsername("requester-a")
	requests := app.store().ListMediaRequests(user.UID, false)
	if len(requests) != 1 {
		t.Fatalf("expected one request, got %#v", requests)
	}
	info := requests[0].MediaInfo
	for _, key := range []string{"inventory_issue", "inventory_exists", "poster", "poster_url", "evil"} {
		if _, exists := info[key]; exists {
			t.Fatalf("media_info should not keep client key %q: %#v", key, info)
		}
	}
	if len([]rune(requests[0].Title)) > mediaRequestTitleMaxRunes || info["vote_average"] != 7.3 || info["overview"] != "ok" {
		t.Fatalf("unexpected sanitized request: title=%d info=%#v", len([]rune(requests[0].Title)), info)
	}

	dup := doJSON(app, http.MethodPost, "/api/v2/media/requests", `{"source":"tmdb","media_id":"123","media_type":"movie","title":"x","inventory_issue":true}`, second)
	if dup.Code != http.StatusBadRequest || !strings.Contains(dup.Body.String(), "MEDIA_REQUEST_ALREADY_EXISTS") {
		t.Fatalf("forged inventory_issue must not bypass dedupe, status=%d body=%s", dup.Code, dup.Body.String())
	}
}

func TestTrustedMediaPosterURL(t *testing.T) {
	app := newTestApp(t)
	app.cfg().TMDBImageURL = "https://tmdb-proxy.example.org/t/p"
	cases := map[string]string{
		"https://image.tmdb.org/t/p/w500/a.jpg":         "https://image.tmdb.org/t/p/w500/a.jpg",
		"https://lain.bgm.tv/pic/cover/l/1.jpg":         "https://lain.bgm.tv/pic/cover/l/1.jpg",
		"https://tmdb-proxy.example.org/t/p/w500/b.jpg": "https://tmdb-proxy.example.org/t/p/w500/b.jpg",
		"http://image.tmdb.org/t/p/w500/a.jpg":          "",
		"https://attacker.example/p.png":                "",
		"https://image.tmdb.org.attacker.example/x.jpg": "",
		"https://user@image.tmdb.org/x.jpg":             "",
		"https://image.tmdb.org:8443/x.jpg":             "",
		"javascript:alert(1)":                           "",
	}
	for input, want := range cases {
		if got := app.trustedMediaPosterURL(input); got != want {
			t.Errorf("trustedMediaPosterURL(%q)=%q, want %q", input, got, want)
		}
	}
}

// 库存搜索只允许 Movie / Series，受求片开关与每用户限流约束。
func TestInventorySearchRestrictsTypesAndHonorsGuards(t *testing.T) {
	app := newTestApp(t)
	var seenTypes []string
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenTypes = append(seenTypes, r.URL.Query().Get("IncludeItemTypes"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Items":[],"TotalRecordCount":0}`))
	}))
	defer emby.Close()
	app.cfg().EmbyURL = emby.URL
	app.cfg().EmbyToken = "test-token"
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	user := registerAndLogin(t, app, "inventory-user", "User123456")

	if rr := doJSON(app, http.MethodGet, "/api/v2/media/inventory/search?q=a&type=Photo,Video,Folder", ``, user); rr.Code != http.StatusBadRequest {
		t.Fatalf("arbitrary type status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(seenTypes) != 0 {
		t.Fatalf("rejected type must not reach Emby, saw %v", seenTypes)
	}
	if rr := doJSON(app, http.MethodGet, "/api/v2/media/inventory/search?q=a", ``, user); rr.Code != http.StatusOK {
		t.Fatalf("default search status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(app, http.MethodGet, "/api/v1/media/inventory/search?q=a&type=series", ``, user); rr.Code != http.StatusOK {
		t.Fatalf("series search status=%d body=%s", rr.Code, rr.Body.String())
	}
	if len(seenTypes) != 2 || seenTypes[0] != "Movie,Series" || seenTypes[1] != "Series" {
		t.Fatalf("unexpected IncludeItemTypes: %v", seenTypes)
	}

	app.cfg().MediaRequestEnabled = false
	if rr := doJSON(app, http.MethodGet, "/api/v2/media/inventory/search?q=a", ``, user); rr.Code != http.StatusForbidden {
		t.Fatalf("disabled media requests status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := doJSON(app, http.MethodPost, "/api/v2/media/inventory/check", `{"title":"x"}`, user); rr.Code != http.StatusForbidden {
		t.Fatalf("disabled inventory check status=%d body=%s", rr.Code, rr.Body.String())
	}

	app.cfg().MediaRequestEnabled = true
	app.cfg().RateLimitEnabled = true
	limited := false
	for i := 0; i < mediaInventoryRateLimitPerMinute+2; i++ {
		if rr := doJSON(app, http.MethodGet, "/api/v2/media/inventory/search?q=a", ``, user); rr.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("inventory search should be rate limited per user")
	}
}

// 库存检查失败不能把内部 Emby 地址回给用户。
func TestInventoryCheckFailureHidesInternalEmbyURL(t *testing.T) {
	app := newTestApp(t)
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	internalURL := emby.URL
	emby.Close() // 连接被拒，错误里会带上内部地址
	app.cfg().EmbyURL = internalURL
	app.cfg().EmbyToken = "test-token"
	_ = registerAndLogin(t, app, "admin", "Admin123456")
	user := registerAndLogin(t, app, "inventory-user", "User123456")

	rr := doJSON(app, http.MethodPost, "/api/v2/media/inventory/check", `{"source":"tmdb","media_id":"550","title":"x","media_type":"movie"}`, user)
	host := strings.TrimPrefix(internalURL, "http://")
	if strings.Contains(rr.Body.String(), host) || !strings.Contains(rr.Body.String(), inventoryCheckFailedMessage) {
		t.Fatalf("inventory failure leaked internal address or missing fixed message: %s", rr.Body.String())
	}
}
