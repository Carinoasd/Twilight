package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// Maps are immutable after publication; mutation replaces the whole map.
// Evidence uses the record key. Account checkpoints use accountID:recordKey.
type BangumiWatchRecord struct {
	SubjectID   string `json:"subject_id,omitempty"`
	SubjectName string `json:"subject_name,omitempty"`
	Episode     int    `json:"episode,omitempty"`
	Season      int    `json:"season,omitempty"`
	Completed   bool   `json:"completed,omitempty"`
	Manual      bool   `json:"manual,omitempty"`
	Status      string `json:"status,omitempty"`
	Message     string `json:"message,omitempty"`
	UpdatedAt   int64  `json:"updated_at,omitempty"`
}

func BangumiRecordKey(r PlaybackRecord) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", r.ItemID, r.SeriesName, strings.ToLower(r.MediaType), r.IndexNumber))))
}

func (s *Store) UpdateBangumiWatch(uid int64, key, token string, update func(*BangumiWatchRecord)) error {
	_, err := s.UpdateUser(uid, func(u *User) error {
		if token != "" && u.BGMToken != token {
			return fmt.Errorf("Bangumi account changed")
		}
		next := maps.Clone(u.BangumiWatch)
		if next == nil {
			next = make(map[string]BangumiWatchRecord)
		}
		entry := next[key]
		update(&entry)
		entry.UpdatedAt = time.Now().Unix()
		// Bound growth without deleting historical checkpoints.
		if _, exists := next[key]; !exists && len(next) >= 20000 {
			return fmt.Errorf("Bangumi checkpoint capacity reached")
		}
		next[key] = entry
		u.BangumiWatch = next
		return nil
	})
	return err
}

// A transaction-scoped advisory lock coordinates API and scheduler processes.
func (s *Store) LockBangumiSync(ctx context.Context, uid int64) (release func(), acquired bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, "twilight:bangumi:"+strconv.FormatInt(uid, 10)).Scan(&acquired)
	if err != nil || !acquired {
		_ = tx.Rollback()
		return nil, false, err
	}
	return func() { _ = tx.Rollback() }, true, nil
}
