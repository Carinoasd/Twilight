package api

import (
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/prejudice-studio/twilight/internal/store"
)

func (a *App) applyConfiguredAdmins() {
	if a.store() == nil {
		return
	}
	uidSet := a.configuredAdminUIDSet()
	nameSet := a.configuredAdminUsernameSet()
	if len(uidSet) == 0 && len(nameSet) == 0 {
		return
	}
	for _, user := range a.store().ListUsers() {
		if !configuredAdminMatchSets(uidSet, nameSet, user.UID, user.Username) {
			continue
		}
		updated, err := a.store().UpdateUser(user.UID, func(u *store.User) error {
			u.Role = store.RoleAdmin
			u.Active = true
			return nil
		})
		if err == nil {
			zap.L().Info("configured administrator applied", zap.Int64("uid", updated.UID), zap.String("username", updated.Username))
		}
	}
}

func (a *App) configuredAdminMatch(uid int64, username string) bool {
	return configuredAdminMatchSets(a.configuredAdminUIDSet(), a.configuredAdminUsernameSet(), uid, username)
}

func configuredAdminMatchSets(uidSet map[int64]bool, nameSet map[string]bool, uid int64, username string) bool {
	if uid > 0 && uidSet[uid] {
		return true
	}
	username = strings.ToLower(strings.TrimSpace(username))
	return username != "" && nameSet[username]
}

func (a *App) configuredAdminUIDSet() map[int64]bool {
	uidSet := map[int64]bool{}
	for _, uid := range a.cfg().AdminUIDs {
		if uid > 0 {
			uidSet[uid] = true
		}
	}
	return uidSet
}

func (a *App) configuredAdminUsernameSet() map[string]bool {
	nameSet := map[string]bool{}
	for _, username := range a.cfg().AdminUsernames {
		username = strings.ToLower(strings.TrimSpace(username))
		if username != "" {
			nameSet[username] = true
		}
	}
	return nameSet
}

// errReservedAdminUsername 表示目标用户名是配置文件里的管理员用户名，自助改名不可占用。
// 包装 store.ErrConflict，经 statusFromError 映射为 409，与“用户名已被占用”口径一致，
// 不向调用方透露该名字是管理员名单成员。
var errReservedAdminUsername = fmt.Errorf("%w: username reserved for configured administrator", store.ErrConflict)

// usernameReservedForConfiguredAdmin 报告 self 能否把用户名改成 name。
//
// Admin.usernames 只按名字比对：applyConfiguredAdmins 在启动/热重载时会把同名账号
// 提权为管理员，protectedUserReason 也会立刻把它当作受保护账号。若普通用户能自助
// 改名（或注册）成名单里的名字——例如原管理员已改名、名字空出——就能抢占这个名字，
// 在下次重载时拿到管理员。因此名单里的名字对普通用户保留：
//   - 与自己当前用户名大小写不敏感相同（只改大小写）时放行，不构成新的抢占；
//   - 自己已是管理员时放行，没有提权可言；
//   - 其余情况一律视为保留。
func (a *App) usernameReservedForConfiguredAdmin(name string, self store.User) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if normalized == "" || !a.configuredAdminUsernameSet()[normalized] {
		return false
	}
	if self.UID > 0 && strings.EqualFold(strings.TrimSpace(self.Username), normalized) {
		return false
	}
	if self.UID > 0 && self.Role == store.RoleAdmin {
		return false
	}
	return true
}
