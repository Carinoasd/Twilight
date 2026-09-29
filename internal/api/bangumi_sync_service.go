package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/prejudice-studio/twilight/internal/store"
)

func bangumiTokenHash(token string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(token))) }
func bangumiWatchAccount(u store.User) string {
	if u.BGMToken == "" || u.BGMAccountTokenHash != bangumiTokenHash(u.BGMToken) {
		return ""
	}
	return u.BGMAccountID
}
func (a *App) rememberBangumiAccount(u store.User, me map[string]any) (string, error) {
	id := asString(me["id"])
	if !isPositiveNumericID(id) {
		return "", fmt.Errorf("Bangumi 账号信息无效")
	}
	if bangumiWatchAccount(u) == id {
		return id, nil
	}
	_, err := a.store().UpdateUser(u.UID, func(current *store.User) error {
		if current.BGMToken != u.BGMToken {
			return fmt.Errorf("Bangumi 账号已变更，请刷新重试")
		}
		current.BGMAccountID, current.BGMAccountTokenHash = id, bangumiTokenHash(u.BGMToken)
		return nil
	})
	return id, err
}
func bangumiWatchState(u store.User, r store.PlaybackRecord, account string) store.BangumiWatchRecord {
	key := store.BangumiRecordKey(r)
	evidence := u.BangumiWatch[key]
	state, found := u.BangumiWatch[account+":"+key]
	if !found || account == "" {
		state = evidence
		state.Status = "pending"
	}
	state.Completed = state.Completed || evidence.Completed
	if state.Season == 0 {
		state.Season = evidence.Season
	}
	if state.Status == "needs_review" && state.Completed && state.Message == "尚未确认看完，请确认后再同步" {
		state.Status = "pending"
	}
	return state
}
func bangumiSyncError(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case strings.Contains(err.Error(), "401"), strings.Contains(err.Error(), "403"):
		return "Bangumi Token 无效或权限不足"
	case strings.Contains(err.Error(), "429"):
		return "Bangumi 请求过于频繁，请稍后重试"
	case strings.Contains(err.Error(), "context"):
		return "同步超时或已取消，可重试未完成项目"
	default:
		return "Bangumi 请求失败，请稍后重试"
	}
}

func (a *App) syncBangumiForUser(ctx context.Context, uid int64) (synced int, skipped int, failed int, logs []string) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if !a.cfg().BangumiEnabled {
		return 0, 0, 1, []string{"Bangumi 同步已关闭"}
	}
	release, acquired, err := a.store().LockBangumiSync(ctx, uid)
	if err != nil {
		return 0, 0, 1, []string{"同步锁不可用，请稍后重试"}
	}
	if !acquired {
		return 0, 0, 1, []string{"该账号已有同步任务，请稍后刷新"}
	}
	defer release()
	if err := a.store().Refresh(); err != nil {
		return 0, 0, 1, []string{"读取同步状态失败"}
	}
	u, ok := a.store().User(uid)
	if !ok || !u.Active || !u.BGMMode || u.BGMToken == "" {
		return 0, 0, 1, []string{"用户未开启 Bangumi 同步或未配置个人 Token"}
	}
	me, expired, err := a.getBangumiMe(ctx, u.BGMToken)
	if err != nil {
		return 0, 0, 1, []string{bangumiSyncError(err)}
	}
	if expired {
		return 0, 0, 1, []string{"Bangumi Token 已过期"}
	}
	account, err := a.rememberBangumiAccount(u, me)
	if err != nil {
		return 0, 0, 1, []string{"验证 Bangumi 账号失败"}
	}
	records := a.store().PlaybackRecords(uid, 0, 5000)
	sort.SliceStable(records, func(i, j int) bool {
		return bangumiWatchState(u, records[i], account).UpdatedAt < bangumiWatchState(u, records[j], account).UpdatedAt
	})
	seen := map[string]bool{}
	attempted := 0
	for _, record := range records {
		if ctx.Err() != nil {
			failed++
			logs = append(logs, "同步超时或已取消，可重试未完成项目")
			break
		}
		key := store.BangumiRecordKey(record)
		if seen[key] {
			continue
		}
		seen[key] = true
		state := bangumiWatchState(u, record, account)
		if state.Status == "success" || state.Status == "ignored" || state.Status == "needs_review" {
			skipped++
			continue
		}
		if attempted >= 25 {
			break
		}
		attempted++
		// Recheck current account/preferences before every outbound mutation.
		current, found := a.store().User(uid)
		if !found || !current.Active || !current.BGMMode || current.BGMToken != u.BGMToken {
			failed++
			logs = append(logs, "同步设置已变更，请刷新重试")
			break
		}
		state.Status, state.Message = "needs_review", ""
		switch {
		case a.bangumiBlockedRecord(record):
			state.Status, state.Message = "ignored", "命中管理员设置的屏蔽关键词"
		case record.ItemID == "":
			state.Message = "缺少媒体标识，无法安全同步"
		case !state.Completed:
			state.Message = "尚未确认看完，请确认后再同步"
		case !strings.EqualFold(record.MediaType, "Episode") && !strings.EqualFold(record.MediaType, "Movie"):
			state.Message = "不支持的媒体类型，请在收藏页面管理"
		case state.Season > 1 && !state.Manual:
			state.Message = "多季作品需要确认对应的 Bangumi 条目"
		default:
			subjectID, name := state.SubjectID, state.SubjectName
			if !state.Manual {
				subjectID, name, err = a.matchBangumiSubject(ctx, record, a.bangumiUserHeaders(u.BGMToken))
				if err != nil {
					state.Status, state.Message = "failed", bangumiSyncError(err)
				}
				if err == nil && subjectID == "" {
					state.Message = "无法唯一匹配条目，请指定 Bangumi ID"
				}
			}
			if subjectID != "" && state.Message == "" {
				state.SubjectID, state.SubjectName = subjectID, name
				episode := record.IndexNumber
				if state.Manual {
					episode = state.Episode
				}
				movie := strings.EqualFold(record.MediaType, "Movie")
				if !movie && episode <= 0 {
					state.Message = "缺少有效集数，请手动确认"
				} else {
					// Bangumi requires the username once one has been set; the numeric
					// account ID is stable for checkpoints but is not always a valid URL key.
					username := firstNonEmpty(asString(me["username"]), account)
					err = a.ensureBangumiCollection(ctx, subjectID, username, u.BGMToken, movie)
					if err == nil && !movie {
						err = a.markBangumiEpisode(ctx, subjectID, episode, u.BGMToken)
					}
					if err != nil {
						state.Status, state.Message = "failed", bangumiSyncError(err)
						if errors.Is(err, errBangumiCollectionRequired) {
							state.Status, state.Message = "needs_review", "请先在 Bangumi 收藏该条目，再重试同步"
						}
					} else {
						state.Status, state.Message, state.Episode = "success", "已同步", episode
					}
				}
			}
		}
		if err := a.store().UpdateBangumiWatch(uid, account+":"+key, u.BGMToken, func(v *store.BangumiWatchRecord) { *v = state }); err != nil {
			failed++
			logs = append(logs, "保存同步检查点失败，请稍后重试")
			break
		}
		switch state.Status {
		case "success":
			synced++
		case "failed":
			failed++
		default:
			skipped++
		}
		logs = append(logs, firstNonEmpty(record.SeriesName, record.Title)+": "+state.Message)
		if err := a.store().AddBangumiSyncLog(store.BangumiSyncLog{UID: uid, RecordItemID: record.ItemID, SubjectID: state.SubjectID, SubjectName: state.SubjectName, Episode: state.Episode, Status: state.Status, Message: state.Message}); err != nil {
			failed++
			logs = append(logs, "同步状态已保存，但日志写入失败")
			break
		}
		if state.Status == "failed" && (strings.Contains(state.Message, "Token") || strings.Contains(state.Message, "频繁")) {
			break
		}
	}
	if synced > 0 {
		_ = a.store().DeleteBangumiCollectionCache(uid, 0)
	}
	return
}

func episodeSuffix(ep int) string {
	if ep > 0 {
		return fmt.Sprintf(" 第%d话", ep)
	}
	return ""
}

var errBangumiCollectionRequired = errors.New("Bangumi collection required")

func (a *App) bangumiBlockedRecord(record store.PlaybackRecord) bool {
	title := strings.ToLower(record.SeriesName + " " + record.Title)
	for _, keyword := range a.cfg().BangumiBlockKeywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword != "" && strings.Contains(title, keyword) {
			return true
		}
	}
	return false
}
func bangumiMatchName(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, value)
}
func (a *App) matchBangumiSubject(ctx context.Context, record store.PlaybackRecord, headers map[string]string) (subjectID, subjectName string, err error) {
	query := firstNonEmpty(record.SeriesName, record.Title)
	if strings.TrimSpace(query) == "" {
		return "", "", nil
	}
	endpoint, err := bangumiEndpoint(a.cfg().BangumiAPIURL, "/search/subjects", url.Values{"limit": {"10"}, "offset": {"0"}})
	if err != nil {
		return "", "", err
	}
	var payload map[string]any
	if err := postJSON(ctx, endpoint, headers, map[string]any{"keyword": query, "sort": "match", "filter": map[string]any{"type": []int{2, 6}}}, &payload); err != nil {
		return "", "", err
	}
	rows, _ := payload["data"].([]any)
	for _, row := range rows {
		item, _ := row.(map[string]any)
		if item == nil {
			continue
		}
		if bangumiMatchName(asString(item["name"])) != bangumiMatchName(query) && bangumiMatchName(asString(item["name_cn"])) != bangumiMatchName(query) {
			continue
		}
		id := asString(item["id"])
		if !isPositiveNumericID(id) {
			continue
		}
		if subjectID != "" && subjectID != id {
			return "", "", nil
		}
		subjectID, subjectName = id, firstNonEmpty(asString(item["name_cn"]), asString(item["name"]))
	}
	return
}
func (a *App) ensureBangumiCollection(ctx context.Context, subjectID, username, token string, movie bool) error {
	endpoint, err := bangumiEndpoint(a.cfg().BangumiAPIURL, "/users/"+url.PathEscape(username)+"/collections/"+subjectID, nil)
	if err != nil {
		return err
	}
	var existing map[string]any
	err = getJSON(ctx, endpoint, a.bangumiUserHeaders(token), &existing)
	if err == nil {
		if movie && int(numeric(existing["type"])) != 2 {
			return a.updateBangumiCollection(ctx, subjectID, token, 2, 0, false)
		}
		return nil
	} // Episode sync never downgrades an existing collection.
	if !strings.Contains(err.Error(), "remote status 404") {
		return err
	}
	if !a.cfg().BangumiAutoAddCollection {
		return errBangumiCollectionRequired
	}
	endpoint, err = bangumiEndpoint(a.cfg().BangumiAPIURL, "/users/-/collections/"+subjectID, nil)
	if err != nil {
		return err
	}
	kind := 3
	if movie {
		kind = 2
	}
	return postJSON(ctx, endpoint, a.bangumiUserHeaders(token), map[string]any{"type": kind, "private": a.cfg().BangumiPrivateCollection}, nil)
}
func (a *App) markBangumiEpisode(ctx context.Context, subjectID string, episode int, token string) error {
	episodes, err := a.bangumiEpisodes(ctx, subjectID, token)
	if err != nil {
		return err
	}
	for _, ep := range episodes {
		if ep.Number == episode {
			return a.patchBangumiEpisodes(ctx, subjectID, token, []int{ep.ID}, 2)
		}
	}
	return fmt.Errorf("Bangumi 未找到第 %s 话", strconv.Itoa(episode))
}
