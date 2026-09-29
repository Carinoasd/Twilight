package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/prejudice-studio/twilight/internal/redis"
	"github.com/prejudice-studio/twilight/internal/security"
	"github.com/prejudice-studio/twilight/internal/store"
)

const maxSessionTokenCreateAttempts = 8

// sessionDigestPrefix 标记已摘要化的会话键。旧版本把明文 token 直接当作 PG 主键与
// Redis key；明文与摘要都是 64 位 hex，需要前缀才能区分并做一次性迁移。
const sessionDigestPrefix = "sha256:"

// sessionTokenDigest 返回会话 token 的存储键。
//
// 服务端只保存 token 的 SHA-256 摘要（PG twilight_sessions.token、Redis key、内存表），
// 客户端持有的明文 token 不落库：数据库备份或只读账号外泄时拿不到可直接使用的
// Bearer token（与 API Key、邮箱验证码只存哈希的口径一致）。token 本身是 32 字节
// crypto/rand，无需加盐或慢哈希。
func sessionTokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return sessionDigestPrefix + hex.EncodeToString(sum[:])
}

type sessionStore struct {
	mu     sync.RWMutex
	items  map[string]sessionRecord
	ttl    time.Duration
	redis  *redis.Client
	st     *store.Store
	prefix string

	// redis 写 / 读失败导致路径退到 memory + postgres 时累加，便于运维通过
	// /system/stats 观察是否进入降级。值持续增长意味着 redis 不可用，登录会
	// 话存活仍由 postgres 兜底，但 ttl 失效不再实时同步多副本。
	fallbackCount atomic.Int64
}

type sessionRecord struct {
	UID       int64 `json:"uid"`
	ExpiresAt int64 `json:"expires_at"`
	// DeviceID 是签发会话时的设备（与 store.Device.DeviceID 一致）。管理员封禁设备、
	// 设备数上限淘汰、用户删除设备时，按 (uid, device_id) 吊销对应会话。
	DeviceID string `json:"device_id,omitempty"`
}

func newSessionStore(ttl time.Duration, redisClient *redis.Client) *sessionStore {
	return &sessionStore{items: map[string]sessionRecord{}, ttl: ttl, redis: redisClient, prefix: "twilight:session:"}
}

func newSessionStoreWithDB(ttl time.Duration, redisClient *redis.Client, st *store.Store) *sessionStore {
	ss := &sessionStore{items: map[string]sessionRecord{}, ttl: ttl, redis: redisClient, st: st, prefix: "twilight:session:"}
	if db := st.DB(); db != nil {
		ss.restoreFromPostgres(db)
	}
	return ss
}

// restoreFromPostgres loads valid sessions from PostgreSQL.
// When Redis is available, it re-populates Redis with any sessions that survived
// a Redis restart. When Redis is unavailable, sessions are loaded into memory.
//
// 启动期没有 caller ctx，这里用显式 30s WithTimeout 兜底，避免 PG 慢响应让
// newSessionStoreWithDB 阻塞整个启动流程；之前裸 context.Background() 一旦
// PG 卡住会让 App.New 卡死。
func (s *sessionStore) restoreFromPostgres(db *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	now := time.Now().Unix()
	// Purge expired sessions
	_, _ = db.ExecContext(ctx, `DELETE FROM twilight_sessions WHERE expires_at <= $1`, now)
	s.migrateLegacyTokens(ctx, db)

	rows, err := db.QueryContext(ctx, `SELECT token, uid, expires_at, device_id FROM twilight_sessions WHERE expires_at > $1`, now)
	if err != nil {
		zap.L().Warn("failed to load sessions from PostgreSQL", zap.Error(err))
		return
	}
	defer rows.Close()

	restored := 0
	restoredToRedis := 0
	for rows.Next() {
		var token string
		var record sessionRecord
		if err := rows.Scan(&token, &record.UID, &record.ExpiresAt, &record.DeviceID); err != nil {
			continue
		}
		restored++

		// If Redis is available, push sessions back into Redis (handles Redis restart)
		if s.redis != nil {
			remainTTL := record.ExpiresAt - now
			if remainTTL > 0 {
				payload, _ := json.Marshal(record)
				if err := s.redis.SetEX(ctx, s.prefix+token, int(remainTTL), string(payload)); err == nil {
					restoredToRedis++
					continue
				}
			}
		}
		// Fallback: load into memory
		s.mu.Lock()
		s.items[token] = record
		s.mu.Unlock()
	}
	if restored > 0 {
		zap.L().Info("restored sessions from PostgreSQL",
			zap.Int("total", restored),
			zap.Int("to_redis", restoredToRedis),
			zap.Int("to_memory", restored-restoredToRedis))
	}
}

// migrateLegacyTokens 把旧版本以明文 token 为主键的会话一次性改写为摘要键，并删除
// Redis 里对应的明文 key（随后 restore 循环会以摘要 key 回填）。现有登录不受影响：
// 客户端仍持有同一个明文 token，查找时按摘要命中。幂等：只处理没有摘要前缀的行。
func (s *sessionStore) migrateLegacyTokens(ctx context.Context, db *sql.DB) {
	rows, err := db.QueryContext(ctx, `SELECT token FROM twilight_sessions WHERE token NOT LIKE 'sha256:%'`)
	if err != nil {
		zap.L().Warn("failed to scan legacy plaintext sessions", zap.Error(err))
		return
	}
	var legacy []string
	for rows.Next() {
		var token string
		if rows.Scan(&token) == nil {
			legacy = append(legacy, token)
		}
	}
	rows.Close()
	migrated := 0
	for _, token := range legacy {
		if _, err := db.ExecContext(ctx, `UPDATE twilight_sessions SET token = $1 WHERE token = $2`, sessionTokenDigest(token), token); err != nil {
			zap.L().Warn("failed to migrate legacy plaintext session", zap.Error(err))
			continue
		}
		if s.redis != nil {
			_ = s.redis.Del(ctx, s.prefix+token)
		}
		migrated++
	}
	if migrated > 0 {
		zap.L().Info("migrated plaintext session tokens to SHA-256 digests", zap.Int("count", migrated))
	}
}

func (s *sessionStore) pgDB() *sql.DB {
	if s.st == nil {
		return nil
	}
	return s.st.DB()
}

func (s *sessionStore) Create(ctx context.Context, uid int64, deviceID string) (string, time.Time, error) {
	for attempt := 0; attempt < maxSessionTokenCreateAttempts; attempt++ {
		token, err := security.RandomHex(32)
		if err != nil {
			return "", time.Time{}, err
		}
		expires := time.Now().Add(s.ttl)
		record := sessionRecord{UID: uid, ExpiresAt: expires.Unix(), DeviceID: deviceID}
		key := sessionTokenDigest(token)

		inserted, err := s.persistToPostgres(ctx, key, record)
		if err != nil {
			// PG 写入失败就让创建失败：旧实现照样返回 token，会话只存在于 Redis /
			// 内存，而 DeleteUser 只能从内存表和 PG 的 RETURNING 收集要删的 Redis key，
			// 这类会话在改密 / 登出全部后仍在整个 TTL 内有效。
			return "", time.Time{}, fmt.Errorf("persist session: %w", err)
		}
		if !inserted {
			zap.L().Warn("session token collision in PostgreSQL; retrying")
			continue
		}

		redisOK := false
		if s.redis != nil {
			payload, _ := json.Marshal(record)
			ok, err := s.redis.SetEXNX(ctx, s.prefix+key, int(s.ttl/time.Second), string(payload))
			if err == nil && ok {
				redisOK = true
			} else if err == nil {
				s.deletePostgresToken(ctx, key)
				zap.L().Warn("session token collision in Redis; retrying")
				continue
			} else {
				s.fallbackCount.Add(1)
				zap.L().Warn("redis session create failed; using memory+pg fallback", zap.Error(err))
			}
		}

		if !redisOK {
			// Memory fallback when Redis is unavailable. Check again under the
			// write lock so concurrent fallback writers cannot reuse a token.
			s.mu.Lock()
			if _, exists := s.items[key]; exists {
				s.mu.Unlock()
				s.deletePostgresToken(ctx, key)
				zap.L().Warn("session token collision in memory fallback; retrying")
				continue
			}
			s.items[key] = record
			s.mu.Unlock()
		}

		return token, expires, nil
	}
	return "", time.Time{}, errors.New("failed to create unique session token")
}

// persistToPostgres 以摘要键 key 写入会话。返回 inserted=false 表示键冲突（需重试），
// err 非空表示写入失败。
func (s *sessionStore) persistToPostgres(ctx context.Context, key string, record sessionRecord) (bool, error) {
	db := s.pgDB()
	if db == nil {
		return true, nil
	}
	result, err := db.ExecContext(ctx,
		`INSERT INTO twilight_sessions (token, uid, expires_at, device_id) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (token) DO NOTHING`,
		key, record.UID, record.ExpiresAt, record.DeviceID)
	if err != nil {
		zap.L().Warn("failed to persist session to PostgreSQL", zap.Error(err))
		return false, err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return false, nil
	}
	return true, nil
}

func (s *sessionStore) deletePostgresToken(ctx context.Context, key string) {
	if db := s.pgDB(); db != nil {
		_, _ = db.ExecContext(ctx, `DELETE FROM twilight_sessions WHERE token = $1`, key)
	}
}

func (s *sessionStore) Get(ctx context.Context, token string) (int64, bool) {
	record, ok := s.GetRecord(ctx, token)
	return record.UID, ok
}

// GetRecord 与 Get 相同，但返回完整会话记录（含 DeviceID），供续期 / 轮换会话时
// 沿用原设备。
func (s *sessionStore) GetRecord(ctx context.Context, token string) (sessionRecord, bool) {
	if token == "" {
		return sessionRecord{}, false
	}
	key := sessionTokenDigest(token)

	// 1. Try Redis (fastest path)
	if s.redis != nil {
		payload, ok, err := s.redis.Get(ctx, s.prefix+key)
		if err == nil && ok {
			var record sessionRecord
			if json.Unmarshal([]byte(payload), &record) == nil && record.ExpiresAt >= time.Now().Unix() {
				return record, true
			}
			// Expired in Redis - clean up
			return sessionRecord{}, false
		}
		if err != nil {
			s.fallbackCount.Add(1)
			zap.L().Warn("redis session read failed; checking fallbacks", zap.Error(err))
		}
		// Redis miss (not error) - check PostgreSQL for sessions that survived Redis restart
	}

	// 2. Try in-memory cache
	s.mu.RLock()
	record, ok := s.items[key]
	s.mu.RUnlock()
	if ok {
		if record.ExpiresAt >= time.Now().Unix() {
			return record, true
		}
		// Expired in memory - clean up lazily
		s.mu.Lock()
		delete(s.items, key)
		s.mu.Unlock()
		return sessionRecord{}, false
	}

	// 3. Try PostgreSQL (handles Redis restart scenario)
	if db := s.pgDB(); db != nil {
		var rec sessionRecord
		err := db.QueryRowContext(ctx,
			`SELECT uid, expires_at, device_id FROM twilight_sessions WHERE token = $1 AND expires_at > $2`,
			key, time.Now().Unix()).Scan(&rec.UID, &rec.ExpiresAt, &rec.DeviceID)
		if err == nil {
			// Found in PG - re-populate Redis for future fast lookups
			if s.redis != nil {
				remainTTL := rec.ExpiresAt - time.Now().Unix()
				if remainTTL > 0 {
					payload, _ := json.Marshal(rec)
					_ = s.redis.SetEX(ctx, s.prefix+key, int(remainTTL), string(payload))
				}
			}
			return rec, true
		}
	}

	return sessionRecord{}, false
}

func (s *sessionStore) Delete(ctx context.Context, token string) {
	if token == "" {
		return
	}
	key := sessionTokenDigest(token)
	// Remove from all layers
	if s.redis != nil {
		_ = s.redis.Del(ctx, s.prefix+key)
	}
	s.mu.Lock()
	delete(s.items, key)
	s.mu.Unlock()
	if db := s.pgDB(); db != nil {
		_, _ = db.ExecContext(ctx, `DELETE FROM twilight_sessions WHERE token = $1`, key)
	}
}

func (s *sessionStore) DeleteUser(ctx context.Context, uid int64) {
	// Collect tokens from memory
	s.mu.Lock()
	for token, record := range s.items {
		if record.UID == uid {
			delete(s.items, token)
			if s.redis != nil {
				_ = s.redis.Del(ctx, s.prefix+token)
			}
		}
	}
	s.mu.Unlock()

	// Also remove from PostgreSQL. Use DELETE ... RETURNING token in a single
	// statement so we collect tokens for Redis cleanup atomically — the old
	// SELECT-then-DELETE pair left a window where another connection could
	// INSERT a fresh session for this uid between the two statements: the new
	// token would either be missed by Redis cleanup (left dangling) or wiped
	// by the broad DELETE (kicking a user who just logged in).
	if db := s.pgDB(); db != nil {
		rows, err := db.QueryContext(ctx,
			`DELETE FROM twilight_sessions WHERE uid = $1 RETURNING token`, uid)
		if err == nil {
			for rows.Next() {
				var token string
				if rows.Scan(&token) == nil && s.redis != nil {
					_ = s.redis.Del(ctx, s.prefix+token)
				}
			}
			rows.Close()
		}
	}
}

// DeleteDevice 吊销某用户在某台设备上签发的全部会话（内存、Redis、PostgreSQL）。
// deviceID 为空时不做任何事：旧会话没有设备归属，不能被误当成“空设备”一并吊销。
func (s *sessionStore) DeleteDevice(ctx context.Context, uid int64, deviceID string) int {
	if deviceID == "" {
		return 0
	}
	removed := 0
	s.mu.Lock()
	for token, record := range s.items {
		if record.UID == uid && record.DeviceID == deviceID {
			delete(s.items, token)
			if s.redis != nil {
				_ = s.redis.Del(ctx, s.prefix+token)
			}
			removed++
		}
	}
	s.mu.Unlock()
	if db := s.pgDB(); db != nil {
		rows, err := db.QueryContext(ctx,
			`DELETE FROM twilight_sessions WHERE uid = $1 AND device_id = $2 RETURNING token`, uid, deviceID)
		if err == nil {
			for rows.Next() {
				var token string
				if rows.Scan(&token) == nil {
					removed++
					if s.redis != nil {
						_ = s.redis.Del(ctx, s.prefix+token)
					}
				}
			}
			rows.Close()
		}
	}
	return removed
}

// CleanupExpired removes expired sessions from all layers.
//
// 接受 caller ctx：scheduler 已提供 runCtx，被 admin 终止时能立刻取消。
// 不接 ctx 的旧版本若 PG DELETE 卡住，scheduler 永远等不到 finish()。
func (s *sessionStore) CleanupExpired(ctx context.Context) int {
	now := time.Now().Unix()
	removed := 0

	// Clean memory
	s.mu.Lock()
	for token, record := range s.items {
		if record.ExpiresAt <= now {
			delete(s.items, token)
			removed++
		}
	}
	s.mu.Unlock()

	// Clean PostgreSQL (Redis handles TTL expiry automatically)
	if db := s.pgDB(); db != nil {
		result, _ := db.ExecContext(ctx, `DELETE FROM twilight_sessions WHERE expires_at <= $1`, now)
		if result != nil {
			if n, err := result.RowsAffected(); err == nil && n > 0 {
				removed = int(n) // PG count is more accurate
			}
		}
	}
	return removed
}

// ActiveCount returns the number of active sessions across all layers.
//
// 接受 caller ctx，避免在 system_stats 等监控请求里被卡死的 PG count(*) 拖
// 累整个 handler 超时。调用方可用 r.Context() 或 WithTimeout 兜底。
func (s *sessionStore) ActiveCount(ctx context.Context) int {
	if db := s.pgDB(); db != nil {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM twilight_sessions WHERE expires_at > $1`,
			time.Now().Unix()).Scan(&count); err == nil {
			return count
		}
	}
	// Fallback to memory count
	now := time.Now().Unix()
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, record := range s.items {
		if record.ExpiresAt > now {
			count++
		}
	}
	return count
}

// FallbackCount 报告自启动以来 redis session 失败回退到内存 / postgres 的累计
// 次数。值持续增长意味着 redis 不可用：sessions 仍能通过 postgres 维持登录，
// 但跨副本 ttl 同步不再走 redis；运维应优先恢复 redis。
func (s *sessionStore) FallbackCount() int64 {
	if s == nil {
		return 0
	}
	return s.fallbackCount.Load()
}

// DeleteAll 吊销全部会话（内存、Redis、PostgreSQL）。数据库恢复 / 迁移导入会把
// NextUserID 回卷到备份时点，之后新注册的用户会拿到旧用户的 UID；会话只绑定 uid，
// 不清掉的话旧浏览器里的会话会直接以新用户身份通过鉴权。
func (s *sessionStore) DeleteAll(ctx context.Context) int {
	removed := 0
	s.mu.Lock()
	for token := range s.items {
		delete(s.items, token)
		if s.redis != nil {
			_ = s.redis.Del(ctx, s.prefix+token)
		}
		removed++
	}
	s.mu.Unlock()
	if db := s.pgDB(); db != nil {
		rows, err := db.QueryContext(ctx, `DELETE FROM twilight_sessions RETURNING token`)
		if err == nil {
			for rows.Next() {
				var token string
				if rows.Scan(&token) == nil {
					removed++
					if s.redis != nil {
						_ = s.redis.Del(ctx, s.prefix+token)
					}
				}
			}
			rows.Close()
		}
	}
	return removed
}
