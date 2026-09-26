package store

import (
	"context"
	"database/sql"
	"time"
)

// createRegistrationUserLocked mutates only the caller's transaction snapshot.
// It must never call another Store method or perform external I/O.
func (s *Store) createRegistrationUserLocked(u User, regCode string, bind BindCode, now int64, fn func(*User, RegCode, BindCode) error) (User, RegCode, error) {
	if s.usernameExistsLocked(u.Username) || s.emailTakenLocked(u.Email, 0) || s.embyIDTakenLocked(u.EmbyID, 0) {
		return User{}, RegCode{}, ErrConflict
	}
	if bind.Code != "" {
		if bind.ExpiresAt <= now {
			return User{}, RegCode{}, ErrExpired
		}
		if bind.Scene != "register" || !bind.Confirmed || bind.TelegramID <= 0 {
			return User{}, RegCode{}, ErrConflict
		}
		u.TelegramID, u.TelegramUsername = bind.TelegramID, bind.TelegramUsername
	}
	if s.telegramIDTakenLocked(u.TelegramID, 0) {
		return User{}, RegCode{}, ErrConflict
	}
	var consumed RegCode
	if regCode != "" {
		reg, err := s.consumableRegCodeLocked(regCode, 0, 0, now)
		if err != nil {
			return User{}, RegCode{}, err
		}
		if reg.Type != 1 || reg.IsDecoy || !regCodeMatchesUser(reg, u) {
			return User{}, RegCode{}, ErrNotFound
		}
		consumed = reg
	}
	u.UID = s.state.NextUserID
	s.state.NextUserID++
	if u.CreatedAt == 0 {
		u.CreatedAt = now
	}
	if u.RegisterTime == 0 {
		u.RegisterTime = now
	}
	if u.ExpiredAt == 0 {
		u.ExpiredAt = -1
	}
	u.Active = true
	if consumed.Code != "" {
		consumed = s.consumeRegCodeLocked(consumed, u.UID, u.TelegramID)
	}
	if fn != nil {
		if err := fn(&u, consumed, bind); err != nil {
			return User{}, RegCode{}, err
		}
	}
	if s.embyIDTakenLocked(u.EmbyID, u.UID) || s.telegramIDTakenLocked(u.TelegramID, u.UID) || s.emailTakenLocked(u.Email, u.UID) || s.usernameExistsLocked(u.Username) {
		return User{}, RegCode{}, ErrConflict
	}
	s.state.Users[u.UID] = u
	s.maintainUserIndexes(User{}, u, u.UID)
	return u, consumed, nil
}

// RegisterWithTelegramChallenge commits account creation, registration grant,
// identity history and single-use challenge consumption in one transaction.
func (s *Store) RegisterWithTelegramChallenge(ctx context.Context, u User, regCode, key, ownerHash string, fn func(*User, RegCode, BindCode) error) (User, RegCode, BindCode, error) {
	var created User
	var consumed RegCode
	var bind BindCode
	err := s.withTelegramChallengeState(ctx, func(ctx context.Context, tx *sql.Tx) (bool, error) {
		c, err := challengeInTx(ctx, tx, key)
		if err != nil {
			return false, err
		}
		if !c.OwnedBy(0, ownerHash) {
			return false, ErrTelegramChallengeOwner
		}
		now := time.Now().Unix()
		if c.ExpiresAt <= now {
			return false, ErrExpired
		}
		bind = c.BindCode
		if c.State != "verified" {
			return false, ErrConflict
		}
		created, consumed, err = s.createRegistrationUserLocked(u, regCode, bind, now, fn)
		if err != nil {
			return false, err
		}
		if err = recordTelegramIdentity(ctx, tx, created.UID, c.TelegramID, c.TelegramUsername, "register"); err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `UPDATE twilight_telegram_challenges SET state='consumed', consumed_at=$2, result_uid=$3, revision=revision+1 WHERE id=$1`, c.ID, now, created.UID)
		return true, err
	})
	if err != nil {
		return User{}, RegCode{}, bind, err
	}
	return created, consumed, bind, nil
}
