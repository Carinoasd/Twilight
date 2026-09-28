package store

import (
	"errors"
	"testing"
	"time"
)

// X 邀请 Y 之后，X 不能再使用 Y 发的邀请码：否则形成 X↔Y 环，之后所有按父链向上
// 走的计算都会在全局写锁里死循环（根用户上限开启时，任何人用码都会让进程卡死）。
func TestInviteCodeRejectsCycle(t *testing.T) {
	st := newJSONStoreForTest(t)
	x, err := st.CreateUser(User{Username: "x", Role: RoleNormal})
	if err != nil {
		t.Fatal(err)
	}
	y, err := st.CreateUser(User{Username: "y", Role: RoleNormal})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, c := range []InviteCode{
		{Code: "FROM-X", InviterUID: x.UID, UseCountLimit: 1, Active: true, CreatedAt: now},
		{Code: "FROM-Y", InviterUID: y.UID, UseCountLimit: 1, Active: true, CreatedAt: now},
	} {
		if err := st.UpsertInviteCode(c); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.ConsumeInviteCodeAndUpdateUser("FROM-X", y.UID, 5, 100, nil); err != nil {
		t.Fatalf("y uses x's code: %v", err)
	}
	if _, _, err := st.ConsumeInviteCodeAndUpdateUser("FROM-Y", x.UID, 5, 100, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("cycle must be rejected, got %v", err)
	}
	if _, err := st.ConsumeInviteCode("FROM-Y", x.UID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cycle must be rejected on the plain consume path, got %v", err)
	}
}

// 修复前可能已经存在的环：向上走的计算必须能结束。
func TestInviteAncestorWalksTerminateOnExistingCycle(t *testing.T) {
	st := newJSONStoreForTest(t)
	st.mu.Lock()
	st.state.InviteRelations[1] = InviteRelation{ParentUID: 2, ChildUID: 1}
	st.state.InviteRelations[2] = InviteRelation{ParentUID: 1, ChildUID: 2}
	done := make(chan struct{})
	go func() {
		_ = st.inviteRootLocked(1)
		_ = st.isDescendantLocked(1, 99)
		_ = st.inviteDescendantCountLocked(1)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ancestor walk did not terminate on a cycle")
	}
	st.mu.Unlock()
}
