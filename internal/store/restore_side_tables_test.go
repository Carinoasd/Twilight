package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/prejudice-studio/twilight/internal/migration"
)

// 恢复备份后，恢复点之后注册的用户已不存在：他们的播放统计与 Telegram 身份历史
// 必须清掉，否则回卷后重新分配到同一 UID 的新用户会继承；恢复后仍存在的用户保留。
func TestLoadSnapshotDropsSideTableRowsOfUsersNotInSnapshot(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	kept, err := st.CreateUser(User{Username: "kept", Active: true, Role: RoleNormal})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	later, err := st.CreateUser(User{Username: "later", Active: true, Role: RoleNormal})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, uid := range []int64{kept.UID, later.UID} {
		if err := st.AddPlaybackRecord(PlaybackRecord{UID: uid, ItemID: "item", Title: "t", MediaType: "Movie", Duration: 60, PlayedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordTelegramIdentity(ctx, uid, 1000+uid, "tg", "bind"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.LoadSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if got := st.PlaybackRecords(later.UID, 0, 10); len(got) != 0 {
		t.Fatalf("playback of user absent from snapshot survived restore: %#v", got)
	}
	if hist, err := st.GetTelegramIdentityHistory(ctx, later.UID, 10); err != nil || len(hist) != 0 {
		t.Fatalf("identity history of user absent from snapshot survived restore: %#v err=%v", hist, err)
	}
	if got := st.PlaybackRecords(kept.UID, 0, 10); len(got) != 1 {
		t.Fatalf("playback of restored user should be kept, got %#v", got)
	}
	if hist, err := st.GetTelegramIdentityHistory(ctx, kept.UID, 10); err != nil || len(hist) != 1 {
		t.Fatalf("identity history of restored user should be kept: %#v err=%v", hist, err)
	}
	// 回卷后新注册者拿到旧 UID，也不应看到任何继承数据。
	reused, err := st.CreateUser(User{Username: "reused", Active: true, Role: RoleNormal})
	if err != nil {
		t.Fatal(err)
	}
	if reused.UID == later.UID && len(st.PlaybackRecords(reused.UID, 0, 10)) != 0 {
		t.Fatal("reused uid inherited playback records")
	}
}

// 迁移导入会整体替换用户集合：目标站原有的 Telegram 身份历史不得挂到导入进来、
// uid 相同的账号上。
func TestImportMigrationArchiveClearsTelegramIdentityHistory(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	user, err := st.CreateUser(User{Username: "source", Active: true, Role: RoleNormal})
	if err != nil {
		t.Fatal(err)
	}
	files, err := st.ExportMigrationFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, _, err := migration.Create(migration.Input{TwilightVersion: "test", DatabaseSchemaVersion: "postgres-state-v1", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := migration.Open(archiveBytes, "", migration.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// 目标站在同一 uid 上有别人的身份历史。
	if err := st.RecordTelegramIdentity(ctx, user.UID, 424242, "someone_else", "bind"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportMigrationArchive(ctx, archive); err != nil {
		t.Fatal(err)
	}
	if hist, err := st.GetTelegramIdentityHistory(ctx, user.UID, 10); err != nil || len(hist) != 0 {
		t.Fatalf("target identity history leaked onto imported uid: %#v err=%v", hist, err)
	}
}
