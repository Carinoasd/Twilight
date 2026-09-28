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
