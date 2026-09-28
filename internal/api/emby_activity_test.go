package api

import (
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

func TestEmbyActivityUserKeysAndScopedMatching(t *testing.T) {
	events := []embyActivityPlaybackEvent{
		{UserID: "EMBY-1", UserName: "Alice", UserKey: "alice"},
		{UserID: "emby-1", UserName: "Bob"},
	}
	keys := embyActivityUserKeys(events)
	if len(keys) != 3 {
		t.Fatalf("keys=%v, want three normalized keys", keys)
	}
	users := []store.User{
		{UID: 1, EmbyID: "emby-1", Username: "unrelated"},
		{UID: 2, EmbyID: "emby-2", Username: "Alice"},
		{UID: 3, EmbyID: "emby-3", Username: "nobody"},
	}
	matched := make([]store.User, 0, len(users))
	for _, user := range users {
		for _, key := range []string{user.EmbyID, user.EmbyUsername, user.Username} {
			if _, ok := keys[normalizeEmbyActivityUserKey(key)]; ok {
				matched = append(matched, user)
				break
			}
		}
	}
	if len(matched) != 2 || matched[0].UID != 1 || matched[1].UID != 2 {
		t.Fatalf("matched=%v, want users 1 and 2", matched)
	}
}

// TestEmbyActivityAttributionIgnoresSiteUsername 回归审查 L13：Emby 活动里的 UserName
// 只能对应本地绑定的 Emby 用户名，不能匹配站点用户名；同名歧义时宁可不记。
func TestEmbyActivityAttributionIgnoresSiteUsername(t *testing.T) {
	users := []store.User{
		{UID: 1, Username: "alice", EmbyID: "emby-1", EmbyUsername: "alice_tv"},
		{UID: 2, Username: "bob", EmbyID: "emby-2", EmbyUsername: "alice"},
		{UID: 3, Username: "carol", EmbyID: "emby-3", EmbyUsername: "dup"},
		{UID: 4, Username: "dave", EmbyID: "emby-4", EmbyUsername: "DUP"},
	}
	index := embyActivityUsersByKey(users)
	if got := index.resolve("", "alice"); got.UID != 2 {
		t.Fatalf("Emby name alice must map to the user bound to Emby alice (uid 2), got %d", got.UID)
	}
	if got := index.resolve("EMBY-1", "alice"); got.UID != 1 {
		t.Fatalf("Emby user id must win over name, got %d", got.UID)
	}
	if got := index.resolve("", "dup"); got.UID != 0 {
		t.Fatalf("ambiguous Emby name must not be attributed, got %d", got.UID)
	}
	if got := index.resolve("", "carol"); got.UID != 0 {
		t.Fatalf("site username must not be used for attribution, got %d", got.UID)
	}
}
