package store

import (
	"context"
	"testing"
	"time"
)

// persistedStateVersion 直接从 PostgreSQL 读 twilight_state.version，用来断言某次调用
// 是否真的整份写了库。
func persistedStateVersion(t *testing.T, st *Store) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var version int64
	if err := st.db.QueryRowContext(ctx, `SELECT version FROM twilight_state WHERE id = 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// 第 6 条：闭包复检后无改动时不应整份落盘、递增 version。
func TestMutateNoChangeSkipsPersist(t *testing.T) {
	st, owner, ticket := conversationFixture(t)
	if _, err := st.CommitTicketAttachment(ticket.ID, TicketAttachment{Filename: "0000000000000009.png"}, owner, 5); err != nil {
		t.Fatal(err)
	}
	before := persistedStateVersion(t, st)
	// revision 对不上：条件不符，什么都不该写。
	removed, err := st.DetachExpiredTicketAttachments(ticket.ID, -1, time.Now().Unix()+10)
	if err != nil || len(removed) != 0 {
		t.Fatalf("detach: %v %v", removed, err)
	}
	if after := persistedStateVersion(t, st); after != before {
		t.Fatalf("no-op detach bumped version %d -> %d", before, after)
	}

	if _, err := st.CreateUser(User{Username: "tg-noop", Role: RoleNormal, Active: true, TelegramID: 88001, TelegramUsername: "same"}); err != nil {
		t.Fatal(err)
	}
	before = persistedStateVersion(t, st)
	// 读锁快路径会先挡掉用户名相同的情况；这里模拟另一进程已把用户名写成 same，
	// 而本进程内存仍是旧值，迫使请求进入锁内复检。
	st.mu.Lock()
	for uid, u := range st.state.Users {
		if u.TelegramID == 88001 {
			u.TelegramUsername = "stale"
			st.state.Users[uid] = u
		}
	}
	st.stateVersion = -1 // 强制 refresh 取回持久层的真实值
	st.mu.Unlock()
	before = persistedStateVersion(t, st)
	if _, changed, err := st.UpdateTelegramUsernameIfBound(88001, "same"); err != nil || changed {
		t.Fatalf("update username: changed=%v err=%v", changed, err)
	}
	if after := persistedStateVersion(t, st); after != before {
		t.Fatalf("no-op username update bumped version %d -> %d", before, after)
	}
}
