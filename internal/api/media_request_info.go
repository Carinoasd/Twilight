package api

import (
	"math"
	"net/url"
	"strconv"
	"strings"
)

// 求片 media_info 的字段上限。原实现把 POST body 的全部键原样复制进 media_info，
// 用户可以写入 inventory_issue 跳过去重、写任意外站 poster 让管理员浏览器去请求，
// 每条还能塞约 256KB。现在只接受白名单字段并限长，inventory_* 只能由服务端写入。
const (
	mediaRequestTitleMaxRunes    = 200
	mediaRequestNoteMaxRunes     = 500
	mediaRequestOverviewMaxRunes = 2000
	mediaRequestShortFieldRunes  = 32
	mediaRequestPosterMaxBytes   = 1024
)

// sanitizeMediaRequestInfo 从客户端 payload 构造可存储的 media_info。
func (a *App) sanitizeMediaRequestInfo(payload map[string]any, title, source string) map[string]any {
	info := map[string]any{"title": title, "source": source}
	for _, key := range []string{"original_title"} {
		if value := strings.TrimSpace(stringValue(payload, key)); value != "" {
			info[key] = truncateString(value, mediaRequestTitleMaxRunes)
		}
	}
	for _, key := range []string{"year", "release_date", "media_type"} {
		if value := strings.TrimSpace(stringValue(payload, key)); value != "" {
			info[key] = truncateString(value, mediaRequestShortFieldRunes)
		}
	}
	if value := strings.TrimSpace(stringValue(payload, "overview")); value != "" {
		info["overview"] = truncateString(value, mediaRequestOverviewMaxRunes)
	}
	if value := strings.TrimSpace(stringValue(payload, "note")); value != "" {
		info["note"] = truncateString(value, mediaRequestNoteMaxRunes)
	}
	if season := intValue(payload, "season", 0); season > 0 && season <= 1000 {
		info["season"] = season
	}
	for _, key := range []string{"vote_average", "rating"} {
		if score, ok := mediaRequestScore(payload[key]); ok {
			info[key] = score
		}
	}
	// poster 只接受 https 且主机属于 TMDB 图床（含配置的 tmdb_image_url）或 Bangumi 图床。
	if poster := a.trustedMediaPosterURL(firstNonEmpty(stringValue(payload, "poster"), stringValue(payload, "poster_url"))); poster != "" {
		info["poster"] = poster
		info["poster_url"] = poster
	}
	return info
}

func mediaRequestScore(value any) (float64, bool) {
	var score float64
	switch typed := value.(type) {
	case float64:
		score = typed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, false
		}
		score = parsed
	default:
		return 0, false
	}
	if math.IsNaN(score) || math.IsInf(score, 0) || score <= 0 || score > 10 {
		return 0, false
	}
	return math.Round(score*10) / 10, true
}

// trustedMediaPosterURL 返回规范化后的海报地址；不在白名单内返回空串。
func (a *App) trustedMediaPosterURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > mediaRequestPosterMaxBytes {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Port() != "" && parsed.Port() != "443" {
		return ""
	}
	if !a.isTrustedMediaPosterHost(host) {
		return ""
	}
	parsed.Fragment = ""
	return parsed.String()
}

func (a *App) isTrustedMediaPosterHost(host string) bool {
	if host == "image.tmdb.org" || isBangumiImageHost(host) {
		return true
	}
	if base, err := url.Parse(strings.TrimSpace(a.cfg().TMDBImageURL)); err == nil && base.Scheme == "https" {
		if configured := strings.ToLower(base.Hostname()); configured != "" && configured == host {
			return true
		}
	}
	return false
}
