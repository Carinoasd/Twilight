package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/prejudice-studio/twilight/internal/security"
)

var ErrTwoFactorInvalid = errors.New("two-factor request invalid or expired")
var ErrTwoFactorRequired = errors.New("two-factor verification required")

func (s *Store) TwoFactorLoginOwner(ctx context.Context, hash string) (int64, string, error) {
	var uid int64
	var method string
	err := s.db.QueryRowContext(ctx, `SELECT uid,method FROM twilight_two_factor_requests WHERE hash=$1 AND purpose='login' AND state='pending' AND expires_at>$2`, hash, time.Now().Unix()).Scan(&uid, &method)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrTwoFactorInvalid
	}
	return uid, method, err
}

type TwoFactorAccount struct {
	UID       int64    `json:"uid"`
	Secret    string   `json:"secret"`
	Recovery  []string `json:"recovery"`
	Version   string   `json:"version"`
	EnabledAt int64    `json:"enabled_at"`
	LastStep  int64    `json:"last_step"`
}

type TwoFactorRequest struct {
	Hash, Purpose, Secret, Version, DeviceID, DeviceKey, SessionHash, Method, TelegramRequest string
	UID, ExpiresAt                                                                            int64
}

func prepareTwoFactorSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(724193824)`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS twilight_two_factor_schema (id integer PRIMARY KEY CHECK(id=1), version integer NOT NULL);
INSERT INTO twilight_two_factor_schema VALUES(1,1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS twilight_two_factor_accounts (uid bigint PRIMARY KEY, data jsonb NOT NULL);
CREATE TABLE IF NOT EXISTS twilight_two_factor_requests (
 hash text PRIMARY KEY, uid bigint NOT NULL, purpose text NOT NULL, secret text NOT NULL DEFAULT '',
 version text NOT NULL, device_id text NOT NULL, device_key text NOT NULL, session_hash text NOT NULL,
 method text NOT NULL, telegram_request text NOT NULL DEFAULT '', expires_at bigint NOT NULL,
 attempts integer NOT NULL DEFAULT 0, state text NOT NULL DEFAULT 'pending'
);
CREATE INDEX IF NOT EXISTS twilight_two_factor_requests_uid ON twilight_two_factor_requests(uid);
CREATE INDEX IF NOT EXISTS twilight_two_factor_requests_expiry ON twilight_two_factor_requests(expires_at);
ALTER TABLE twilight_sessions ADD COLUMN IF NOT EXISTS auth_version text NOT NULL DEFAULT '';
ALTER TABLE twilight_sessions ADD COLUMN IF NOT EXISTS auth_request text NOT NULL DEFAULT '';
CREATE OR REPLACE FUNCTION twilight_invalidate_two_factor_requests() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE old_users jsonb; new_users jsonb; old_devices jsonb; new_devices jsonb;
BEGIN
 old_users := OLD.state->'users'; new_users := NEW.state->'users';
 old_devices := OLD.state->'devices'; new_devices := NEW.state->'devices';
 UPDATE twilight_two_factor_requests r SET state='revoked' WHERE r.state<>'revoked' AND
 (ARRAY[old_users->r.uid::text->'password_hash',old_users->r.uid::text->'active',old_users->r.uid::text->'role',old_users->r.uid::text->'telegram_id',old_users->r.uid::text->'rebinding_in_progress'] IS DISTINCT FROM
 ARRAY[new_users->r.uid::text->'password_hash',new_users->r.uid::text->'active',new_users->r.uid::text->'role',new_users->r.uid::text->'telegram_id',new_users->r.uid::text->'rebinding_in_progress'] OR
 COALESCE(old_devices->r.device_key->'is_blocked','false'::jsonb) IS DISTINCT FROM COALESCE(new_devices->r.device_key->'is_blocked','false'::jsonb));
 DELETE FROM twilight_sessions s USING twilight_two_factor_requests r WHERE s.auth_request=r.hash AND r.state='revoked';
 DELETE FROM twilight_two_factor_accounts a WHERE NOT (new_users ? a.uid::text);
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS twilight_two_factor_invalidation ON twilight_state;
CREATE TRIGGER twilight_two_factor_invalidation AFTER UPDATE ON twilight_state FOR EACH ROW EXECUTE FUNCTION twilight_invalidate_two_factor_requests();
`)
	if err != nil {
		return err
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT version FROM twilight_two_factor_schema WHERE id=1`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return errors.New("unsupported two-factor schema version")
	}
	return tx.Commit()
}

type twoFactorQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func twoFactorAccount(ctx context.Context, q twoFactorQuerier, uid int64) (TwoFactorAccount, error) {
	a := TwoFactorAccount{UID: uid, LastStep: -1}
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM twilight_two_factor_accounts WHERE uid=$1`, uid).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	err = json.Unmarshal(raw, &a)
	return a, err
}
func (s *Store) TwoFactorAccount(ctx context.Context, uid int64) (TwoFactorAccount, error) {
	return twoFactorAccount(ctx, s.db, uid)
}
func saveTwoFactorAccount(ctx context.Context, tx *sql.Tx, a TwoFactorAccount) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO twilight_two_factor_accounts(uid,data) VALUES($1,$2::jsonb) ON CONFLICT(uid) DO UPDATE SET data=excluded.data`, a.UID, string(b))
	return err
}

func sameLoginIdentity(a, b User) bool {
	return a.UID != 0 && a.UID == b.UID && a.Active && a.PasswordHash == b.PasswordHash && a.Role == b.Role && a.TelegramID == b.TelegramID && a.RebindingInProgress == b.RebindingInProgress
}

func insertTwoFactorRequest(ctx context.Context, tx *sql.Tx, r TwoFactorRequest) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM twilight_two_factor_requests WHERE expires_at<=$1`, time.Now().Unix())
	if err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM twilight_two_factor_requests`).Scan(&count); err != nil {
		return err
	}
	if count >= 1000 {
		return ErrTelegramLinkCapacity
	}
	// Only the latest setup/login attempt per browser is retained.
	_, err = tx.ExecContext(ctx, `DELETE FROM twilight_two_factor_requests WHERE uid=$1 AND purpose=$2 AND ($2='setup' OR device_id=$3)`, r.UID, r.Purpose, r.DeviceID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO twilight_two_factor_requests(hash,uid,purpose,secret,version,device_id,device_key,session_hash,method,telegram_request,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, r.Hash, r.UID, r.Purpose, r.Secret, r.Version, r.DeviceID, r.DeviceKey, r.SessionHash, r.Method, r.TelegramRequest, r.ExpiresAt)
	return err
}

// BeginTwoFactorLogin revalidates the primary proof under the shared state lock.
func (s *Store) BeginTwoFactorLogin(ctx context.Context, expected User, r TwoFactorRequest) (bool, error) {
	needed := false
	err := s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		u := s.state.Users[expected.UID]
		if !sameLoginIdentity(u, expected) {
			return false, ErrTwoFactorInvalid
		}
		if d, ok := s.state.Devices[deviceKey(u.UID, r.DeviceID)]; ok && d.Blocked {
			return false, ErrTwoFactorInvalid
		}
		if r.Method == "telegram" {
			var valid bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM twilight_telegram_logins WHERE id=$1 AND uid=$2 AND state='consumed' AND expires_at>$3 AND device_id=$4)`, r.TelegramRequest, u.UID, time.Now().Unix(), r.DeviceID).Scan(&valid); err != nil {
				return false, err
			}
			if !valid {
				return false, ErrTwoFactorInvalid
			}
		}
		a, err := twoFactorAccount(ctx, tx, u.UID)
		if err != nil {
			return false, err
		}
		needed = a.EnabledAt > 0
		if !needed {
			return false, nil
		}
		r.UID, r.Version, r.Purpose, r.DeviceKey = u.UID, a.Version, "login", deviceKey(u.UID, r.DeviceID)
		return false, insertTwoFactorRequest(ctx, tx, r)
	})
	return needed, err
}

func validTwoFactorSession(ctx context.Context, tx *sql.Tx, uid int64, hash, version string) bool {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM twilight_sessions WHERE token=$1 AND uid=$2 AND auth_version=$3 AND expires_at>$4)`, hash, uid, version, time.Now().Unix()).Scan(&valid)
	return err == nil && valid
}

func (s *Store) BeginTwoFactorSetup(ctx context.Context, expected User, r TwoFactorRequest) error {
	return s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		u := s.state.Users[expected.UID]
		if !sameLoginIdentity(u, expected) {
			return false, ErrTwoFactorInvalid
		}
		a, err := twoFactorAccount(ctx, tx, u.UID)
		if err != nil {
			return false, err
		}
		if a.EnabledAt > 0 || !validTwoFactorSession(ctx, tx, u.UID, r.SessionHash, a.Version) {
			return false, ErrTwoFactorInvalid
		}
		r.UID, r.Version, r.Purpose = u.UID, a.Version, "setup"
		r.DeviceKey = deviceKey(u.UID, r.DeviceID)
		return false, insertTwoFactorRequest(ctx, tx, r)
	})
}

func readTwoFactorRequest(ctx context.Context, tx *sql.Tx, hash string) (TwoFactorRequest, int, string, error) {
	var r TwoFactorRequest
	var attempts int
	var state string
	err := tx.QueryRowContext(ctx, `SELECT hash,uid,purpose,secret,version,device_id,device_key,session_hash,method,telegram_request,expires_at,attempts,state FROM twilight_two_factor_requests WHERE hash=$1 FOR UPDATE`, hash).Scan(&r.Hash, &r.UID, &r.Purpose, &r.Secret, &r.Version, &r.DeviceID, &r.DeviceKey, &r.SessionHash, &r.Method, &r.TelegramRequest, &r.ExpiresAt, &attempts, &state)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrTwoFactorInvalid
	}
	return r, attempts, state, err
}

func verifyTwoFactor(a *TwoFactorAccount, key []byte, code string, recovery bool) error {
	secret, err := security.OpenTOTP(key, a.UID, a.Secret)
	if err != nil {
		return err
	}
	if recovery {
		hash := security.RecoveryDigest(code)
		for i, v := range a.Recovery {
			if subtle.ConstantTimeCompare([]byte(hash), []byte(v)) == 1 {
				a.Recovery = append(a.Recovery[:i:i], a.Recovery[i+1:]...)
				return nil
			}
		}
		return security.ErrTwoFactorCode
	}
	step, err := security.VerifyTOTP(secret, code, time.Now(), a.LastStep)
	if err != nil {
		return err
	}
	a.LastStep = step
	return nil
}

// RevokeTwoFactorRequests also invalidates consumed grants before session creation.
func (s *Store) RevokeTwoFactorRequests(ctx context.Context, uid int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE twilight_two_factor_requests SET state='revoked' WHERE uid=$1`, uid)
	return err
}
func (s *Store) CancelTwoFactorRequest(ctx context.Context, hash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE twilight_two_factor_requests SET state='revoked' WHERE hash=$1`, hash); err != nil {
		return err
	}
	// A browser may leave while the final response is in flight. Revoke the
	// session already created by that request as well, without touching others.
	if _, err = tx.ExecContext(ctx, `DELETE FROM twilight_sessions WHERE auth_request=$1`, hash); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CancelTwoFactorSetups(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM twilight_two_factor_requests WHERE purpose='setup'`)
	return err
}
func (s *Store) CleanupTwoFactorRequests(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM twilight_two_factor_requests WHERE expires_at<=$1`, time.Now().Unix())
	return err
}

func invalidateTwoFactorUser(ctx context.Context, tx *sql.Tx, uid int64) error {
	for _, query := range []string{`UPDATE twilight_two_factor_requests SET state='revoked' WHERE uid=$1`, `UPDATE twilight_telegram_logins SET state='cancelled' WHERE uid=$1`, `DELETE FROM twilight_sessions WHERE uid=$1`} {
		if _, err := tx.ExecContext(ctx, query, uid); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EnableTwoFactor(ctx context.Context, uid int64, hash, sessionHash, code string, key []byte, hashes []string) error {
	var denied error
	err := s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		r, attempts, state, err := readTwoFactorRequest(ctx, tx, hash)
		if err != nil {
			return false, err
		}
		a, err := twoFactorAccount(ctx, tx, uid)
		if err != nil {
			return false, err
		}
		if r.UID != uid || r.Purpose != "setup" || state != "pending" || attempts >= 5 || r.ExpiresAt <= time.Now().Unix() || a.EnabledAt > 0 || r.Version != a.Version || r.SessionHash != sessionHash || !validTwoFactorSession(ctx, tx, uid, sessionHash, a.Version) {
			return false, ErrTwoFactorInvalid
		}
		a.Secret = r.Secret
		a.LastStep = -1
		if err = verifyTwoFactor(&a, key, code, false); err != nil {
			denied = err
			_, err = tx.ExecContext(ctx, `UPDATE twilight_two_factor_requests SET attempts=attempts+1 WHERE hash=$1`, hash)
			return false, err
		}
		a.Version, err = security.RandomHex(16)
		if err != nil {
			return false, err
		}
		a.EnabledAt = time.Now().Unix()
		a.Recovery = hashes
		if err = saveTwoFactorAccount(ctx, tx, a); err != nil {
			return false, err
		}
		return false, invalidateTwoFactorUser(ctx, tx, uid)
	})
	if err == nil {
		return denied
	}
	return err
}

type TwoFactorGrant struct {
	User                         User
	Version, Method, RequestHash string
}

func (s *Store) ConsumeTwoFactorLogin(ctx context.Context, hash, deviceID, code string, recovery bool, key []byte) (TwoFactorGrant, error) {
	var grant TwoFactorGrant
	var denied error
	err := s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		r, attempts, state, err := readTwoFactorRequest(ctx, tx, hash)
		if err != nil {
			return false, err
		}
		u := s.state.Users[r.UID]
		if r.Purpose != "login" || state != "pending" || attempts >= 5 || r.ExpiresAt <= time.Now().Unix() || r.DeviceID != deviceID || !u.Active || u.UID == 0 {
			return false, ErrTwoFactorInvalid
		}
		a, err := twoFactorAccount(ctx, tx, r.UID)
		if err != nil {
			return false, err
		}
		if a.EnabledAt == 0 || a.Version != r.Version {
			return false, ErrTwoFactorInvalid
		}
		if err = verifyTwoFactor(&a, key, code, recovery); err != nil {
			denied = err
			_, err = tx.ExecContext(ctx, `UPDATE twilight_two_factor_requests SET attempts=attempts+1 WHERE hash=$1`, hash)
			return false, err
		}
		if err = saveTwoFactorAccount(ctx, tx, a); err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE twilight_two_factor_requests SET state='consumed' WHERE hash=$1`, hash)
		grant = TwoFactorGrant{User: u, Version: a.Version, Method: r.Method, RequestHash: hash}
		return false, err
	})
	if err == nil {
		err = denied
	}
	return grant, err
}

func (s *Store) ChangeTwoFactor(ctx context.Context, expected User, sessionHash, code string, recovery, disable bool, key []byte, hashes []string) error {
	return s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		if !sameLoginIdentity(s.state.Users[expected.UID], expected) {
			return false, ErrTwoFactorInvalid
		}
		a, err := twoFactorAccount(ctx, tx, expected.UID)
		if err != nil {
			return false, err
		}
		if a.EnabledAt == 0 || !validTwoFactorSession(ctx, tx, a.UID, sessionHash, a.Version) {
			return false, ErrTwoFactorInvalid
		}
		if err = verifyTwoFactor(&a, key, code, recovery); err != nil {
			return false, err
		}
		a.Version, err = security.RandomHex(16)
		if err != nil {
			return false, err
		}
		a.Recovery = hashes
		if disable {
			a.Secret = ""
			a.Recovery = nil
			a.EnabledAt = 0
			a.LastStep = -1
		}
		if err = saveTwoFactorAccount(ctx, tx, a); err != nil {
			return false, err
		}
		return false, invalidateTwoFactorUser(ctx, tx, a.UID)
	})
}

// ResetTwoFactor is for the explicit offline recovery command only.
func (s *Store) ResetTwoFactor(ctx context.Context, uid int64) error {
	return s.withTelegramLinkState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		if _, ok := s.state.Users[uid]; !ok {
			return false, ErrNotFound
		}
		version, err := security.RandomHex(16)
		if err != nil {
			return false, err
		}
		if err = saveTwoFactorAccount(ctx, tx, TwoFactorAccount{UID: uid, Version: version, LastStep: -1}); err != nil {
			return false, err
		}
		return false, invalidateTwoFactorUser(ctx, tx, uid)
	})
}
