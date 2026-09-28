package api

import (
	"fmt"
	"sync"
	"time"
)

const (
	// telegramUnauthorizedCooldown 是同一 (chat, user) 两次「无权限」群内提示之间、
	// 以及同一 (chat, sender_chat) 两次匿名管理员验证面板之间的最短间隔。
	telegramUnauthorizedCooldown = 30 * time.Second
	telegramCooldownMaxEntries   = 4096
)

// telegramCooldown 是一个带容量上限的简单冷却表。
// 修复：非管理员点面板按钮、或用 sender_chat 发 /twguser，每次都会在群里发一条
// 消息并挂一个 30 秒定时器，没有任何节流，可被用来刷屏、耗尽 Bot 发信配额。
type telegramCooldown struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

// allow 在 key 不处于冷却期时返回 true 并开始新的冷却期。
func (c *telegramCooldown) allow(key string, cooldown time.Duration) bool {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]time.Time{}
	}
	if until, ok := c.entries[key]; ok && now.Before(until) {
		return false
	}
	if len(c.entries) >= telegramCooldownMaxEntries {
		for k, until := range c.entries {
			if !now.Before(until) {
				delete(c.entries, k)
			}
		}
		// 仍然满（大量并发刷屏）时整体清空；最坏情况是少量提示提前放行。
		if len(c.entries) >= telegramCooldownMaxEntries {
			c.entries = map[string]time.Time{}
		}
	}
	c.entries[key] = now.Add(cooldown)
	return true
}

func telegramCooldownKey(kind string, chatID, actorID int64) string {
	return fmt.Sprintf("%s:%d:%d", kind, chatID, actorID)
}
