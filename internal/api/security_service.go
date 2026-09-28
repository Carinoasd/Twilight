package api

import (
	"context"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

type securityService struct {
	app *App
}

func (s *securityService) listDevices(uid int64) []map[string]any {
	items := []map[string]any{}
	for _, d := range s.app.store().ListDevices(uid) {
		items = append(items, map[string]any{
			"device_id":   d.DeviceID,
			"device_name": d.DeviceName,
			"client":      d.Client,
			"last_ip":     d.LastIP,
			"first_seen":  d.FirstSeen,
			"last_seen":   d.LastSeen,
			"is_trusted":  d.Trusted,
			"blocked":     d.Blocked,
		})
	}
	return items
}

// blockDevice 由管理员封禁设备，并立即吊销该设备上已签发的会话——只改标记不吊销
// 会话时，被封设备上的登录态照样有效。
func (s *securityService) blockDevice(ctx context.Context, uid int64, deviceID string) error {
	if err := s.app.store().UpdateDevice(uid, deviceID, func(d *store.Device) {
		d.Blocked = true
		d.Trusted = false
	}); err != nil {
		return err
	}
	s.app.revokeDeviceSessions(ctx, uid, deviceID)
	return nil
}

// trustDevice 只作用于已存在且未被封禁的设备（见 store.TrustDevice）。
func (s *securityService) trustDevice(uid int64, deviceID string) error {
	return s.app.store().TrustDevice(uid, deviceID)
}

// deleteDevice 删除设备记录并吊销其会话：否则“删设备”后旧会话仍在线，且设备数
// 上限按记录计数，删记录就能绕过上限。已封禁设备不可删（store 返回 ErrDeviceBlocked）。
func (s *securityService) deleteDevice(ctx context.Context, uid int64, deviceID string) error {
	if err := s.app.store().DeleteDevice(uid, deviceID); err != nil {
		return err
	}
	s.app.revokeDeviceSessions(ctx, uid, deviceID)
	return nil
}

func (s *securityService) loginHistory(uid int64, limit int) []map[string]any {
	logs := s.app.store().LoginHistory(uid, false, 0, limit)
	items := make([]map[string]any, 0, len(logs))
	for _, log := range logs {
		items = append(items, map[string]any{
			"id":      log.ID,
			"ip":      log.IP,
			"device":  log.DeviceName,
			"client":  log.Client,
			"time":    log.Time,
			"blocked": log.Blocked,
			"country": log.Country,
			"city":    log.City,
		})
	}
	return items
}

func (s *securityService) listIPBlacklist() []store.IPBlacklistEntry {
	return s.app.store().ListIPBlacklist()
}

func (s *securityService) addIPBlacklist(ip, reason string, hours int) error {
	expireAt := int64(-1)
	if hours > 0 {
		expireAt = time.Now().Add(time.Duration(hours) * time.Hour).Unix()
	}
	return s.app.store().AddIPBlacklist(ip, reason, expireAt)
}

func (s *securityService) removeIPBlacklist(ip string) error {
	return s.app.store().RemoveIPBlacklist(ip)
}

func (s *securityService) suspiciousActivity(hours int) []map[string]any {
	logs := s.app.store().LoginHistory(0, true, time.Now().Add(-time.Duration(hours)*time.Hour).Unix(), 100)
	items := make([]map[string]any, 0, len(logs))
	for _, log := range logs {
		items = append(items, map[string]any{
			"uid":    log.UID,
			"ip":     log.IP,
			"device": log.DeviceName,
			"time":   log.Time,
			"reason": firstNonEmpty(log.Reason, "blocked"),
		})
	}
	return items
}

func (a *App) security() *securityService {
	return &securityService{app: a}
}
