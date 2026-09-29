package store

import (
	"errors"
	"strings"
	"time"
)

// ErrEmbyCapacityReached 表示在 store 写锁内复核 Emby 名额时发现已达上限。
var ErrEmbyCapacityReached = errors.New("emby capacity reached")

// EmbyOccupancyOptions 描述一次 Emby 名额占用统计的口径。
type EmbyOccupancyOptions struct {
	// ExcludeUID 不计入统计的用户（通常是本次正在开通的用户本人）。
	ExcludeUID int64
	// ExcludeRegCode 本次将消费的注册码（锁外预检，尚未消费）：只从它的剩余名额里扣 1。
	ExcludeRegCode string
	// ConsumedRegCode 本次已在同一把锁内消费过的注册码（锁内复核）：有限次码的
	// UseCount 已包含本次，不再扣减；无限次码（-1）恒按 1 计，需扣掉本次这 1 个，
	// 否则锁内复核会比锁外预检多算 1。
	ConsumedRegCode string
	// CountActiveUsers 对应 emby_direct_register_enabled：开启自由开通时每个
	// 活跃用户都视为潜在占用。
	CountActiveUsers bool
}

// EmbyOccupancy 统计当前占用 / 预占 Emby 名额的数量：已绑定 Emby 或处于
// PendingEmby 的用户（自由开通开启时再加上活跃用户），以及管理员签发的有效注册码
// / 白名单码（type 1/3）的剩余次数。无限次码（-1）按 1 计。
func (s *Store) EmbyOccupancy(opt EmbyOccupancyOptions) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.embyOccupancyLocked(opt)
}

// EmbyOccupancyHeldLock 与 EmbyOccupancy 相同，但不加锁：只能在 store 已持有写锁的
// 回调里调用（ConsumeRegCodeAndUpdateUser / ConsumeInviteCodeAndUpdateUser /
// CreateUserForRegistration 等的 fn）。用来在消费的同一把锁内复核名额——锁外检查
// 与消费之间存在 TOCTOU，多个用户同时兑换同一张无限次码（剩余名额只算 1）时会
// 全部通过并超发。
func (s *Store) EmbyOccupancyHeldLock(opt EmbyOccupancyOptions) int {
	return s.embyOccupancyLocked(opt)
}

func (s *Store) embyOccupancyLocked(opt EmbyOccupancyOptions) int {
	current := 0
	for uid, u := range s.state.Users {
		if uid == opt.ExcludeUID {
			continue
		}
		if strings.TrimSpace(u.EmbyID) != "" || u.PendingEmby || (opt.CountActiveUsers && u.Active) {
			current++
		}
	}
	now := time.Now().Unix()
	for _, code := range s.state.RegCodes {
		if !code.Active || code.IsDecoy || RegCodeExpired(code, now) {
			continue
		}
		if code.Type != 1 && code.Type != 3 {
			continue
		}
		slots := remainingRegCodeSlots(code.UseCount, code.UseCountLimit)
		// 本次消费只占用该码 1 个名额：仅扣减 1，而非把整码剩余名额全部排除。
		if opt.ExcludeRegCode != "" && strings.EqualFold(code.Code, opt.ExcludeRegCode) && slots > 0 {
			slots--
		}
		if opt.ConsumedRegCode != "" && code.UseCountLimit == -1 && strings.EqualFold(code.Code, opt.ConsumedRegCode) && slots > 0 {
			slots--
		}
		current += slots
	}
	return current
}

func remainingRegCodeSlots(used, limit int) int {
	if limit == -1 {
		return 1
	}
	if limit <= used {
		return 0
	}
	return limit - used
}
