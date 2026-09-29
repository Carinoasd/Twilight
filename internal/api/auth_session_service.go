package api

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// refreshSession rotates one authenticated session. The HTTP layer owns the
// response envelope and cookie headers; this function owns the state change so
// V1 compatibility routes and V2 resources cannot drift apart.
func (a *App) refreshSession(ctx context.Context, token string, uid int64) (string, time.Time, error) {
	// 续期沿用原会话的设备归属，否则续期后的会话脱离设备，封禁 / 淘汰设备时吊销不到。
	record, _ := a.sessions().GetRecord(ctx, token)
	a.sessions().Delete(ctx, token)
	return a.sessions().Create(ctx, uid, record.DeviceID)
}

// revokeDeviceSessions 吊销某用户某台设备上的全部会话（封禁 / 淘汰 / 删除设备时调用）。
func (a *App) revokeDeviceSessions(ctx context.Context, uid int64, deviceIDs ...string) {
	for _, deviceID := range deviceIDs {
		a.sessions().DeleteDevice(ctx, uid, deviceID)
	}
}

func (a *App) revokeSession(ctx context.Context, token string) {
	a.sessions().Delete(ctx, token)
}

func (a *App) revokeAllSessions(ctx context.Context, uid int64) {
	if err := a.store().RevokeTelegramLogins(ctx, uid); err != nil {
		zap.L().Warn("revoke telegram login requests failed", zap.Int64("uid", uid))
	}
	a.sessions().DeleteUser(ctx, uid)
}
