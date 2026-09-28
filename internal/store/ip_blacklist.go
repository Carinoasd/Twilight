package store

import (
	"sort"
	"time"
)

func (s *Store) AddIPBlacklist(ip, reason string, expireAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 走 mutateAndSaveLocked：存档失败时回滚内存，避免未落盘的封禁残留在本进程生效。
	return s.mutateAndSaveLocked(func() error {
		s.state.IPBlacklist[ip] = IPBlacklistEntry{IP: ip, Reason: reason, CreatedAt: time.Now().Unix(), ExpireAt: expireAt}
		return nil
	})
}

func (s *Store) RemoveIPBlacklist(ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutateAndSaveLocked(func() error {
		if _, ok := s.state.IPBlacklist[ip]; !ok {
			return errNoChange
		}
		delete(s.state.IPBlacklist, ip)
		return nil
	})
}

func (s *Store) ListIPBlacklist() []IPBlacklistEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]IPBlacklistEntry, 0, len(s.state.IPBlacklist))
	for _, entry := range s.state.IPBlacklist {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func (s *Store) IsIPBlacklisted(ip string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.state.IPBlacklist[ip]
	if !ok {
		return false
	}
	return entry.ExpireAt == -1 || entry.ExpireAt > time.Now().Unix()
}
