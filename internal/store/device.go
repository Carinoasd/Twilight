package store

import (
	"sort"
	"time"
)

func (s *Store) UpsertDevice(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return err
	}
	if d.FirstSeen == 0 {
		d.FirstSeen = time.Now().Unix()
	}
	if d.LastSeen == 0 {
		d.LastSeen = d.FirstSeen
	}
	key := deviceKey(d.UID, d.DeviceID)
	previous, existed := s.state.Devices[key]
	if existed && previous == d {
		return nil
	}
	s.state.Devices[key] = d
	if err := s.saveLocked(); err != nil {
		if existed {
			s.state.Devices[key] = previous
		} else {
			delete(s.state.Devices, key)
		}
		return err
	}
	return nil
}

func (s *Store) ListDevices(uid int64) []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Device, 0)
	for _, d := range s.state.Devices {
		if d.UID == uid && !d.Blocked {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	return out
}

func (s *Store) UpdateDevice(uid int64, deviceID string, fn func(*Device)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return err
	}
	key := deviceKey(uid, deviceID)
	d, existed := s.state.Devices[key]
	if !existed {
		now := time.Now().Unix()
		d = Device{UID: uid, DeviceID: deviceID, DeviceName: deviceID, FirstSeen: now, LastSeen: now}
	}
	previous := d
	fn(&d)
	d.UID = uid
	d.DeviceID = deviceID
	if existed && previous == d {
		return nil
	}
	s.state.Devices[key] = d
	if err := s.saveLocked(); err != nil {
		if existed {
			s.state.Devices[key] = previous
		} else {
			delete(s.state.Devices, key)
		}
		return err
	}
	return nil
}

// Device 返回某用户的一台设备（含已封禁的）。
func (s *Store) Device(uid int64, deviceID string) (Device, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.state.Devices[deviceKey(uid, deviceID)]
	return d, ok
}

// TrustDevice 由用户自助把自己的设备标为受信任。
//
// 与 UpdateDevice 不同：设备不存在时返回 ErrNotFound 而不是新建——否则用户可以
// 用任意 device_id 造出无限多台受信任设备（受信任设备不参与 EnforceDeviceLimit
// 淘汰），既绕过设备数上限又让 state 无限膨胀；设备已被管理员封禁时返回
// ErrDeviceBlocked，用户不能借“信任”自行解封。
func (s *Store) TrustDevice(uid int64, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return err
	}
	key := deviceKey(uid, deviceID)
	previous, existed := s.state.Devices[key]
	if !existed {
		return ErrNotFound
	}
	if previous.Blocked {
		return ErrDeviceBlocked
	}
	if previous.Trusted {
		return nil
	}
	next := previous
	next.Trusted = true
	s.state.Devices[key] = next
	if err := s.saveLocked(); err != nil {
		s.state.Devices[key] = previous
		return err
	}
	return nil
}

// DeleteDevice 删除一台设备。已封禁的设备返回 ErrDeviceBlocked：封禁记录一旦删掉，
// 同一设备重新登录就不再被拦，用户自助删除等于自行解封。
func (s *Store) DeleteDevice(uid int64, deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return err
	}
	key := deviceKey(uid, deviceID)
	previous, existed := s.state.Devices[key]
	if !existed {
		return nil
	}
	if previous.Blocked {
		return ErrDeviceBlocked
	}
	delete(s.state.Devices, key)
	if err := s.saveLocked(); err != nil {
		s.state.Devices[key] = previous
		return err
	}
	return nil
}

// EnforceDeviceLimit 仅保留某用户最近活跃的 max 台设备（按 LastSeen 倒序），淘汰
// 更旧的「未受信任」设备。约定（防误踢/防锁死）：
//   - max<=0 视为不限制，直接返回；
//   - 受信任设备（Trusted）永不淘汰，即使超出名额；
//   - 刚登录的设备 LastSeen 最新，必在保留区，不会把当前会话踢掉；
//   - 已 Blocked 的设备不计入活跃集，也不参与淘汰（保持封禁状态）。
//
// 仅在 DeviceLimitEnabled 时由登录路径调用。返回被淘汰的设备 ID，调用方据此吊销
// 这些设备上的会话——只删设备记录而不吊销会话时，上限形同虚设。
//
// current 是本次登录的设备，无论 LastSeen 排序如何都保留：同一秒内多次登录时
// LastSeen 相同、排序不稳定，不指明的话可能把刚登录的设备淘汰掉。
func (s *Store) EnforceDeviceLimit(uid int64, max int, current string) ([]string, error) {
	if max <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshLocked(); err != nil {
		return nil, err
	}
	type keyed struct {
		key string
		dev Device
	}
	active := make([]keyed, 0)
	for key, d := range s.state.Devices {
		if d.UID == uid && !d.Blocked {
			active = append(active, keyed{key, d})
		}
	}
	if len(active) <= max {
		return nil, nil
	}
	sort.SliceStable(active, func(i, j int) bool {
		ci, cj := active[i].dev.DeviceID == current, active[j].dev.DeviceID == current
		if ci != cj {
			return ci
		}
		return active[i].dev.LastSeen > active[j].dev.LastSeen
	})
	var previous map[string]Device
	var evicted []string
	for i, item := range active {
		if i < max || item.dev.Trusted {
			continue // 在名额内，或受信任 → 保留
		}
		if previous == nil {
			previous = make(map[string]Device)
		}
		previous[item.key] = item.dev
		delete(s.state.Devices, item.key)
		evicted = append(evicted, item.dev.DeviceID)
	}
	if len(evicted) == 0 {
		return nil, nil
	}
	if err := s.saveLocked(); err != nil {
		for key, dev := range previous {
			s.state.Devices[key] = dev
		}
		return nil, err
	}
	return evicted, nil
}

func deviceKey(uid int64, deviceID string) string {
	return strconv36(uid) + ":" + deviceID
}
