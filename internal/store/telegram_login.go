package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

const TelegramLoginTTL = 3 * time.Minute
const TelegramLoginLimit = 1000

// Login requests are transient credentials, deliberately excluded from snapshots.
type TelegramLogin struct {
	ID, StartHash, SecretHash, State     string
	DeviceID, UserAgent, Site, CheckCode string
	UID, TelegramID, ExpiresAt           int64
}

func TelegramLoginHash(kind, value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("twilight:telegram-login:v1:" + kind + ":" + value))
	return hex.EncodeToString(sum[:])
}

func prepareTelegramLoginSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(724193823)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS twilight_telegram_login_schema (id integer PRIMARY KEY CHECK (id=1), version integer NOT NULL);
INSERT INTO twilight_telegram_login_schema VALUES (1,1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS twilight_telegram_logins (
 id text PRIMARY KEY, start_hash text NOT NULL UNIQUE, secret_hash text NOT NULL,
 state text NOT NULL CHECK (state IN ('pending','scanned','approved','rejected','cancelled','consumed')),
 device_id text NOT NULL, user_agent text NOT NULL, site text NOT NULL, check_code text NOT NULL,
 uid bigint NOT NULL DEFAULT 0, telegram_id bigint NOT NULL DEFAULT 0, expires_at bigint NOT NULL
);
ALTER TABLE twilight_telegram_logins ADD COLUMN IF NOT EXISTS device_key text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS twilight_telegram_logins_expiry ON twilight_telegram_logins(expires_at);
CREATE INDEX IF NOT EXISTS twilight_telegram_logins_active ON twilight_telegram_logins(uid) WHERE uid>0 AND state IN ('scanned','approved','consumed');
-- All state writers (including independent Bot/Scheduler processes) invalidate
-- challenges in the same transaction as security changes. Reverting a change
-- cannot revive a previously cancelled request.
CREATE OR REPLACE FUNCTION twilight_invalidate_telegram_logins() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 old_users jsonb; new_users jsonb; old_devices jsonb; new_devices jsonb;
BEGIN
 IF NOT EXISTS (SELECT 1 FROM twilight_telegram_logins WHERE uid>0 AND state IN ('scanned','approved','consumed')) THEN
  RETURN NEW;
 END IF;
 -- Detoast the shared state once, rather than once per field per request.
 old_users := OLD.state->'users'; new_users := NEW.state->'users';
 old_devices := OLD.state->'devices'; new_devices := NEW.state->'devices';
 UPDATE twilight_telegram_logins l SET state='cancelled'
 WHERE l.uid>0 AND l.state IN ('scanned','approved','consumed') AND
 (ARRAY[old_users->l.uid::text->'telegram_id', old_users->l.uid::text->'password_hash',
 old_users->l.uid::text->'active', old_users->l.uid::text->'role',
 old_users->l.uid::text->'rebinding_in_progress'] IS DISTINCT FROM
 ARRAY[new_users->l.uid::text->'telegram_id', new_users->l.uid::text->'password_hash',
 new_users->l.uid::text->'active', new_users->l.uid::text->'role',
 new_users->l.uid::text->'rebinding_in_progress'] OR
 COALESCE(old_devices->l.device_key->'is_blocked','false'::jsonb) IS DISTINCT FROM
 COALESCE(new_devices->l.device_key->'is_blocked','false'::jsonb));
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS twilight_telegram_login_invalidation ON twilight_state;
CREATE TRIGGER twilight_telegram_login_invalidation AFTER UPDATE ON twilight_state
 FOR EACH ROW EXECUTE FUNCTION twilight_invalidate_telegram_logins();
`); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT version FROM twilight_telegram_login_schema WHERE id=1`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return errors.New("unsupported telegram login schema version")
	}
	return tx.Commit()
}

const telegramLoginColumns = `id,start_hash,secret_hash,state,device_id,user_agent,site,check_code,uid,telegram_id,expires_at`

func scanTelegramLogin(row telegramLinkScanner) (TelegramLogin, error) {
	var l TelegramLogin
	err := row.Scan(&l.ID, &l.StartHash, &l.SecretHash, &l.State, &l.DeviceID, &l.UserAgent, &l.Site, &l.CheckCode, &l.UID, &l.TelegramID, &l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return l, err
}

func (s *Store) CreateTelegramLogin(ctx context.Context, l TelegramLogin) error {
	if l.ID == "" || l.StartHash == "" || l.SecretHash == "" || l.ExpiresAt <= time.Now().Unix() || l.ExpiresAt > time.Now().Add(TelegramLoginTTL).Unix() {
		return ErrInvalid
	}
	return s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM twilight_telegram_logins WHERE expires_at<=$1`, time.Now().Unix()); err != nil {
			return false, err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM twilight_telegram_logins`).Scan(&count); err != nil {
			return false, err
		}
		if count >= TelegramLoginLimit {
			return false, ErrTelegramLinkCapacity
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO twilight_telegram_logins (`+telegramLoginColumns+`) VALUES ($1,$2,$3,'pending',$4,$5,$6,$7,0,0,$8)`, l.ID, l.StartHash, l.SecretHash, l.DeviceID, l.UserAgent, l.Site, l.CheckCode, l.ExpiresAt)
		return false, err
	})
}

func (s *Store) TelegramLogin(ctx context.Context, id, secret string) (TelegramLogin, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	l, err := scanTelegramLogin(s.db.QueryRowContext(ctx, `SELECT `+telegramLoginColumns+` FROM twilight_telegram_logins WHERE id=$1`, id))
	if err != nil {
		return l, err
	}
	if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(l.SecretHash)) != 1 {
		return TelegramLogin{}, ErrTelegramLinkOwner
	}
	if l.ExpiresAt <= time.Now().Unix() {
		l.State = "expired"
	}
	return l, nil
}

// Scan claims a request for one Telegram identity; it never approves login.
func (s *Store) ScanTelegramLogin(ctx context.Context, token string, telegramID int64) (TelegramLogin, error) {
	var result TelegramLogin
	err := s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		l, err := scanTelegramLogin(tx.QueryRowContext(ctx, `SELECT `+telegramLoginColumns+` FROM twilight_telegram_logins WHERE start_hash=$1 FOR UPDATE`, TelegramLoginHash("start", token)))
		if err != nil {
			return false, err
		}
		if telegramID <= 0 || l.ExpiresAt <= time.Now().Unix() {
			return false, ErrConflict
		}
		if l.State == "scanned" && l.TelegramID == telegramID {
			result = l
			return false, nil
		}
		if l.State != "pending" {
			return false, ErrConflict
		}
		user := s.state.Users[s.telegramIDMap[telegramID]]
		if user.UID == 0 || user.TelegramID != telegramID || !user.Active || user.RebindingInProgress {
			return false, ErrConflict
		}
		l.State, l.UID, l.TelegramID = "scanned", user.UID, telegramID
		_, err = tx.ExecContext(ctx, `UPDATE twilight_telegram_logins SET state='scanned',uid=$2,telegram_id=$3,device_key=$4 WHERE id=$1`, l.ID, l.UID, l.TelegramID, deviceKey(l.UID, l.DeviceID))
		result = l
		return false, err
	})
	return result, err
}

func (s *Store) DecideTelegramLogin(ctx context.Context, id string, telegramID int64, approve bool) error {
	return s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		l, err := scanTelegramLogin(tx.QueryRowContext(ctx, `SELECT `+telegramLoginColumns+` FROM twilight_telegram_logins WHERE id=$1 FOR UPDATE`, id))
		if err != nil {
			return false, err
		}
		u, exists := s.state.Users[l.UID]
		if l.ExpiresAt <= time.Now().Unix() || l.State != "scanned" || telegramID <= 0 || l.TelegramID != telegramID || !exists || !u.Active || u.RebindingInProgress || u.TelegramID != telegramID {
			return false, ErrConflict
		}
		state := "rejected"
		if approve {
			state = "approved"
		}
		_, err = tx.ExecContext(ctx, `UPDATE twilight_telegram_logins SET state=$2 WHERE id=$1`, id, state)
		return false, err
	})
}

func (s *Store) ConsumeTelegramLogin(ctx context.Context, id, secret, deviceID string) (User, error) {
	var user User
	err := s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		l, err := scanTelegramLogin(tx.QueryRowContext(ctx, `SELECT `+telegramLoginColumns+` FROM twilight_telegram_logins WHERE id=$1 FOR UPDATE`, id))
		if err != nil {
			return false, err
		}
		if secret == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(l.SecretHash)) != 1 {
			return false, ErrTelegramLinkOwner
		}
		u, exists := s.state.Users[l.UID]
		if l.State != "approved" || l.ExpiresAt <= time.Now().Unix() || l.DeviceID != deviceID || !exists || !u.Active || u.RebindingInProgress || u.TelegramID != l.TelegramID {
			return false, ErrConflict
		}
		_, err = tx.ExecContext(ctx, `UPDATE twilight_telegram_logins SET state='consumed' WHERE id=$1`, id)
		user = u
		return false, err
	})
	return user, err
}

func (s *Store) CancelTelegramLogin(ctx context.Context, id, secret string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `UPDATE twilight_telegram_logins SET state='cancelled' WHERE id=$1 AND secret_hash=$2 AND state IN ('pending','scanned','approved')`, id, secret)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return ErrConflict
	}
	return err
}

func (s *Store) CleanupTelegramLogins(ctx context.Context, now int64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM twilight_telegram_logins WHERE expires_at<=$1`, now)
	return err
}

func (s *Store) RevokeTelegramLogins(ctx context.Context, uid int64) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE twilight_telegram_logins SET state='cancelled' WHERE uid=$1 AND state IN ('scanned','approved','consumed')`, uid)
	return err
}
