package store

import (
	"path/filepath"
	"testing"
)

// TestDisabledReasonClearedByOtherActiveChanges 验证停用原因的不变量：群成员巡检停用
// 会写 telegram_membership；之后任何其他路径改动 Active（管理员启用再停用）都会清掉
// 旧原因，回群自动启用因此不会误把管理员手动停权的人放出来。
func TestDisabledReasonClearedByOtherActiveChanges(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.CreateUser(User{Username: "reason", Role: RoleNormal, Active: true, PasswordHash: "x", TelegramID: 1})
	if err != nil {
		t.Fatal(err)
	}
	updated, disabled, _, err := st.DisableUserForTelegramMembership(u.UID)
	if err != nil || !disabled || updated.DisabledReason != DisabledReasonTelegramMembership {
		t.Fatalf("membership disable: disabled=%v err=%v user=%#v", disabled, err, updated)
	}
	if updated, err = st.SetUserActiveAtomic(u.UID, true); err != nil || updated.DisabledReason != "" {
		t.Fatalf("enable must clear reason: err=%v user=%#v", err, updated)
	}
	if updated, err = st.UpdateUser(u.UID, func(u *User) error { u.Active = false; return nil }); err != nil || updated.DisabledReason != "" {
		t.Fatalf("manual disable must not carry a reason: err=%v user=%#v", err, updated)
	}
	// Emby 自动停用标记在 EmbyDisabled=false 时一律清掉。
	if updated, err = st.UpdateUser(u.UID, func(u *User) error { u.EmbyAutoDisabled = true; return nil }); err != nil || updated.EmbyAutoDisabled {
		t.Fatalf("EmbyAutoDisabled without EmbyDisabled must be cleared: %#v", updated)
	}
}
