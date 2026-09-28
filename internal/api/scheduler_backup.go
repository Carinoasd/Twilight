package api

import (
	"fmt"
	"os"
	"sort"

	"github.com/prejudice-studio/twilight/internal/store"
)

// autoBackupNote 是自动备份写进备注的固定标记。轮替只删除带这个标记的备份，
// 管理员手动创建的备份永远不会被自动删掉。
const autoBackupNote = "自动备份（定时任务 auto_backup_database）"

// runAutoBackupDatabase 创建一份数据库备份，并只保留最近 keep 份自动备份。
func (a *App) runAutoBackupDatabase(keep int) (map[string]any, []string, error) {
	if keep < 1 {
		keep = 1
	}
	dir := a.cfg().DatabaseBackupDir
	info, err := a.store().BackupWithNote(dir, autoBackupNote)
	if err != nil {
		return map[string]any{"success": false, "keep": keep}, nil, fmt.Errorf("create backup: %w", err)
	}
	logs := []string{fmt.Sprintf("created backup %s (%d bytes)", info.Name, info.Size)}
	backups, err := store.ListBackups(dir)
	if err != nil {
		return map[string]any{"success": false, "backup": info.Name, "keep": keep}, logs, fmt.Errorf("list backups: %w", err)
	}
	autos := []store.BackupInfo{}
	for _, b := range backups {
		if b.Note == autoBackupNote {
			autos = append(autos, b)
		}
	}
	// 按文件修改时间（纳秒）从新到旧排：ListBackups 的 CreatedAt 只有秒级，同一秒内的
	// 多份备份会排错，文件名里的纳秒段也没有补零，不能拿来比较。
	modNanos := make(map[string]int64, len(autos))
	for _, b := range autos {
		if st, err := os.Stat(b.Path); err == nil {
			modNanos[b.Name] = st.ModTime().UnixNano()
		}
	}
	sort.SliceStable(autos, func(i, j int) bool { return modNanos[autos[i].Name] > modNanos[autos[j].Name] })
	deleted, failed := []string{}, 0
	for _, old := range autos[min(keep, len(autos)):] {
		if old.Name == info.Name {
			continue
		}
		target, err := store.ResolveBackupPath(dir, old.Name)
		if err == nil {
			err = os.Remove(target)
		}
		if err != nil {
			failed++
			logs = append(logs, fmt.Sprintf("failed to delete old backup %s: %s", old.Name, redactSensitiveText(err.Error())))
			continue
		}
		_ = os.Remove(store.BackupMetaPath(target))
		deleted = append(deleted, old.Name)
	}
	a.auditSystem("scheduler", "auto_backup_database", 0, map[string]any{"backup": info.Name, "size": info.Size, "keep": keep, "deleted": deleted})
	logs = append(logs, fmt.Sprintf("kept %d auto backups, deleted %d", min(keep, len(autos)), len(deleted)))
	return map[string]any{"success": failed == 0, "backup": info.Name, "size": info.Size, "keep": keep, "deleted": len(deleted), "deleted_names": deleted, "delete_failed": failed}, logs, nil
}
