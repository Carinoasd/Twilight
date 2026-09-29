package store

import (
	"path/filepath"
	"testing"
)

// TestConsumeRebindRequestPreservesAuditTrail 锁定换绑审计链路的关键不变量：
// 用户用"已批准"的换绑请求解绑 Telegram 后，请求必须翻成 used 以防二次复用，
// 但管理员的审核元数据（ReviewerUID / AdminNote / ReviewedAt）必须原样保留。
//
// 回归背景：handleUnbindTelegram 曾用 ReviewRebindRequest(id, 0, "used", "auto-...")
// 消费请求，等于把 ReviewerUID 抹成 0、覆盖管理员原始备注、并把 ReviewedAt 重置
// 成 now——销毁了"哪位管理员、何时、为何批准"的审计痕迹。改走 ConsumeRebindRequest
// 后只翻 Status，本测试守护该差异。
func TestConsumeRebindRequestPreservesAuditTrail(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := telegramBindingUser(t, st, 111)

	created, err := st.CreateRebindRequest(RebindRequest{UID: u.UID, Username: u.Username, OldTelegramID: 111, Reason: "lost access"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != "pending" {
		t.Fatalf("new request should be pending, got %q", created.Status)
	}

	const reviewerUID = int64(42)
	const adminNote = "approved after manual identity check"
	reviewed, err := st.ReviewRebindRequest(created.ID, reviewerUID, "approved", adminNote)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Status != "approved" || reviewed.ReviewerUID != reviewerUID || reviewed.AdminNote != adminNote || reviewed.ReviewedAt == 0 {
		t.Fatalf("approval did not record audit metadata: %#v", reviewed)
	}

	if err := st.ConsumeRebindRequest(created.ID); err != nil {
		t.Fatal(err)
	}

	final, ok := st.UserLatestRebindRequest(u.UID)
	if !ok {
		t.Fatal("expected to find the consumed request")
	}
	if final.Status != "used" {
		t.Fatalf("consumed request should be marked used, got %q", final.Status)
	}
	// 审计元数据必须与审批时一致，不被消费动作改写。
	if final.ReviewerUID != reviewerUID {
		t.Fatalf("ReviewerUID overwritten by consume: got %d want %d", final.ReviewerUID, reviewerUID)
	}
	if final.AdminNote != adminNote {
		t.Fatalf("AdminNote overwritten by consume: got %q want %q", final.AdminNote, adminNote)
	}
	if final.ReviewedAt != reviewed.ReviewedAt {
		t.Fatalf("ReviewedAt overwritten by consume: got %d want %d", final.ReviewedAt, reviewed.ReviewedAt)
	}
}

// TestConsumeRebindRequestOnlyConsumesApproved 锁定 ConsumeRebindRequest 的状态
// 守卫：仅 approved 请求会被翻成 used；pending / rejected 请求不受影响，避免
// "解绑接口在非批准态下误把请求标记成 used"导致用户失去再次申请的机会。
func TestConsumeRebindRequestOnlyConsumesApproved(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pending, err := st.CreateRebindRequest(RebindRequest{UID: 1, Username: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ConsumeRebindRequest(pending.ID); err != nil {
		t.Fatal(err)
	}
	got, ok := st.UserLatestRebindRequest(1)
	if !ok || got.Status != "pending" {
		t.Fatalf("pending request must not be consumed: ok=%v status=%q", ok, got.Status)
	}
}
