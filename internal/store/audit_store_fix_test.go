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

// injectStateWriteFailure 在 twilight_state 上挂一个 BEFORE UPDATE 触发器让整份写入
// 失败，而持久层 version 保持不变——正是「PG 抖动、逾时」时直接写路径留下幽灵变更的
// 情境。返回的函数（以及 t.Cleanup）会移除触发器。
func injectStateWriteFailure(t *testing.T, st *Store) func() {
	t.Helper()
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION twilight_test_fail_state() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'injected state write failure'; END $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `CREATE TRIGGER twilight_test_fail_state BEFORE UPDATE ON twilight_state FOR EACH ROW EXECUTE FUNCTION twilight_test_fail_state()`); err != nil {
		t.Fatal(err)
	}
	removed := false
	remove := func() {
		if removed {
			return
		}
		removed = true
		if _, err := st.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS twilight_test_fail_state ON twilight_state`); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(remove)
	return remove
}

// 第 2 条：直接写路径存档失败后，内存里不应留下幽灵变更；之后无关的写入也不能把它带进库。
func TestDirectWritersRollbackOnSaveFailure(t *testing.T) {
	st := newJSONStoreForTest(t)
	user, err := st.CreateUser(User{Username: "ghost-owner", Role: RoleNormal, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.AddSchedulerRunReturning(SchedulerRun{JobID: "ghost-job", Status: "running", StartedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	base := PlaybackRecord{UID: user.UID, ItemID: "item-base", PlayedAt: 1_700_000_000, Duration: 10, Source: "activity_log"}
	if _, err := st.AddPlaybackRecordIdempotent(base); err != nil {
		t.Fatal(err)
	}

	remove := injectStateWriteFailure(t, st)
	ghost := PlaybackRecord{UID: user.UID, ItemID: "item-ghost", PlayedAt: 1_700_000_100, Duration: 5}
	calls := map[string]func() error{
		"AddLoginLog":    func() error { return st.AddLoginLog(LoginLog{UID: user.UID, IP: "203.0.113.9"}) },
		"AddIPBlacklist": func() error { return st.AddIPBlacklist("203.0.113.9", "ghost", -1) },
		"AddSchedulerRun": func() error {
			return st.AddSchedulerRun(SchedulerRun{JobID: "ghost-job-2", Status: "running"})
		},
		"UpdateSchedulerRun": func() error {
			_, err := st.UpdateSchedulerRun(run.ID, func(r *SchedulerRun) error { r.Status = "success"; return nil })
			return err
		},
		"AddPlaybackRecordIdempotent": func() error { _, err := st.AddPlaybackRecordIdempotent(ghost); return err },
		"AddPlaybackRecordsIdempotent": func() error {
			_, err := st.AddPlaybackRecordsIdempotent([]PlaybackRecord{{UID: user.UID, ItemID: "item-batch", PlayedAt: 1_700_000_200}})
			return err
		},
		"applyPlaybackReportingMemory": func() error {
			_, _, err := st.applyPlaybackReportingMemory([]PlaybackRecord{{UID: user.UID, ItemID: "item-base", PlayedAt: 1_700_000_000, Duration: 999}})
			return err
		},
		"UpdateDevice": func() error {
			return st.UpdateDevice(user.UID, "ghost-device", func(d *Device) { d.LastIP = "203.0.113.9" })
		},
		"RecordLogin": func() error {
			return st.RecordLogin(user.UID, "ghost-device", func(d *Device) {}, LoginLog{UID: user.UID})
		},
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s: expected injected failure", name)
		}
	}
	assertNoGhost := func(where string, st *Store) {
		t.Helper()
		st.mu.RLock()
		defer st.mu.RUnlock()
		if len(st.state.LoginLogs) != 0 {
			t.Errorf("%s: ghost login logs %+v", where, st.state.LoginLogs)
		}
		if _, ok := st.state.IPBlacklist["203.0.113.9"]; ok {
			t.Errorf("%s: ghost ip blacklist entry", where)
		}
		for _, r := range st.state.SchedulerRuns {
			if r.JobID == "ghost-job-2" || (r.ID == run.ID && r.Status != "running") {
				t.Errorf("%s: ghost scheduler change %+v", where, r)
			}
		}
		for _, p := range st.state.PlaybackRecords {
			if p.ItemID != "item-base" || p.Duration != 10 {
				t.Errorf("%s: ghost playback record %+v", where, p)
			}
		}
		if _, ok := st.state.Devices[deviceKey(user.UID, "ghost-device")]; ok {
			t.Errorf("%s: ghost device", where)
		}
	}
	assertNoGhost("memory after failure", st)

	remove()
	// 无关写入成功后，持久层也不能夹带任何幽灵变更。
	if _, err := st.CreateUser(User{Username: "unrelated", Role: RoleNormal, Active: true}); err != nil {
		t.Fatal(err)
	}
	assertNoGhost("persisted after unrelated write", reopenTestStore(t))

	// 失败后重送必须真的写入，而不是被幽灵记录当成重复。
	inserted, err := st.AddPlaybackRecordIdempotent(ghost)
	if err != nil || !inserted {
		t.Fatalf("retry after failure: inserted=%v err=%v", inserted, err)
	}
}

// 第 2 条：applyPlaybackReportingMemory 只有命中修正、没有新增时也必须落盘。
func TestPlaybackReportingMemoryPersistsMatchedOnly(t *testing.T) {
	st := newJSONStoreForTest(t)
	user, err := st.CreateUser(User{Username: "reporting-owner", Role: RoleNormal, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddPlaybackRecordIdempotent(PlaybackRecord{UID: user.UID, ItemID: "item-r", PlayedAt: 1_700_000_000, Duration: 10}); err != nil {
		t.Fatal(err)
	}
	matched, inserted, err := st.applyPlaybackReportingMemory([]PlaybackRecord{{UID: user.UID, ItemID: "item-r", PlayedAt: 1_700_000_050, Duration: 777}})
	if err != nil || matched != 1 || inserted != 0 {
		t.Fatalf("apply: matched=%d inserted=%d err=%v", matched, inserted, err)
	}
	other := reopenTestStore(t)
	other.mu.RLock()
	defer other.mu.RUnlock()
	if len(other.state.PlaybackRecords) != 1 || other.state.PlaybackRecords[0].Duration != 777 || other.state.PlaybackRecords[0].Source != PlaybackSourceReporting {
		t.Fatalf("matched-only correction not persisted: %+v", other.state.PlaybackRecords)
	}
}

// 第 6 条：登录时设备更新与登录记录合并成一次整份写入。
func TestRecordLoginWritesOnce(t *testing.T) {
	st := newJSONStoreForTest(t)
	user, err := st.CreateUser(User{Username: "login-once", Role: RoleNormal, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	before := persistedStateVersion(t, st)
	if err := st.RecordLogin(user.UID, "dev-1", func(d *Device) { d.LastIP = "198.51.100.1" }, LoginLog{UID: user.UID, IP: "198.51.100.1", DeviceID: "dev-1"}); err != nil {
		t.Fatal(err)
	}
	if after := persistedStateVersion(t, st); after != before+1 {
		t.Fatalf("login wrote %d times, want 1", after-before)
	}
	if logs := st.LoginHistory(user.UID, false, 0, 10); len(logs) != 1 || logs[0].DeviceID != "dev-1" {
		t.Fatalf("login log: %+v", logs)
	}
	if devices := st.ListDevices(user.UID); len(devices) != 1 || devices[0].LastIP != "198.51.100.1" {
		t.Fatalf("device: %+v", devices)
	}
}

// forceConflictOnce 让下一次 mutateAndSave 在 mutate 之后、写库之前，由另一个 Store
// （模拟他进程）先写一笔，从而必然撞上版本守卫并重放闭包。
func forceConflictOnce(t *testing.T, write func(other *Store)) {
	t.Helper()
	other := reopenTestStore(t)
	fired := false
	testHookBeforePersist = func() {
		if fired {
			return
		}
		fired = true
		testHookBeforePersist = nil
		write(other)
	}
	t.Cleanup(func() {
		testHookBeforePersist = nil
		if !fired {
			t.Errorf("conflict hook never fired")
		}
	})
}

// 第 3 条：批次改媒体请求状态遇到版本冲突重放时，seen 不能残留导致误报 ErrInvalid。
func TestMediaRequestBatchStatusSurvivesConflictReplay(t *testing.T) {
	st := newJSONStoreForTest(t)
	var items []MediaRequestBatchItem
	for i := 0; i < 2; i++ {
		req, err := st.CreateMediaRequest(MediaRequest{UID: 1, Source: "tmdb", MediaID: int64(100 + i), Title: "m"})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, MediaRequestBatchItem{RequireKey: req.RequireKey})
	}
	forceConflictOnce(t, func(other *Store) {
		if err := other.AddViolationLog(ViolationLog{Code: "x"}); err != nil {
			t.Error(err)
		}
	})
	updated, err := st.UpdateMediaRequestsStatusByKey(items, MediaRequestStatusCompleted, "", false)
	if err != nil {
		t.Fatalf("batch update after conflict replay: %v", err)
	}
	if len(updated) != len(items) {
		t.Fatalf("updated %d requests, want %d", len(updated), len(items))
	}
}

// 第 3 条：清理计数在冲突重放后不能翻倍。
func TestCleanupCountNotDoubledOnConflictReplay(t *testing.T) {
	st := newJSONStoreForTest(t)
	now := time.Now().Unix()
	for _, id := range []string{"ev-1", "ev-2"} {
		if err := st.PutEmailVerification(EmailVerification{ID: id, Purpose: "bind", Email: id + "@example.com", ExpiresAt: now - 10}); err != nil {
			t.Fatal(err)
		}
	}
	forceConflictOnce(t, func(other *Store) {
		if err := other.AddViolationLog(ViolationLog{Code: "x"}); err != nil {
			t.Error(err)
		}
	})
	deleted, err := st.CleanupExpiredEmailVerifications(now)
	if err != nil || deleted != 2 {
		t.Fatalf("cleanup deleted=%d err=%v, want 2", deleted, err)
	}
}

// 第 3 条：带 ID==0 判断的新建路径在重放时必须重新分配 ID，不能覆盖他进程刚建的条目。
func TestUpsertAnnouncementReplayAllocatesFreshID(t *testing.T) {
	st := newJSONStoreForTest(t)
	forceConflictOnce(t, func(other *Store) {
		if _, err := other.UpsertAnnouncement(Announcement{Title: "from-other", Content: "o", Visible: true}); err != nil {
			t.Error(err)
		}
	})
	mine, err := st.UpsertAnnouncement(Announcement{Title: "mine", Content: "m", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	all := st.ListAnnouncements(true)
	if len(all) != 2 {
		t.Fatalf("announcements=%+v, want both (mine id=%d)", all, mine.ID)
	}
}

// 第 6 条：ViolationLogs 有上限，只保留最新的记录。
func TestViolationLogsAreCapped(t *testing.T) {
	st := newJSONStoreForTest(t)
	st.mu.Lock()
	for i := 0; i < maxStoredViolationLogs+5; i++ {
		st.state.ViolationLogs = append(st.state.ViolationLogs, ViolationLog{ID: int64(i + 1), Code: "seed"})
	}
	st.state.NextViolationLogID = int64(maxStoredViolationLogs + 6)
	if err := st.saveLocked(); err != nil {
		st.mu.Unlock()
		t.Fatal(err)
	}
	st.mu.Unlock()
	if err := st.AddViolationLog(ViolationLog{Code: "newest"}); err != nil {
		t.Fatal(err)
	}
	logs := st.ListViolationLogs()
	if len(logs) != maxStoredViolationLogs {
		t.Fatalf("violation logs=%d, want %d", len(logs), maxStoredViolationLogs)
	}
	if logs[0].Code != "newest" {
		t.Fatalf("newest log dropped: %+v", logs[0])
	}
}

// 第 7 条：两个进程同时冷启动，后到者播种时不能覆盖先到者已写入的数据。
func TestColdStartSeedDoesNotOverwriteConcurrentWriter(t *testing.T) {
	st := newJSONStoreForTest(t)
	if _, err := st.db.ExecContext(context.Background(), `DELETE FROM twilight_state`); err != nil {
		t.Fatal(err)
	}
	testHookColdStartBeforeSeed = func() {
		testHookColdStartBeforeSeed = nil
		// 另一进程抢先冷启动（播种）并完成第一笔写入。
		first := reopenTestStore(t)
		if _, err := first.CreateUser(User{Username: "first-writer", Role: RoleNormal, Active: true}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { testHookColdStartBeforeSeed = nil })
	late := reopenTestStore(t)
	if _, ok := late.FindUserByUsername("first-writer"); !ok {
		t.Fatalf("late cold start does not see first writer's user")
	}
	check := reopenTestStore(t)
	if _, ok := check.FindUserByUsername("first-writer"); !ok {
		t.Fatalf("late cold start overwrote the persisted state")
	}
}
