package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

type bangumiWatchItem struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	SeriesName  string `json:"series_name"`
	MediaType   string `json:"media_type"`
	IndexNumber int    `json:"index_number"`
	PlayedAt    int64  `json:"played_at"`
	store.BangumiWatchRecord
}

func bangumiWatchItems(u store.User, records []store.PlaybackRecord) []bangumiWatchItem {
	out := make([]bangumiWatchItem, 0, len(records))
	seen := map[string]bool{}
	for _, r := range records {
		key := store.BangumiRecordKey(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, bangumiWatchItem{key, r.Title, r.SeriesName, r.MediaType, r.IndexNumber, r.PlayedAt, bangumiWatchState(u, r, bangumiWatchAccount(u))})
	}
	return out
}
func (a *App) handleBangumiWatchRecords(w http.ResponseWriter, r *http.Request, _ Params) {
	if a.requireBangumiSyncEnabled(w) {
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	u := current(r).User
	page, size := clamp(queryInt(r, "page", 1), 1, 100000), clamp(queryInt(r, "per_page", 20), 1, 50)
	filter := r.URL.Query().Get("status")
	switch filter {
	case "", "pending", "success", "needs_review", "failed", "ignored":
	default:
		failWithCode(w, 400, ErrBadRequest, "无效的同步状态")
		return
	}
	items := bangumiWatchItems(u, a.store().PlaybackRecords(u.UID, 0, 5000))
	counts := map[string]int{}
	filtered := make([]bangumiWatchItem, 0)
	for _, item := range items {
		counts[item.Status]++
		if filter == "" || item.Status == filter {
			filtered = append(filtered, item)
		}
	}
	ok(w, "OK", map[string]any{"items": paginate(filtered, page, size), "total": len(filtered), "page": page, "per_page": size, "counts": counts, "account_verified": bangumiWatchAccount(u) != ""})
}
func (a *App) handleBangumiWatchUpdate(w http.ResponseWriter, r *http.Request, p Params) {
	if a.requireBangumiSyncEnabled(w) {
		return
	}
	u := current(r).User
	if u.BGMToken == "" {
		failWithCode(w, 400, ErrBangumiTokenMissing, "请先配置 Bangumi Token")
		return
	}
	payload := decodeMap(r)
	action := asString(payload["action"])
	if action != "confirm" && action != "ignore" && action != "retry" {
		failWithCode(w, 400, ErrBadRequest, "无效操作")
		return
	}
	var record store.PlaybackRecord
	found := false
	for _, item := range a.store().PlaybackRecords(u.UID, 0, 5000) {
		if store.BangumiRecordKey(item) == p["key"] {
			record, found = item, true
			break
		}
	}
	if !found {
		failWithCode(w, 404, ErrNotFound, "观看记录不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	release, acquired, err := a.store().LockBangumiSync(ctx, u.UID)
	if err != nil || !acquired {
		failWithCode(w, 409, ErrBadRequest, "同步正在运行，请稍后重试")
		return
	}
	defer release()
	me, expired, err := a.getBangumiMe(ctx, u.BGMToken)
	if err != nil || expired {
		failWithCode(w, 502, ErrBadRequest, "无法验证 Bangumi 账号，请检查 Token 后重试")
		return
	}
	account, err := a.rememberBangumiAccount(u, me)
	if err != nil {
		failWithCode(w, 500, ErrInternal, "保存账号状态失败")
		return
	}
	state := bangumiWatchState(u, record, account)
	switch action {
	case "ignore":
		state.Status, state.Message = "ignored", "已忽略"
	case "retry":
		state.Status, state.Message = "pending", ""
	case "confirm":
		id := strings.TrimSpace(asString(payload["subject_id"]))
		ep, valid := strictBangumiInt(payload["episode"], 0, 10000)
		if !isPositiveNumericID(id) || !valid || (strings.EqualFold(record.MediaType, "Episode") && ep < 1) {
			failWithCode(w, 400, ErrBadRequest, "请填写有效的 Bangumi 条目 ID 和集数")
			return
		}
		endpoint, err := bangumiEndpoint(a.cfg().BangumiAPIURL, "/subjects/"+id, nil)
		if err != nil {
			failWithCode(w, 502, ErrInternal, "Bangumi 不可用")
			return
		}
		var subject map[string]any
		if err := getJSON(ctx, endpoint, a.bangumiUserHeaders(u.BGMToken), &subject); err != nil {
			failWithCode(w, 502, ErrBadRequest, "无法读取指定的 Bangumi 条目")
			return
		}
		if kind := int(numeric(subject["type"])); kind != 2 && kind != 6 {
			failWithCode(w, 400, ErrBadRequest, "条目不是动画或三次元影视")
			return
		}
		state.SubjectID, state.SubjectName = id, firstNonEmpty(asString(subject["name_cn"]), asString(subject["name"]))
		state.Completed, state.Manual, state.Episode, state.Status, state.Message = true, true, ep, "pending", ""
	}
	if err := a.store().UpdateBangumiWatch(u.UID, account+":"+p["key"], u.BGMToken, func(v *store.BangumiWatchRecord) { *v = state }); err != nil {
		failWithCode(w, 500, ErrInternal, "保存同步设置失败")
		return
	}
	a.audit(r, "update_bangumi_watch", "user", u.UID, map[string]any{"record_key": p["key"], "action": action, "subject_id": state.SubjectID})
	ok(w, "已保存", map[string]any{"state": state})
}
func (a *App) runBangumiWatchSync(ctx context.Context) (map[string]any, []string, error) {
	if !a.cfg().BangumiEnabled {
		return map[string]any{"success": true, "enabled": false}, nil, nil
	}
	ids, _ := a.store().UserUIDsMatching(0, func(u store.User) bool { return u.Active && u.BGMMode && u.BGMToken != "" })
	total, skipped, failed := 0, 0, 0
	for _, uid := range ids {
		if ctx.Err() != nil {
			return map[string]any{"success": false, "synced": total, "failed": failed}, nil, ctx.Err()
		}
		done, skip, fail, _ := a.syncBangumiForUser(ctx, uid)
		total += done
		skipped += skip
		failed += fail
	}
	return map[string]any{"success": failed == 0, "synced": total, "skipped": skipped, "failed": failed, "users": len(ids)}, nil, nil
}
