package store

import (
	"net/netip"
	"sort"
	"strings"
	"time"
)

// NormalizeIPBlacklistEntry 校验并规范化黑名单条目：支持单个 IPv4/IPv6 地址与 CIDR 前缀。
// 原实现不校验也不规范化，按原字符串精确匹配 map key：填 1.2.3.0/24、带空白或非规范
// IPv6 都会写入成功却永远匹配不到。IPv4-mapped IPv6 统一成 IPv4；全长前缀存成单地址。
func NormalizeIPBlacklistEntry(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.Contains(value, "%") {
		return "", false
	}
	if strings.Contains(value, "/") {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return "", false
		}
		addr := prefix.Addr()
		bits := prefix.Bits()
		if addr.Is4In6() && bits >= 96 {
			addr = addr.Unmap()
			bits -= 96
		}
		prefix = netip.PrefixFrom(addr, bits).Masked()
		if prefix.Bits() == prefix.Addr().BitLen() {
			return prefix.Addr().String(), true
		}
		return prefix.String(), true
	}
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return "", false
	}
	return addr.Unmap().String(), true
}

func (s *Store) AddIPBlacklist(ip, reason string, expireAt int64) error {
	normalized, valid := NormalizeIPBlacklistEntry(ip)
	if !valid {
		return ErrInvalid
	}
	ip = normalized
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return err
	}
	s.state.IPBlacklist[ip] = IPBlacklistEntry{IP: ip, Reason: reason, CreatedAt: time.Now().Unix(), ExpireAt: expireAt}
	return s.saveLocked()
}

func (s *Store) RemoveIPBlacklist(ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return err
	}
	// 规范化后的 key 与原样 key 都删，兼容历史上未规范化写入的条目。
	if normalized, valid := NormalizeIPBlacklistEntry(ip); valid {
		delete(s.state.IPBlacklist, normalized)
	}
	delete(s.state.IPBlacklist, ip)
	return s.saveLocked()
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

// IsIPBlacklisted 先按规范化地址精确匹配，再逐条检查 CIDR 前缀（含历史未规范化的条目）。
func (s *Store) IsIPBlacklisted(ip string) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		entry, ok := s.state.IPBlacklist[ip]
		return ok && ipBlacklistEntryActive(entry)
	}
	addr = addr.Unmap().WithZone("")
	now := time.Now().Unix()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if entry, ok := s.state.IPBlacklist[addr.String()]; ok && ipBlacklistEntryActiveAt(entry, now) {
		return true
	}
	for key, entry := range s.state.IPBlacklist {
		if !ipBlacklistEntryActiveAt(entry, now) {
			continue
		}
		normalized, valid := NormalizeIPBlacklistEntry(key)
		if !valid {
			continue
		}
		if strings.Contains(normalized, "/") {
			if prefix, err := netip.ParsePrefix(normalized); err == nil && prefix.Contains(addr) {
				return true
			}
		} else if normalized == addr.String() {
			return true
		}
	}
	return false
}

func ipBlacklistEntryActive(entry IPBlacklistEntry) bool {
	return ipBlacklistEntryActiveAt(entry, time.Now().Unix())
}

func ipBlacklistEntryActiveAt(entry IPBlacklistEntry, now int64) bool {
	return entry.ExpireAt == -1 || entry.ExpireAt > now
}
