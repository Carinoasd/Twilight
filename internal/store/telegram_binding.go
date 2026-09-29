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
			u.RebindEmbySuspended = false
		}
		s.state.Users[uid] = u
		s.maintainUserIndexes(previous, u, uid)
		updated = u
		return nil
	}, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM twilight_telegram_links WHERE uid=$1`, uid); err != nil {
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
//
// embySuspended 报告这次换绑流程是否亲自停用过远端 Emby（RebindEmbySuspended）；
// 调用方只应对这种账号恢复 Emby，换绑前就被单独封禁的 Emby 保持原状。
func (s *Store) CompleteUserTelegramRebind(uid, expectedTelegramID, expectedSince int64) (updated User, changed bool, embySuspended bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.mutateAndSaveLocked(func() error {
		changed, embySuspended = false, false
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
			embySuspended = u.RebindEmbySuspended
			u.RebindingInProgress = false
			u.RebindingSince = 0
			u.RebindEmbySuspended = false
			s.state.Users[uid] = u
			changed = true
		}
		updated = u
		return nil
	})
	if err != nil {
		return User{}, false, false, err
	}
	return updated, changed, embySuspended, nil
}

// MarkRebindEmbySuspended 记录“本次换绑流程停用了远端 Emby”。只在账号仍处于
// 同一个换绑周期（RebindingSince 未变）时写入，避免慢的远端调用把标记写到下一轮。
func (s *Store) MarkRebindEmbySuspended(uid, expectedSince int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutateAndSaveLocked(func() error {
		u, ok := s.state.Users[uid]
		if !ok {
			return ErrNotFound
		}
		if !u.RebindingInProgress || u.RebindingSince != expectedSince {
			return ErrConflict
		}
		if u.RebindEmbySuspended {
			return nil
		}
		u.RebindEmbySuspended = true
		s.state.Users[uid] = u
		return nil
	})
}

// BeginAdminTelegramRebind 是管理员解绑后要求用户立即重新绑定的入口：清除身份、
// 进入换绑状态并记录身份历史，与自助解绑共用同一套换绑完成逻辑。管理员账号不进入
// 换绑状态（管理员自己可以随时绑定）。
func (s *Store) BeginAdminTelegramRebind(uid, actorUID int64) (User, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var updated, previous User
	err := s.mutateAndSaveWithTxLocked(func() error {
		u, ok := s.state.Users[uid]
		if !ok {
			return ErrNotFound
		}
		if u.Role == RoleAdmin && u.UID != actorUID {
			return ErrConflict
		}
		previous = u
		u.TelegramID, u.TelegramUsername = 0, ""
		if u.Role != RoleAdmin {
			u.RebindingInProgress = true
			u.RebindingSince = time.Now().Unix()
			u.RebindEmbySuspended = false
		}
		s.state.Users[uid] = u
		s.maintainUserIndexes(previous, u, uid)
		updated = u
		return nil
	}, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM twilight_telegram_links WHERE uid=$1`, uid); err != nil {
			return err
		}
		if previous.TelegramID == 0 {
			return nil
		}
		return recordTelegramIdentity(ctx, tx, uid, previous.TelegramID, previous.TelegramUsername, "admin_unbind")
	})
	if err != nil {
		return User{}, 0, err
	}
	return updated, previous.TelegramID, nil
}
