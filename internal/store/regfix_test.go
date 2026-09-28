package store

import (
	"slices"
	"testing"
)

// 删号不得退还注册码次数、不得重新启用码，否则单次码可借 /delAccount 重放；
// 同时 UsedByUIDs 必须新建切片，不能改写锁外调用方持有的旧快照。
func TestDeleteUserKeepsRegCodeConsumption(t *testing.T) {
	st := newJSONStoreForTest(t)
	victim, err := st.CreateUser(User{Username: "del-replay", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateUser(User{Username: "del-other", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.state.RegCodes["ONE-SHOT"] = RegCode{Code: "ONE-SHOT", Type: 1, Days: 365, ValidityTime: -1, UseCountLimit: 1, UseCount: 1, UsedBy: victim.UID, UsedByUIDs: []int64{victim.UID}, Active: false}
	st.state.RegCodes["MULTI"] = RegCode{Code: "MULTI", Type: 1, Days: 30, ValidityTime: -1, UseCountLimit: 5, UseCount: 2, UsedBy: other.UID, UsedByUIDs: []int64{victim.UID, other.UID}, Active: true}
	if err := st.saveLocked(); err != nil {
		st.mu.Unlock()
		t.Fatal(err)
	}
	st.mu.Unlock()

	snapshot, _ := st.RegCode("MULTI")
	snapshotUIDs := append([]int64(nil), snapshot.UsedByUIDs...)

	if err := st.DeleteUser(victim.UID); err != nil {
		t.Fatal(err)
	}

	one, ok := st.RegCode("ONE-SHOT")
	if !ok {
		t.Fatal("ONE-SHOT missing")
	}
	if one.UseCount != 1 || one.Active {
		t.Fatalf("single-use code must stay consumed after user deletion: %+v", one)
	}
	if one.UsedBy != 0 || len(one.UsedByUIDs) != 0 {
		t.Fatalf("references to deleted uid must be removed: %+v", one)
	}
	multi, _ := st.RegCode("MULTI")
	if multi.UseCount != 2 || !slices.Equal(multi.UsedByUIDs, []int64{other.UID}) {
		t.Fatalf("multi-use code must keep count and only drop deleted uid: %+v", multi)
	}
	if !slices.Equal(snapshot.UsedByUIDs, snapshotUIDs) {
		t.Fatalf("caller snapshot was mutated in place: got %v want %v", snapshot.UsedByUIDs, snapshotUIDs)
	}
}

// 下级断开邀请关系后，原邀请码不得被第二个账号再次使用。
func TestDetachInviteDoesNotReviveInviteCode(t *testing.T) {
	st := newJSONStoreForTest(t)
	parent, err := st.CreateUser(User{Username: "inv-parent", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateUser(User{Username: "inv-child", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	alt, err := st.CreateUser(User{Username: "inv-alt", PasswordHash: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertInviteCode(InviteCode{Code: "INV-ONCE", UID: parent.UID, InviterUID: parent.UID, Days: 30, UseCountLimit: 1, Active: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeInviteCode("INV-ONCE", child.UID); err != nil {
		t.Fatal(err)
	}
	if err := st.DetachInvite(child.UID); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.ParentOf(child.UID); ok {
		t.Fatal("relation should be detached")
	}
	if _, err := st.ConsumeInviteCode("INV-ONCE", alt.UID); err == nil {
		t.Fatal("detached invite code must not be reusable by another account")
	}
}

// 管理员移除 Emby / 清注册队列后，授权锁仍在：残留修复（启动、重载设置时执行）
// 不得据码侧使用记录重新发放 PendingEmby。
func TestRepairRegistrationResidueSkipsRevokedGrant(t *testing.T) {
	st := newJSONStoreForTest(t)
	revoked, err := st.CreateUser(User{Username: "revoked", PasswordHash: "x", EmbyGrantLocked: true, RegistrationSource: "regcode", RegistrationCode: "USED-30"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRegCode(RegCode{Code: "USED-30", Type: 1, Days: 30, Active: false, UseCountLimit: 1, UseCount: 1, UsedBy: revoked.UID, UsedByUIDs: []int64{revoked.UID}}); err != nil {
		t.Fatal(err)
	}
	result, err := st.RepairRegistrationResidue()
	if err != nil {
		t.Fatal(err)
	}
	if result.RestoredPendingEntitlement != 0 {
		t.Fatalf("revoked grant must not be re-issued: %#v", result)
	}
	got, _ := st.User(revoked.UID)
	if got.PendingEmby || got.PendingEmbyDays != nil {
		t.Fatalf("pending entitlement re-issued for revoked user: %#v", got)
	}
}

// 批量删除注册码遇到版本冲突重放时，deleted / missing 不得重复累加。
func TestDeleteRegCodesReplayDoesNotDuplicateResults(t *testing.T) {
	st := newJSONStoreForTest(t)
	for _, code := range []string{"DEL-A", "DEL-B"} {
		if err := st.UpsertRegCode(RegCode{Code: code, Type: 1, Days: 30, UseCountLimit: 1, Active: true}); err != nil {
			t.Fatal(err)
		}
	}
	other := reopenTestStore(t)
	fired := false
	beforeSaveHookForTest = func(attempt int) {
		// 钩子是包级变量，other 的写入也会触发它：只在第一次触发，避免递归自锁。
		if attempt == 0 && !fired {
			fired = true
			// 另一进程抢先提交一次写入，制造版本冲突迫使闭包重放。
			if _, err := other.CreateUser(User{Username: "conflict-writer", PasswordHash: "x"}); err != nil {
				t.Errorf("conflict writer: %v", err)
			}
		}
	}
	t.Cleanup(func() { beforeSaveHookForTest = nil })
	deleted, missing, err := st.DeleteRegCodes([]string{"DEL-A", "DEL-B", "DEL-MISSING"})
	beforeSaveHookForTest = nil
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"DEL-A", "DEL-B"}) || !slices.Equal(missing, []string{"DEL-MISSING"}) {
		t.Fatalf("replay duplicated results: deleted=%v missing=%v", deleted, missing)
	}
	if _, ok := st.FindUserByUsername("conflict-writer"); !ok {
		t.Fatal("conflict writer's user should survive the replay")
	}
}
