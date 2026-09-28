package store

import "time"

const maxStoredLoginLogs = 1000

func (s *Store) AddLoginLog(log LoginLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 走 mutateAndSaveLocked：存档失败回滚内存、版本冲突重放。旧的「refresh → 直接改
	// → saveLocked」失败后把登录记录留在内存里成为幽灵变更，之后被无关写入顺手落盘。
	return s.mutateAndSaveLocked(func() error {
		s.appendLoginLogLocked(log)
		return nil
	})
}

// appendLoginLogLocked 在 mutate 闭包内追加一条登录记录。log 按值传入，重放时
// 每次都基于最新 NextLoginLogID 重新分配 ID。
func (s *Store) appendLoginLogLocked(log LoginLog) {
	if log.ID == 0 {
		log.ID = s.state.NextLoginLogID
		s.state.NextLoginLogID++
	}
	if log.Time == 0 {
		log.Time = time.Now().Unix()
	}
	s.state.LoginLogs = prependBoundedHead(s.state.LoginLogs, log, maxStoredLoginLogs)
}

func (s *Store) LoginHistory(uid int64, blockedOnly bool, since int64, limit int) []LoginLog {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	out := make([]LoginLog, 0, limit)
	for _, log := range s.state.LoginLogs {
		if uid != 0 && log.UID != uid {
			continue
		}
		if blockedOnly && !log.Blocked {
			continue
		}
		if since > 0 && log.Time < since {
			continue
		}
		out = append(out, log)
		if len(out) >= limit {
			break
		}
	}
	return out
}
