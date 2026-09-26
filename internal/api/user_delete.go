package api

import (
	"context"

	"github.com/prejudice-studio/twilight/internal/store"
)

func (a *App) deleteLocalUser(ctx context.Context, u store.User) error {
	if err := a.store().DeleteUser(u.UID); err != nil {
		return err
	}
	if ctx != nil {
		a.sessions().DeleteUser(ctx, u.UID)
	}
	return nil
}

func (a *App) cleanupUserTelegramResidue(uid, telegramID int64) int {
	n, err := a.store().CleanupTelegramChallenges(context.Background(), 0, uid, telegramID)
	logTelegramChallengeFailure("cleanup_identity", err)
	return n
}
func (a *App) cleanupOrphanedUserBindCodes() int {
	n, err := a.store().CleanupOrphanedTelegramChallenges(context.Background())
	logTelegramChallengeFailure("cleanup_orphaned", err)
	return n
}
