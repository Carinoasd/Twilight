package store

import (
	"context"
)

// PlaybackAvatarVisible shares only current avatars of users with recorded plays.
// Avoid copying the complete user state for each image request. A database read
// failure must not expose an avatar whose audience we cannot establish.
func (s *Store) PlaybackAvatarVisible(ctx context.Context, avatar string) bool {
	if avatar == "" {
		return false
	}
	uids := []int64{}
	s.mu.RLock()
	for _, user := range s.state.Users {
		if user.Avatar == avatar {
			uids = append(uids, user.UID)
		}
	}
	s.mu.RUnlock()
	if len(uids) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, pgPlaybackReadTimeout)
	defer cancel()
	var visible bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM twilight_playback_records WHERE uid = ANY($1))`, uids).Scan(&visible)
	return err == nil && visible
}
