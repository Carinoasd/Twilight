package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
	"go.uber.org/zap"
)

// bangumiWebhookReplayWindowSeconds 是 X-Twilight-Bangumi-Timestamp 的容忍窗
// 口。攻击者抓到一份合法请求后，在窗口外重放会被直接拒绝；窗口内的重放仍然
// 由 store 层的 (UID, ItemID, PlayedAt) 幂等键挡住的双层防御。
//
// 客户端时钟漂移最常见在 ±60s，留 5 分钟避免合法请求被误杀。
const bangumiWebhookReplayWindowSeconds = 300

// Bangumi webhook 有两种鉴权方式：
//
//  1. 签名模式（推荐）：X-Twilight-Bangumi-Timestamp 必填，
//     X-Twilight-Bangumi-Signature = "sha256=" + hex(HMAC-SHA256(secret, timestamp + "." + body))。
//     时间戳在窗口内、签名覆盖 body，且同一签名在窗口内只接受一次，截获的请求无法改写或重放。
//  2. 旧的共享 token 模式（X-Twilight-Bangumi-Token / X-Webhook-Token 头，或已淘汰的 ?token=）：
//     token 只是 bearer 口令，时间戳可自填，防重放只是装饰。仅在
//     BangumiSync.webhook_allow_legacy_token=true（默认，兼容期）时接受，每次命中都记 Warn；
//     改为 false 后只接受签名模式。
const (
	bangumiWebhookSignatureHeader = "X-Twilight-Bangumi-Signature"
	bangumiWebhookTimestampHeader = "X-Twilight-Bangumi-Timestamp"
	bangumiWebhookSignaturePrefix = "sha256="
)

// bangumiWebhookSignature 计算签名，供文档示例与测试使用。
func bangumiWebhookSignature(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return bangumiWebhookSignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// bangumiWebhookReplayCache 记住窗口内已接受的签名，拒绝逐字节重放。
// 进程内存即可：跨实例的重放仍会被 store 的 (uid,item_id,played_at) 幂等键去重。
type bangumiWebhookReplayCache struct {
	mu   sync.Mutex
	seen map[string]int64
}

var bangumiWebhookReplays = &bangumiWebhookReplayCache{seen: map[string]int64{}}

// remember 返回 false 表示该签名在有效期内已出现过。
func (c *bangumiWebhookReplayCache) remember(signature string, now int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, expiresAt := range c.seen {
		if expiresAt <= now {
			delete(c.seen, key)
		}
	}
	if expiresAt, exists := c.seen[signature]; exists && expiresAt > now {
		return false
	}
	c.seen[signature] = now + 2*bangumiWebhookReplayWindowSeconds
	return true
}

func parseBangumiWebhookTimestamp(raw string) (int64, bool, bool) {
	ts, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, false, false
	}
	drift := time.Now().Unix() - ts
	if drift < 0 {
		drift = -drift
	}
	return ts, true, drift <= bangumiWebhookReplayWindowSeconds
}

func (a *App) handleBangumiWebhook(w http.ResponseWriter, r *http.Request, _ Params) {
	if a.requireBangumiSyncEnabled(w) {
		return
	}
	secretConfigured := a.cfg().BangumiWebhookSecret
	if secretConfigured == "" {
		failWithCode(w, http.StatusForbidden, ErrUnauthorized, "Webhook 密钥无效")
		return
	}
	var headerPlayedAt int64
	if signature := strings.TrimSpace(r.Header.Get(bangumiWebhookSignatureHeader)); signature != "" {
		// 签名模式：时间戳必填；先限量读 body 再验签，验签失败不解析 JSON。
		tsHeader := strings.TrimSpace(r.Header.Get(bangumiWebhookTimestampHeader))
		if tsHeader == "" {
			failWithCode(w, http.StatusUnauthorized, ErrUnauthorized, "签名请求必须携带 "+bangumiWebhookTimestampHeader)
			return
		}
		ts, valid, fresh := parseBangumiWebhookTimestamp(tsHeader)
		if !valid {
			failWithCode(w, http.StatusBadRequest, ErrUnauthorized, "Webhook timestamp 非法")
			return
		}
		if !fresh {
			failWithCode(w, http.StatusGone, ErrUnauthorized, "Webhook 请求已过期")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
		if err != nil {
			failWithCode(w, http.StatusRequestEntityTooLarge, ErrBadRequest, "Webhook 请求体过大")
			return
		}
		expected := bangumiWebhookSignature(secretConfigured, ts, body)
		if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(expected)) {
			failWithCode(w, http.StatusForbidden, ErrUnauthorized, "Webhook 签名无效")
			return
		}
		if !bangumiWebhookReplays.remember(expected, time.Now().Unix()) {
			failWithCode(w, http.StatusConflict, ErrUnauthorized, "Webhook 请求重复")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		headerPlayedAt = ts
	} else {
		// 旧 token 模式：只在兼容期接受。优先 header，?token= 已淘汰（会进代理 / CDN access log）。
		secret := firstNonEmpty(r.Header.Get("X-Twilight-Bangumi-Token"), r.Header.Get("X-Webhook-Token"))
		usingQuerySecret := false
		if secret == "" {
			if q := r.URL.Query().Get("token"); q != "" {
				secret = q
				usingQuerySecret = true
			}
		}
		// 鉴权必须在 decodeMap 之前完成，未鉴权请求不读 body。
		if !constantTimeStringEqual(secret, secretConfigured) {
			failWithCode(w, http.StatusForbidden, ErrUnauthorized, "Webhook 密钥无效")
			return
		}
		if !a.cfg().BangumiWebhookAllowLegacyToken {
			failWithCode(w, http.StatusUnauthorized, ErrUnauthorized, "Webhook 需要签名（"+bangumiWebhookSignatureHeader+"），共享 token 模式已关闭")
			return
		}
		zap.L().Warn(
			"bangumi webhook 仍在使用共享 token 鉴权（兼容期）；请改用 X-Twilight-Bangumi-Timestamp + X-Twilight-Bangumi-Signature 签名，并将 BangumiSync.webhook_allow_legacy_token 设为 false",
			zap.String("remote", r.RemoteAddr),
			zap.Bool("query_token", usingQuerySecret),
		)
		if tsHeader := strings.TrimSpace(r.Header.Get(bangumiWebhookTimestampHeader)); tsHeader != "" {
			ts, valid, fresh := parseBangumiWebhookTimestamp(tsHeader)
			if !valid {
				failWithCode(w, http.StatusBadRequest, ErrUnauthorized, "Webhook timestamp 非法")
				return
			}
			if !fresh {
				failWithCode(w, http.StatusGone, ErrUnauthorized, "Webhook 请求已过期")
				return
			}
			headerPlayedAt = ts
		}
	}
	payload := decodeMap(r)
	item, _ := payload["Item"].(map[string]any)
	// Jellyfin's official webhook plugin sends item fields at the top level.
	if item == nil && asString(payload["ItemId"]) != "" {
		item = map[string]any{"Id": payload["ItemId"], "Name": payload["Name"], "Type": payload["ItemType"], "SeriesName": payload["SeriesName"], "IndexNumber": payload["EpisodeNumber"], "ParentIndexNumber": payload["SeasonNumber"], "RunTimeTicks": payload["RunTimeTicks"]}
	}
	eventName := strings.ToLower(firstNonEmpty(asString(payload["Event"]), asString(payload["NotificationType"]), asString(payload["Name"])))
	if item != nil && (strings.Contains(eventName, "stop") || strings.Contains(eventName, "played") || payload["PlaybackPositionTicks"] != nil) {
		userID := firstNonEmpty(asString(payload["UserId"]), asString(payload["UserID"]))
		if userID == "" {
			if userData, ok := payload["User"].(map[string]any); ok {
				userID = firstNonEmpty(asString(userData["Id"]), asString(userData["ID"]))
			}
		}
		if userID == "" {
			if sessionData, ok := payload["Session"].(map[string]any); ok {
				userID = firstNonEmpty(asString(sessionData["UserId"]), asString(sessionData["UserID"]))
			}
		}
		if local, okUser := a.store().FindUserByEmbyID(userID); okUser {
			duration := numeric(payload["PlaybackPositionTicks"]) / 10000000
			if duration < 0 {
				duration = 0
			}
			// PlayedAt 优先用 header 时间戳：同一份字节重放总是命中相同 PlayedAt，
			// store 层的 (uid, item_id, played_at) 唯一键保证去重；只有缺 header
			// 的兼容路径才回落到 time.Now()，那条路径在 SECRET 已被合法持有时
			// 才会进入，重放风险在这里能容忍。
			playedAt := headerPlayedAt
			if playedAt == 0 {
				playedAt = time.Now().Unix()
			}
			// 走幂等版：即便攻击者绕过了 timestamp window 在同一秒内重放同一条
			// 合法请求，store 层的 (uid, item_id, played_at) 三元组检查会让第二
			// 次以后的写入直接静默丢弃，不会让 PlaybackRecords 无限堆积。
			inserted, err := a.store().AddPlaybackRecordIdempotent(store.PlaybackRecord{
				UID:         local.UID,
				ItemID:      firstNonEmpty(asString(item["Id"]), asString(item["ID"])),
				Title:       firstNonEmpty(asString(item["Name"]), asString(item["SeriesName"])),
				SeriesName:  asString(item["SeriesName"]),
				MediaType:   asString(item["Type"]),
				IndexNumber: int(intValue(item, "IndexNumber", 0)),
				Duration:    duration,
				PlayedAt:    playedAt,
			})
			if err != nil {
				failWithCode(w, http.StatusServiceUnavailable, ErrInternal, "观看记录保存失败")
				return
			} else if !inserted {
				zap.L().Info(
					"bangumi webhook playback record deduplicated by idempotency key",
					zap.Int64("uid", local.UID),
					zap.String("item_id", firstNonEmpty(asString(item["Id"]), asString(item["ID"]))),
				)
			}
			runtime := numeric(item["RunTimeTicks"])
			position := numeric(payload["PlaybackPositionTicks"])
			threshold := a.cfg().BangumiMinProgressPercent
			if threshold < 1 || threshold > 100 {
				threshold = 85
			}
			completed := runtime > 0 && float64(position) >= float64(runtime)*float64(threshold)/100
			if played, ok := payload["PlayedToCompletion"].(bool); ok && played {
				completed = true
			}
			rec := store.PlaybackRecord{ItemID: firstNonEmpty(asString(item["Id"]), asString(item["ID"])), SeriesName: asString(item["SeriesName"]), MediaType: asString(item["Type"]), IndexNumber: int(intValue(item, "IndexNumber", 0))}
			if rec.ItemID != "" {
				err := a.store().UpdateBangumiWatch(local.UID, store.BangumiRecordKey(rec), "", func(v *store.BangumiWatchRecord) {
					v.Completed = v.Completed || completed
					v.Season = int(intValue(item, "ParentIndexNumber", 0))
				})
				if err != nil {
					failWithCode(w, http.StatusServiceUnavailable, ErrInternal, "观看完成状态保存失败")
					return
				}
			}
		}
	}
	ok(w, "webhook accepted", map[string]any{"accepted": true, "subject_name": stringValue(item, "SeriesName"), "episode": intValue(item, "IndexNumber", 0)})
}

// constantTimeStringEqual 在 ConstantTimeCompare 基础上消除 length-mismatch
// 提前 return 0 引入的 timing oracle：先把两侧 zero-pad 到相同长度再比对，
// 然后 AND 上长度等价位。即便攻击者通过响应时延区分"长度不同"与"长度相同
// 但内容不同"，这层补丁保证两条路径的执行时间一致。
//
// 该 helper 局限于 secret 长度 <= 1024（够用所有合理 secret），超过则视为
// 明显非法直接 false——避免攻击者用极长 string 搜出更多 timing 信号。
func constantTimeStringEqual(got, want string) bool {
	const maxSecretBytes = 1024
	if len(got) > maxSecretBytes || len(want) > maxSecretBytes {
		return false
	}
	maxLen := len(got)
	if len(want) > maxLen {
		maxLen = len(want)
	}
	gotBuf := make([]byte, maxLen)
	wantBuf := make([]byte, maxLen)
	copy(gotBuf, got)
	copy(wantBuf, want)
	cmp := subtle.ConstantTimeCompare(gotBuf, wantBuf)
	lenEq := subtle.ConstantTimeEq(int32(len(got)), int32(len(want)))
	return cmp&lenEq == 1
}
