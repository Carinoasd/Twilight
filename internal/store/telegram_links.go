package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Telegram 绑定链接（deep link）
//
// 一条链接对应一次绑定尝试：网页签发 → 用户在 Telegram 里打开
// t.me/<bot>?start=<token>（或手动发送 token）→ Bot 确认 → 网页轮询到终态。
//
// 表里只有摘要，不存明文 token / secret：
//   - start token：随 deep link 交给 Telegram，Bot 收到后用它定位链接；
//   - browser secret：只有注册场景才有，签发时随响应交给浏览器，浏览器在
//     状态查询与提交注册时带回，用来证明“就是签发链接的那个浏览器”。
//     已登录场景不需要 secret，链接直接归属 UID。
//
// 状态机：pending → confirmed → consumed；重新签发会把同一所有者仍在
// pending/confirmed 的旧链接置为 cancelled。已登录场景在 Bot 确认时直接写入
// 绑定并进入 consumed；注册场景在账号创建事务里进入 consumed。
//
// API 与 Bot 进程都直接读写这张表，不需要跨进程通知：API 每个请求前会刷新
// 主状态，链接行本身每次都从数据库读取。

const (
	TelegramLinkLimit = 10000
	TelegramLinkTTL   = 10 * time.Minute
)

var (
	ErrTelegramLinkOwner    = errors.New("telegram link owner mismatch")
	ErrTelegramLinkCapacity = errors.New("telegram link capacity reached")
)

type TelegramLink struct {
	ID                  string
	StartHash           string
	SecretHash          string
	Scene               string
	UID                 int64
	State               string
	TelegramID          int64
	TelegramUsername    string
	CreatedAt           int64
	ExpiresAt           int64
	ConsumedAt          int64
	ResultUID           int64
	ErrorCode           string
	ErrorMessage        string
	Retryable           bool
	ExpectedRebindSince int64
	// CurrentTelegramID 是已登录场景下账号当前绑定的 Telegram ID 投影，
	// 与链接行在同一条语句里读出，用于判断确认结果是否仍与账号一致。
	CurrentTelegramID *int64
}

func (l TelegramLink) Confirmed() bool {
	return l.State == "confirmed" || l.State == "consumed"
}

func (l TelegramLink) Expired(now int64) bool {
	return l.ExpiresAt <= now
}

// OwnedBy 判断调用方是否有权观察 / 消费这条链接。已登录场景比对 UID；注册
// 场景比对浏览器 secret 摘要。资源 ID 本身不是凭据。
func (l TelegramLink) OwnedBy(uid int64, secretHash string) bool {
	if l.Scene == "user" {
		return uid > 0 && l.UID == uid
	}
	return uid == 0 && secretHash != "" && l.SecretHash != "" && subtle.ConstantTimeCompare([]byte(l.SecretHash), []byte(secretHash)) == 1
}

func NormalizeTelegramLinkToken(token string) string {
	return strings.ToLower(strings.TrimSpace(token))
}

func TelegramLinkStartHash(token string) string {
	sum := sha256.Sum256([]byte("twilight:telegram-link:start:v1:" + NormalizeTelegramLinkToken(token)))
	return hex.EncodeToString(sum[:])
}

func TelegramLinkSecretHash(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("twilight:telegram-link:secret:v1:" + secret))
	return hex.EncodeToString(sum[:])
}

func prepareTelegramLinkSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// API / Bot / Scheduler 可能同时启动，DDL 串行化。
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(724193821)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS twilight_telegram_links (
 id text PRIMARY KEY,
 start_hash text NOT NULL UNIQUE,
 secret_hash text NOT NULL DEFAULT '',
 scene text NOT NULL CHECK (scene IN ('register', 'user')),
 uid bigint NOT NULL DEFAULT 0,
 state text NOT NULL CHECK (state IN ('pending', 'confirmed', 'consumed', 'cancelled')),
 telegram_id bigint NOT NULL DEFAULT 0,
 telegram_username text NOT NULL DEFAULT '',
 created_at bigint NOT NULL,
 expires_at bigint NOT NULL,
 consumed_at bigint NOT NULL DEFAULT 0,
 result_uid bigint NOT NULL DEFAULT 0,
 error_code text NOT NULL DEFAULT '',
 error_message text NOT NULL DEFAULT '',
 retryable boolean NOT NULL DEFAULT false,
 expected_rebind_since bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS twilight_telegram_links_expiry_idx ON twilight_telegram_links (expires_at);
CREATE INDEX IF NOT EXISTS twilight_telegram_links_uid_idx ON twilight_telegram_links (uid) WHERE uid <> 0;
CREATE INDEX IF NOT EXISTS twilight_telegram_links_secret_idx ON twilight_telegram_links (secret_hash) WHERE secret_hash <> '';
-- 上一代绑定码挑战表：短期认证数据，升级后直接丢弃，旧码需重新生成。
DROP TABLE IF EXISTS twilight_telegram_challenges;
DROP TABLE IF EXISTS twilight_telegram_challenge_schema;
`); err != nil {
		return err
	}
	return tx.Commit()
}

const telegramLinkColumns = `id, start_hash, secret_hash, scene, uid, state, telegram_id, telegram_username,
 created_at, expires_at, consumed_at, result_uid, error_code, error_message, retryable, expected_rebind_since`

type telegramLinkScanner interface{ Scan(...any) error }

func scanTelegramLink(row telegramLinkScanner, extra ...any) (TelegramLink, error) {
	var l TelegramLink
	dest := []any{&l.ID, &l.StartHash, &l.SecretHash, &l.Scene, &l.UID, &l.State, &l.TelegramID, &l.TelegramUsername,
		&l.CreatedAt, &l.ExpiresAt, &l.ConsumedAt, &l.ResultUID, &l.ErrorCode, &l.ErrorMessage, &l.Retryable, &l.ExpectedRebindSince}
	err := row.Scan(append(dest, extra...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// 与主状态行共享同一事务：锁顺序固定为 进程 Store 锁 → 状态行 → 链接行。
// fn 内不得做网络调用，也不得调用其它会自行提交的 Store 方法。
func (s *Store) withTelegramLinkState(ctx context.Context, fn func(context.Context, *sql.Tx) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.refreshInTxLocked(ctx, tx); err != nil {
		return err
	}
	before, err := s.snapshotStateLocked()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			s.restoreStateLocked(before)
		}
	}()
	changed, err := fn(ctx, tx)
	if err != nil {
		return err
	}
	var saved []byte
	var savedVersion int64
	if changed {
		if saved, savedVersion, err = s.saveStateInTxLocked(ctx, tx, false); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if changed {
		s.stateRaw, s.stateVersion = saved, savedVersion
	}
	committed = true
	return nil
}

// refreshInTxLocked 在事务内用 FOR UPDATE 读取主状态行，版本未变时不传回 JSONB。
func (s *Store) refreshInTxLocked(ctx context.Context, tx *sql.Tx) error {
	var raw []byte
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN version=$1 THEN NULL ELSE state END, version FROM twilight_state WHERE id=1 FOR UPDATE`, s.stateVersion).Scan(&raw, &version); err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	state.ensure()
	s.state, s.stateRaw, s.stateVersion = state, raw, version
	s.rebuildUserIndexes()
	return nil
}

func validTelegramLinkHex(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size
}

// CreateTelegramLink 签发一条 pending 链接。已登录场景要求账号存在、启用且尚未绑定。
func (s *Store) CreateTelegramLink(ctx context.Context, l TelegramLink) error {
	l.ID = strings.ToLower(strings.TrimSpace(l.ID))
	now := time.Now().Unix()
	if !validTelegramLinkHex(l.ID, 16) || !validTelegramLinkHex(l.StartHash, 32) || l.State != "" || l.TelegramID != 0 ||
		l.CreatedAt <= 0 || l.CreatedAt > now+60 || l.ExpiresAt <= now || l.ExpiresAt-l.CreatedAt > int64(TelegramLinkTTL/time.Second) {
		return ErrInvalid
	}
	switch l.Scene {
	case "register":
		if l.UID != 0 || !validTelegramLinkHex(l.SecretHash, 32) {
			return ErrInvalid
		}
	case "user":
		if l.UID <= 0 || l.SecretHash != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		if l.Scene == "user" {
			u, ok := s.state.Users[l.UID]
			if !ok {
				return false, ErrNotFound
			}
			if !u.Active {
				return false, ErrInvalid
			}
			if u.TelegramID != 0 {
				return false, ErrTelegramAlreadyBound
			}
			l.ExpectedRebindSince = u.RebindingSince
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM twilight_telegram_links WHERE expires_at <= $1`, now); err != nil {
			return false, err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM twilight_telegram_links`).Scan(&count); err != nil {
			return false, err
		}
		if count >= TelegramLinkLimit {
			return false, ErrTelegramLinkCapacity
		}
		// 同一所有者只保留最新一条活动链接；已 consumed 的结果不动。
		if _, err := tx.ExecContext(ctx, `UPDATE twilight_telegram_links SET state='cancelled'
 WHERE state IN ('pending','confirmed') AND scene=$1 AND (($2::bigint>0 AND uid=$2) OR ($3<>'' AND secret_hash=$3))`, l.Scene, l.UID, l.SecretHash); err != nil {
			return false, err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO twilight_telegram_links
 (id,start_hash,secret_hash,scene,uid,state,created_at,expires_at,expected_rebind_since)
 VALUES ($1,$2,$3,$4,$5,'pending',$6,$7,$8)`, l.ID, l.StartHash, l.SecretHash, l.Scene, l.UID, l.CreatedAt, l.ExpiresAt, l.ExpectedRebindSince)
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return false, ErrConflict
		}
		return false, err
	})
}

// TelegramLink 按资源 ID 读取，并顺带读出已登录场景账号当前的 telegram_id。
// 数据库故障原样返回，调用方不得把它当成“链接不存在”。
func (s *Store) TelegramLink(ctx context.Context, id string) (TelegramLink, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT `+telegramLinkColumns+`,
 CASE WHEN uid>0 THEN (SELECT (state->'users'->(uid::text)->>'telegram_id')::bigint FROM twilight_state WHERE id=1) ELSE NULL END
 FROM twilight_telegram_links WHERE id=$1`, strings.ToLower(strings.TrimSpace(id)))
	var current *int64
	l, err := scanTelegramLink(row, &current)
	l.CurrentTelegramID = current
	return l, err
}

// TelegramLinkByStartToken 供 Bot 在做外部校验前定位链接。
func (s *Store) TelegramLinkByStartToken(ctx context.Context, token string) (TelegramLink, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT `+telegramLinkColumns+` FROM twilight_telegram_links WHERE start_hash=$1`, TelegramLinkStartHash(token))
	return scanTelegramLink(row)
}

func telegramLinkForUpdate(ctx context.Context, tx *sql.Tx, column, value string) (TelegramLink, error) {
	return scanTelegramLink(tx.QueryRowContext(ctx, `SELECT `+telegramLinkColumns+` FROM twilight_telegram_links WHERE `+column+`=$1 FOR UPDATE`, value))
}

// ConfirmTelegramLink 是 Bot 侧的确认入口。
//
// 注册场景：记录 Telegram 身份，链接进入 confirmed，等待网页提交注册时消费。
// 已登录场景：在同一事务里写入账号绑定与身份历史，链接直接 consumed。
// 同一 Telegram 重复确认是幂等的；另一个 Telegram 不能覆盖已确认的身份。
func (s *Store) ConfirmTelegramLink(ctx context.Context, token string, telegramID int64, username string) (TelegramLink, User, bool, error) {
	var result TelegramLink
	var user User
	var bound bool
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	err := s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		l, err := telegramLinkForUpdate(ctx, tx, "start_hash", TelegramLinkStartHash(token))
		if err != nil {
			return false, err
		}
		now := time.Now().Unix()
		if l.Expired(now) {
			return false, ErrExpired
		}
		if l.State == "cancelled" || telegramID <= 0 {
			return false, ErrConflict
		}
		if l.Confirmed() {
			if l.TelegramID != telegramID {
				return false, ErrConflict
			}
			// 幂等重放：确认结果必须仍与账号现状一致。
			switch {
			case l.Scene == "user":
				u, ok := s.state.Users[l.UID]
				if !ok || !u.Active || u.TelegramID != telegramID {
					return false, ErrConflict
				}
			case l.State == "consumed":
				u, ok := s.state.Users[l.ResultUID]
				if !ok || !u.Active || u.TelegramID != telegramID {
					return false, ErrConflict
				}
			default:
				if s.telegramIDTakenLocked(telegramID, 0) {
					return false, ErrConflict
				}
			}
			result = l
			return false, nil
		}
		if s.telegramIDTakenLocked(telegramID, l.UID) {
			return false, ErrConflict
		}
		l.TelegramID, l.TelegramUsername, l.State = telegramID, username, "confirmed"
		if l.Scene == "user" {
			u, ok := s.state.Users[l.UID]
			if !ok {
				return false, ErrNotFound
			}
			if u.TelegramID != 0 {
				return false, ErrTelegramAlreadyBound
			}
			if !u.Active || u.RebindingSince != l.ExpectedRebindSince {
				return false, ErrConflict
			}
			previous := u
			u.TelegramID, u.TelegramUsername = telegramID, username
			s.state.Users[u.UID] = u
			s.maintainUserIndexes(previous, u, u.UID)
			user, bound = u, true
			l.State, l.ConsumedAt, l.ResultUID = "consumed", now, u.UID
			if err = recordTelegramIdentity(ctx, tx, u.UID, telegramID, username, "bind"); err != nil {
				return false, err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE twilight_telegram_links SET state=$2, telegram_id=$3, telegram_username=$4,
 consumed_at=$5, result_uid=$6, error_code='', error_message='', retryable=false WHERE id=$1`, l.ID, l.State, l.TelegramID, l.TelegramUsername, l.ConsumedAt, l.ResultUID)
		result = l
		return bound, err
	})
	if err != nil {
		return TelegramLink{}, User{}, false, err
	}
	return result, user, bound, nil
}

// SetTelegramLinkFailure 记录一次可重试的外部校验失败（加群校验、上游错误）。
// 只写 pending 行：并发的成功确认不会被慢的失败结果覆盖。
func (s *Store) SetTelegramLinkFailure(ctx context.Context, id, code, message string, retryable bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE twilight_telegram_links SET error_code=$2, error_message=$3, retryable=$4 WHERE id=$1 AND state='pending'`, id, code, message, retryable)
	return err
}

// RegisterWithTelegramLink 在一个事务里完成：所有者校验、账号创建、注册码
// 权益、身份历史、链接消费。创建失败时链接保持 confirmed，可在有效期内重试。
func (s *Store) RegisterWithTelegramLink(ctx context.Context, u User, regCode, id, secretHash string, fn func(*User, RegCode, BindCode) error) (User, RegCode, BindCode, error) {
	var created User
	var consumed RegCode
	var bind BindCode
	err := s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		l, err := telegramLinkForUpdate(ctx, tx, "id", strings.ToLower(strings.TrimSpace(id)))
		if err != nil {
			return false, err
		}
		if !l.OwnedBy(0, secretHash) {
			return false, ErrTelegramLinkOwner
		}
		now := time.Now().Unix()
		if l.Expired(now) {
			return false, ErrExpired
		}
		bind = l.BindCode()
		if l.State != "confirmed" {
			return false, ErrConflict
		}
		created, consumed, err = s.createRegistrationUserLocked(u, regCode, bind, now, fn)
		if err != nil {
			return false, err
		}
		if err = recordTelegramIdentity(ctx, tx, created.UID, l.TelegramID, l.TelegramUsername, "register"); err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE twilight_telegram_links SET state='consumed', consumed_at=$2, result_uid=$3 WHERE id=$1`, l.ID, now, created.UID)
		return true, err
	})
	if err != nil {
		return User{}, RegCode{}, bind, err
	}
	return created, consumed, bind, nil
}

// BindCode 把链接投影成注册路径使用的绑定视图。
func (l TelegramLink) BindCode() BindCode {
	return BindCode{Code: l.ID, Scene: l.Scene, UID: l.UID, Confirmed: l.Confirmed(), TelegramID: l.TelegramID, TelegramUsername: l.TelegramUsername, CreatedAt: l.CreatedAt, ExpiresAt: l.ExpiresAt}
}

func (s *Store) DeleteTelegramLink(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM twilight_telegram_links WHERE id=$1`, strings.ToLower(strings.TrimSpace(id)))
	return err
}

// CleanupTelegramLinks 删除过期链接，以及指定账号 / Telegram 身份的全部链接。
func (s *Store) CleanupTelegramLinks(ctx context.Context, now, uid, telegramID int64) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `DELETE FROM twilight_telegram_links WHERE expires_at<=$1 OR ($2::bigint>0 AND uid=$2) OR ($3::bigint>0 AND telegram_id=$3)`, now, uid, telegramID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// CleanupOrphanedTelegramLinks 删除账号已不存在的已登录场景链接。
func (s *Store) CleanupOrphanedTelegramLinks(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// 旧 SQL 用 `NOT (s.state->'users' ? l.uid::text)` 逐列探测整份 JSONB，links
	// 多时会反复解压 TOAST。改为在同一交易里：对 state 列加 FOR SHARE（挡住并发的
	// 建号/绑定写入，保证 uid 集合与删除同一时点），只读一次 users 的键到 Go 端，
	// 再以数组参数一次删除。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var locked bool
	err = tx.QueryRowContext(ctx, `SELECT true FROM twilight_state WHERE id = 1 FOR SHARE`).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		// 与旧 SQL 一致：没有 state 列时不删任何链接（不能把空集合当成「所有账号都不存在」）。
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT jsonb_object_keys(COALESCE(state->'users', '{}'::jsonb)) FROM twilight_state WHERE id = 1`)
	if err != nil {
		return 0, err
	}
	uids := make([]int64, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return 0, err
		}
		uid, err := strconv.ParseInt(key, 10, 64)
		if err != nil {
			continue
		}
		uids = append(uids, uid)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM twilight_telegram_links WHERE uid > 0 AND NOT (uid = ANY($1))`, uids)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// TelegramLinkCount 仅供测试与诊断。
func (s *Store) TelegramLinkCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM twilight_telegram_links`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count telegram links: %w", err)
	}
	return count, nil
}
