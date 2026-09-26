package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var (
	ErrTelegramAlreadyBound           = errors.New("telegram already bound")
	ErrTelegramRebindApprovalRequired = errors.New("telegram rebind approval required")
)

// BindUnboundUserTelegram is the self-service binding boundary. Changing an
// existing identity must first pass the approved unbind operation; callers
// cannot reuse the administrative override to skip that policy.
func (s *Store) BindUnboundUserTelegram(uid, telegramID int64, username string) (User, error) {
	u, _, err := s.bindUserTelegram(uid, telegramID, username, uid, true)
	return u, err
}

// UnbindUserTelegram commits identity removal, approval consumption, rebind
// flags and identity history together. expectedTelegramID protects stale HTTP
// requests from unbinding an identity that changed after authentication.
func (s *Store) UnbindUserTelegram(uid, expectedTelegramID int64) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var updated, previous User
	err := s.mutateAndSaveWithTxLocked(func() error {
		u, ok := s.state.Users[uid]
		if !ok {
			return ErrNotFound
		}
		if !u.Active {
			return ErrInvalid
		}
		if u.TelegramID != expectedTelegramID {
			return ErrConflict
		}
		if u.Role != RoleAdmin {
			req, found := s.latestRebindRequestLocked(uid)
			if !found || req.Status != "approved" || req.OldTelegramID != u.TelegramID {
				return ErrTelegramRebindApprovalRequired
			}
			req.Status = "used"
			s.state.RebindRequests[req.ID] = req
		}
		previous = u
		u.TelegramID, u.TelegramUsername = 0, ""
		if u.Role != RoleAdmin {
			u.RebindingInProgress = true
			u.RebindingSince = time.Now().Unix()
		}
		s.state.Users[uid] = u
		s.maintainUserIndexes(previous, u, uid)
		updated = u
		return nil
	}, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM twilight_telegram_challenges WHERE uid=$1`, uid); err != nil {
			return err
		}
		if previous.TelegramID == 0 {
			return nil
		}
		return recordTelegramIdentity(ctx, tx, uid, previous.TelegramID, previous.TelegramUsername, "unbind")
	})
	if err != nil {
		return User{}, err
	}
	return updated, nil
}

// CompleteUserTelegramRebind applies a membership result only to the identity
// and rebind attempt checked by the caller. External checks run without a store
// lock; a concurrent unbind or administrative change must invalidate them.
func (s *Store) CompleteUserTelegramRebind(uid, expectedTelegramID, expectedSince int64) (User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var updated User
	var changed bool
	err := s.mutateAndSaveLocked(func() error {
		changed = false
		u, ok := s.state.Users[uid]
		if !ok {
			return ErrNotFound
		}
		if !u.Active || expectedTelegramID <= 0 {
			return ErrInvalid
		}
		if u.TelegramID != expectedTelegramID {
			return ErrConflict
		}
		if u.RebindingInProgress {
			if u.RebindingSince != expectedSince {
				return ErrConflict
			}
			u.RebindingInProgress = false
			u.RebindingSince = 0
			s.state.Users[uid] = u
			changed = true
		}
		updated = u
		return nil
	})
	if err != nil {
		return User{}, false, err
	}
	return updated, changed, nil
}
