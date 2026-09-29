package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestParsePlaybackReportingRowsReadsPositionalColumns 锁住解析约定：插件返回的是
// 二维数组，没有列名到字段的映射，只能按我们自己写的 SELECT 顺序取值。列顺序一
// 旦变动，这里先炸。
func TestParsePlaybackReportingRowsReadsPositionalColumns(t *testing.T) {
	resp := playbackReportingQueryResponse{
		Colums: []string{"UserId", "ItemId", "ItemType", "ItemName", "PlayDuration", "PauseDuration", "DateCreated"},
		Results: [][]any{
			{"abc123", "item-9", "Episode", "某某剧 - 第 3 集", float64(3600), float64(600), "2026-09-18 14:03:22"},
			{"abc123", "item-8", "Movie", "某电影", "1800", "0", "2026-09-18 12:00:00"},
			nil,
			{"too", "few"},
		},
	}
	rows := parsePlaybackReportingRows(resp)
	if len(rows) != 2 {
		t.Fatalf("expected 2 parsed rows, got %d", len(rows))
	}
	first := rows[0]
	if first.UserID != "abc123" || first.ItemID != "item-9" || first.ItemType != "Episode" {
		t.Fatalf("unexpected identity columns: %#v", first)
	}
	// 数字既可能是 JSON number 也可能是字符串，两种都要读成整数。
	if first.PlayDuration != 3600 || first.PauseDuration != 600 {
		t.Fatalf("duration columns must accept numbers: %#v", first)
	}
	second := rows[1]
	if second.PlayDuration != 1800 || second.PauseDuration != 0 {
		t.Fatalf("duration columns must accept strings: %#v", second)
	}
	if first.PlayedAt == 0 || second.PlayedAt == 0 {
		t.Fatalf("timestamps must be parsed: %#v %#v", first, second)
	}
	if first.PlayedAt <= second.PlayedAt {
		t.Fatalf("later row should have a later timestamp: %d vs %d", first.PlayedAt, second.PlayedAt)
	}
}

// TestParsePlaybackReportingTimeAcceptsPluginFormats 覆盖插件写进 SQLite 的几种
// 时间写法。解析不出来必须返回 0——调用方会跳过该行，一条时间错的记录比缺一条
// 记录更糟（它会污染榜单的时间窗口）。
func TestParsePlaybackReportingTimeAcceptsPluginFormats(t *testing.T) {
	for _, value := range []string{
		"2026-09-18 14:03:22",
		"2026-09-18 14:03:22.000",
		"2026-09-18T14:03:22",
		"2026-09-18T14:03:22Z",
	} {
		if got := parsePlaybackReportingTime(value); got <= 0 {
			t.Fatalf("failed to parse %q: %d", value, got)
		}
	}
	for _, value := range []string{"", "   ", "not a time", "2026/09/18"} {
		if got := parsePlaybackReportingTime(value); got != 0 {
			t.Fatalf("unparsable %q must yield 0, got %d", value, got)
		}
	}
	// 同一天同一时刻的两种写法必须解析成同一个值，否则窗口过滤会漏数据。
	plain := parsePlaybackReportingTime("2026-09-18 14:03:22")
	millis := parsePlaybackReportingTime("2026-09-18 14:03:22.000")
	if plain != millis {
		t.Fatalf("same instant with and without millis diverged: %d vs %d", plain, millis)
	}
}

// TestPlaybackReportingDisabledShortCircuits 保证开关关掉时一次请求都不发：探测本
// 身要往 Emby 发一条自定义 SQL，配置里明确关掉就不该有任何出站流量。
func TestPlaybackReportingDisabledShortCircuits(t *testing.T) {
	app := &App{}
	app.cfg().EmbyURL = "http://127.0.0.1:1"
	app.cfg().EmbyToken = "token"
	app.cfg().PlaybackReportingEnabled = false

	if app.playbackReportingEnabled() {
		t.Fatal("disabled switch must not report enabled")
	}
	updated, inserted, err := app.syncPlaybackReporting(context.Background(), time.Now().Add(-time.Hour), time.Now())
	if err != nil || updated != 0 || inserted != 0 {
		t.Fatalf("disabled source must be a no-op: updated=%d inserted=%d err=%v", updated, inserted, err)
	}
}

func TestPlaybackReportingStringNormalizesCellValues(t *testing.T) {
	if got := playbackReportingString(nil); got != "" {
		t.Fatalf("nil must be empty, got %q", got)
	}
	if got := playbackReportingString("  spaced  "); got != "spaced" {
		t.Fatalf("strings must be trimmed, got %q", got)
	}
	if got := playbackReportingString(float64(42)); got != "42" {
		t.Fatalf("numbers must render as integers, got %q", got)
	}
}

// TestFetchPlaybackReportingRowsOrdersAndPages 回归审查 L12：旧查询是无 ORDER BY 的
// LIMIT 5000，超出的行会被随机截掉；现在要稳定排序并翻页取完。
func TestFetchPlaybackReportingRowsOrdersAndPages(t *testing.T) {
	queries := []string{}
	emby := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		query, _ := body["CustomQueryString"].(string)
		queries = append(queries, query)
		n := playbackReportingMaxRows
		if len(queries) > 1 {
			n = 1
		}
		results := make([][]any, 0, n)
		for i := 0; i < n; i++ {
			results = append(results, []any{"u", fmt.Sprintf("item-%d-%d", len(queries), i), "Movie", "x", 60, 0, "2026-09-18 14:03:22"})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"colums": []string{}, "results": results})
	}))
	defer emby.Close()
	app := newTestApp(t)
	app.cfg().EmbyURL = emby.URL
	app.cfg().EmbyToken = "token"

	rows, err := app.fetchPlaybackReportingRows(context.Background(), time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != playbackReportingMaxRows+1 || len(queries) != 2 {
		t.Fatalf("expected two pages (%d rows), got %d rows in %d queries", playbackReportingMaxRows+1, len(rows), len(queries))
	}
	for _, q := range queries {
		if !strings.Contains(q, "ORDER BY DateCreated") {
			t.Fatalf("query must be ordered: %s", q)
		}
	}
	if !strings.Contains(queries[1], fmt.Sprintf("OFFSET %d", playbackReportingMaxRows)) {
		t.Fatalf("second page must use an offset: %s", queries[1])
	}
}
