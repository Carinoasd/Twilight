package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const TelegramChallengeLimit = 10000

var ErrTelegramChallengeOwner = errors.New("telegram challenge owner mismatch")
var ErrTelegramChallengeCapacity = errors.New("telegram challenge capacity reached")

type TelegramChallenge struct {
	ID, TokenHash, OwnerHash string
	BindCode
	State                           string
	ConsumedAt, ResultUID, Revision int64
	ErrorCode, ErrorMessage         string
	ErrorStatus                     int
	Retryable                       bool
	ExpectedRebindSince             int64
	CurrentTelegramID               *int64
}

func TelegramChallengeHash(token string) string {
	sum := sha256.Sum256([]byte("twilight:telegram-challenge:v1:" + strings.ToUpper(strings.TrimSpace(token))))
	return hex.EncodeToString(sum[:])
}

func TelegramBrowserHash(proof string) string {
	if proof == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("twilight:telegram-browser:v1:" + proof))
	return hex.EncodeToString(sum[:])
}

func (c TelegramChallenge) OwnedBy(uid int64, ownerHash string) bool {
	if c.Scene == "user" {
		return uid > 0 && c.UID == uid
	}
	return uid == 0 && ownerHash != "" && c.OwnerHash != "" && subtle.ConstantTimeCompare([]byte(c.OwnerHash), []byte(ownerHash)) == 1
}

const telegramChallengeColumns = `id, token_hash, owner_hash, uid, scene, state, telegram_id, telegram_username,
 created_at, expires_at, consumed_at, result_uid, revision, error_code, error_message, error_status, retryable, expected_rebind_since`

type challengeScanner interface{ Scan(...any) error }

func scanTelegramChallenge(row challengeScanner) (TelegramChallenge, error) {
	var c TelegramChallenge
	err := row.Scan(&c.ID, &c.TokenHash, &c.OwnerHash, &c.UID, &c.Scene, &c.State, &c.TelegramID, &c.TelegramUsername,
		&c.CreatedAt, &c.ExpiresAt, &c.ConsumedAt, &c.ResultUID, &c.Revision, &c.ErrorCode, &c.ErrorMessage, &c.ErrorStatus, &c.Retryable, &c.ExpectedRebindSince)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.Confirmed = c.State == "verified" || c.State == "consumed"
	return c, err
}

// TelegramChallenge reads shared state directly. A database failure is never a
// missing/expired challenge and must be surfaced by the transport adapter.
func (s *Store) TelegramChallenge(ctx context.Context, key string) (TelegramChallenge, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// A single statement observes the challenge and current identity consistently,
	// including after another process bound/unbound the account during a wait.
	row := s.db.QueryRowContext(ctx, `SELECT `+telegramChallengeColumns+`,
 CASE WHEN uid>0 THEN (SELECT (state->'users'->(uid::text)->>'telegram_id')::bigint FROM twilight_state WHERE id=1) ELSE NULL END
 FROM twilight_telegram_challenges WHERE id=$1 OR token_hash=$2`, strings.ToUpper(strings.TrimSpace(key)), TelegramChallengeHash(key))
	var currentID *int64
	c, err := scanTelegramChallenge(challengeIdentityScanner{row: row, currentID: &currentID})
	c.CurrentTelegramID = currentID
	c.Code = strings.ToUpper(strings.TrimSpace(key))
	return c, err
}

// withTelegramChallengeState locks in this order: process Store, state row,
// challenge row. It shares one transaction with user/registration mutations;
// no network calls or externally committed Store methods may run in fn.
func (s *Store) withTelegramChallengeState(ctx context.Context, fn func(context.Context, *sql.Tx) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN version=$1 THEN NULL ELSE state END, version FROM twilight_state WHERE id=1 FOR UPDATE`, s.stateVersion).Scan(&raw, &version)
	if err != nil {
		return err
	}
	if len(raw) > 0 {
		var state State
		if err = json.Unmarshal(raw, &state); err != nil {
			return err
		}
		state.ensure()
		s.state, s.stateRaw, s.stateVersion = state, raw, version
		s.rebuildUserIndexes()
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
		saved, savedVersion, err = s.saveStateInTxLocked(ctx, tx, false)
		if err != nil {
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

func (s *Store) CreateTelegramChallenge(ctx context.Context, c TelegramChallenge) error {
	c.ID = strings.ToUpper(strings.TrimSpace(c.ID))
	validHex := func(value string, size int) bool {
		decoded, err := hex.DecodeString(value)
		return err == nil && len(decoded) == size
	}
	now := time.Now().Unix()
	if !validHex(c.ID, 16) || !validHex(c.TokenHash, 32) || c.Confirmed || c.TelegramID != 0 || c.CreatedAt <= 0 || c.CreatedAt > now+60 || c.ExpiresAt <= now || c.ExpiresAt <= c.CreatedAt || c.ExpiresAt-c.CreatedAt > 600 {
		return ErrInvalid
	}
	if (c.Scene == "register" && (c.UID != 0 || !validHex(c.OwnerHash, 32))) || (c.Scene == "user" && (c.UID <= 0 || c.OwnerHash != "")) || (c.Scene != "register" && c.Scene != "user") {
		return ErrInvalid
	}

	return s.withTelegramChallengeState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		if c.Scene == "user" {
			u, ok := s.state.Users[c.UID]
			if !ok {
				return false, ErrNotFound
			}
			if !u.Active {
				return false, ErrInvalid
			}
			if u.TelegramID != 0 {
				return false, ErrTelegramAlreadyBound
			}
			c.ExpectedRebindSince = u.RebindingSince
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM twilight_telegram_challenges WHERE expires_at <= $1`, time.Now().Unix()); err != nil {
			return false, err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM twilight_telegram_challenges`).Scan(&count); err != nil {
			return false, err
		}
		if count >= TelegramChallengeLimit {
			return false, ErrTelegramChallengeCapacity
		}
		// A fresh challenge supersedes prior pending/verified attempts for its
		// browser or account. Never erase consumed results on a retry.
		if c.UID > 0 || c.OwnerHash != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE twilight_telegram_challenges SET state='cancelled', revision=revision+1
 WHERE state IN ('pending','verified') AND scene=$1 AND (($2::bigint>0 AND uid=$2) OR ($3<>'' AND owner_hash=$3))`, c.Scene, c.UID, c.OwnerHash); err != nil {
				return false, err
			}
		}
		state := "pending"
		_, err := tx.ExecContext(ctx, `INSERT INTO twilight_telegram_challenges
 (id,token_hash,owner_hash,uid,scene,state,telegram_id,telegram_username,created_at,expires_at,expected_rebind_since)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, c.ID, c.TokenHash, c.OwnerHash, c.UID, c.Scene, state, c.TelegramID, c.TelegramUsername, c.CreatedAt, c.ExpiresAt, c.ExpectedRebindSince)
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) && pgerr.Code == "23505" {
			return false, ErrConflict
		}
		return false, err
	})
}

func challengeInTx(ctx context.Context, tx *sql.Tx, key string) (TelegramChallenge, error) {
	c, err := scanTelegramChallenge(tx.QueryRowContext(ctx, `SELECT `+telegramChallengeColumns+` FROM twilight_telegram_challenges WHERE id=$1 OR token_hash=$2 FOR UPDATE`, strings.ToUpper(strings.TrimSpace(key)), TelegramChallengeHash(key)))
	c.Code = strings.ToUpper(strings.TrimSpace(key))
	return c, err
}

func (s *Store) ConfirmTelegramChallenge(ctx context.Context, token string, telegramID int64, username string) (TelegramChallenge, User, bool, error) {
	var result TelegramChallenge
	var user User
	var bound bool
	err := s.withTelegramChallengeState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		c, err := challengeInTx(ctx, tx, token)
		if err != nil {
			return false, err
		}
		// Resource IDs are observation handles, never Bot credentials.
		if subtle.ConstantTimeCompare([]byte(c.TokenHash), []byte(TelegramChallengeHash(token))) != 1 {
			return false, ErrNotFound
		}
		now := time.Now().Unix()
		if c.ExpiresAt <= now {
			return false, ErrExpired
		}
		if c.State == "cancelled" || telegramID <= 0 {
			return false, ErrConflict
		}
		if c.Confirmed {
			if c.TelegramID != telegramID {
				return false, ErrConflict
			}
			if c.UID > 0 {
				u, ok := s.state.Users[c.UID]
				if !ok || !u.Active || u.TelegramID != telegramID {
					return false, ErrConflict
				}
			}
			if c.UID == 0 && c.State == "consumed" {
				u, ok := s.state.Users[c.ResultUID]
				if !ok || !u.Active || u.TelegramID != telegramID {
					return false, ErrConflict
				}
			}
			if c.UID == 0 && c.State != "consumed" && s.telegramIDTakenLocked(telegramID, 0) {
				return false, ErrConflict
			}
			result = c
			return false, nil
		}
		if s.telegramIDTakenLocked(telegramID, c.UID) {
			return false, ErrConflict
		}
		c.TelegramID, c.TelegramUsername, c.Confirmed = telegramID, strings.TrimPrefix(strings.TrimSpace(username), "@"), true
		c.State = "verified"
		if c.UID > 0 {
			u, ok := s.state.Users[c.UID]
			if !ok {
				return false, ErrNotFound
			}
			if u.TelegramID != 0 {
				return false, ErrTelegramAlreadyBound
			}
			if !u.Active || u.RebindingSince != c.ExpectedRebindSince {
				return false, ErrConflict
			}
			previous := u
			u.TelegramID, u.TelegramUsername = telegramID, c.TelegramUsername
			s.state.Users[u.UID] = u
			s.maintainUserIndexes(previous, u, u.UID)
			user, bound = u, true
			c.State, c.ConsumedAt, c.ResultUID = "consumed", now, u.UID
			if err = recordTelegramIdentity(ctx, tx, u.UID, telegramID, c.TelegramUsername, "bind"); err != nil {
				return false, err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE twilight_telegram_challenges SET state=$2, telegram_id=$3, telegram_username=$4,
 consumed_at=$5,result_uid=$6,revision=revision+1,error_code='',error_message='',error_status=0,retryable=false WHERE id=$1`, c.ID, c.State, c.TelegramID, c.TelegramUsername, c.ConsumedAt, c.ResultUID)
		c.Revision++
		result = c
		return bound, err
	})
	if err != nil {
		return TelegramChallenge{}, User{}, false, err
	}
	return result, user, bound, nil
}

func (s *Store) DeleteTelegramChallenge(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM twilight_telegram_challenges WHERE id=$1 OR token_hash=$2`, strings.ToUpper(strings.TrimSpace(key)), TelegramChallengeHash(key))
	return err
}

func (s *Store) CleanupTelegramChallenges(ctx context.Context, now int64, uid, telegramID int64) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `DELETE FROM twilight_telegram_challenges WHERE expires_at<=$1 OR ($2::bigint>0 AND uid=$2) OR ($3::bigint>0 AND telegram_id=$3)`, now, uid, telegramID)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// SetTelegramChallengeFailure is conditional on the observed revision: a slow
// failed check cannot replace a concurrent successful confirmation.
func (s *Store) SetTelegramChallengeFailure(ctx context.Context, c TelegramChallenge, code, message string, status int, retryable bool) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE twilight_telegram_challenges SET error_code=$3,error_message=$4,error_status=$5,retryable=$6,revision=revision+1
 WHERE id=$1 AND revision=$2 AND state='pending'`, c.ID, c.Revision, code, message, status, retryable)
	return err
}

func (s *Store) CleanupOrphanedTelegramChallenges(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `DELETE FROM twilight_telegram_challenges c USING twilight_state s WHERE s.id=1 AND c.uid>0 AND NOT (s.state->'users' ? c.uid::text)`)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

// Append a read-only identity projection without duplicating the row scanner.
type challengeIdentityScanner struct {
	row       *sql.Row
	currentID **int64
}

func (r challengeIdentityScanner) Scan(dest ...any) error {
	return r.row.Scan(append(dest, r.currentID)...)
}
