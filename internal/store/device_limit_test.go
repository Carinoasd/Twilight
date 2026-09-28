package store

import "testing"

func TestEnforceDeviceLimitEvictsOldestUntrusted(t *testing.T) {
	st := newJSONStoreForTest(t)
	// 4 台设备，LastSeen 越大越新。d4 受信任但最旧。
	mustDevice(t, st, Device{UID: 1, DeviceID: "d1", LastSeen: 100})
	mustDevice(t, st, Device{UID: 1, DeviceID: "d2", LastSeen: 200})
	mustDevice(t, st, Device{UID: 1, DeviceID: "d3", LastSeen: 300})
	mustDevice(t, st, Device{UID: 1, DeviceID: "d4", LastSeen: 50, Trusted: true})
	// 别的用户不受影响。
	mustDevice(t, st, Device{UID: 2, DeviceID: "x1", LastSeen: 10})

	evicted, err := st.EnforceDeviceLimit(1, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0] != "d1" {
		t.Fatalf("evicted = %v, want [d1]", evicted)
	}
	ids := deviceIDSet(st.ListDevices(1))
	// 保留：最近 2 台 d3/d2 + 受信任 d4；淘汰：最旧未受信任 d1。
	if ids["d1"] {
		t.Fatal("oldest untrusted device d1 should be evicted")
	}
	if !ids["d2"] || !ids["d3"] {
		t.Fatalf("recent devices must be kept, got %v", ids)
	}
	if !ids["d4"] {
		t.Fatal("trusted device must never be evicted")
	}
	if len(st.ListDevices(2)) != 1 {
		t.Fatal("other users' devices must be untouched")
	}
}

func TestEnforceDeviceLimitNoopWhenUnderLimit(t *testing.T) {
	st := newJSONStoreForTest(t)
	mustDevice(t, st, Device{UID: 1, DeviceID: "d1", LastSeen: 100})
	if _, err := st.EnforceDeviceLimit(1, 5, ""); err != nil {
		t.Fatal(err)
	}
	if len(st.ListDevices(1)) != 1 {
		t.Fatal("under-limit set must be unchanged")
	}
	// max<=0 表示不限制。
	mustDevice(t, st, Device{UID: 1, DeviceID: "d2", LastSeen: 200})
	if _, err := st.EnforceDeviceLimit(1, 0, ""); err != nil {
		t.Fatal(err)
	}
	if len(st.ListDevices(1)) != 2 {
		t.Fatal("max<=0 must not evict")
	}
}

// TestUpdateDevicePreservesFlagsOnRelogin 锁定登录改用 UpdateDevice 后的不变量：
// 再次「登录」（更新 UA/IP/LastSeen）不得清掉 FirstSeen / Trusted / Blocked。
func TestUpdateDevicePreservesFlagsOnRelogin(t *testing.T) {
	st := newJSONStoreForTest(t)
	mustDevice(t, st, Device{UID: 1, DeviceID: "d1", DeviceName: "UA-old", FirstSeen: 100, LastSeen: 100, Trusted: true})
	// 模拟再次登录：只刷新 UA/IP/LastSeen。
	if err := st.UpdateDevice(1, "d1", func(d *Device) {
		d.DeviceName = "UA-new"
		d.LastIP = "1.2.3.4"
		d.LastSeen = 500
	}); err != nil {
		t.Fatal(err)
	}
	got := st.ListDevices(1)
	if len(got) != 1 {
		t.Fatalf("expected 1 device, got %d", len(got))
	}
	d := got[0]
	if d.FirstSeen != 100 {
		t.Fatalf("FirstSeen must be preserved, got %d", d.FirstSeen)
	}
	if !d.Trusted {
		t.Fatal("Trusted must be preserved on relogin")
	}
	if d.DeviceName != "UA-new" || d.LastIP != "1.2.3.4" || d.LastSeen != 500 {
		t.Fatalf("UA/IP/LastSeen should refresh, got %+v", d)
	}
}

func mustDevice(t *testing.T, st *Store, d Device) {
	t.Helper()
	if err := st.UpsertDevice(d); err != nil {
		t.Fatal(err)
	}
}

func deviceIDSet(devices []Device) map[string]bool {
	out := map[string]bool{}
	for _, d := range devices {
		out[d.DeviceID] = true
	}
	return out
}

// TestTrustDeviceRequiresExistingUnblockedDevice 防回归：用户自助信任不能新建设备，
// 也不能解除管理员的封禁；已封禁设备不能被用户删除（删除等于解封）。
func TestTrustDeviceRequiresExistingUnblockedDevice(t *testing.T) {
	st := newJSONStoreForTest(t)
	if err := st.TrustDevice(1, "ghost"); err != ErrNotFound {
		t.Fatalf("trust missing device err=%v, want ErrNotFound", err)
	}
	if _, ok := st.Device(1, "ghost"); ok {
		t.Fatal("trust must not create a device")
	}
	mustDevice(t, st, Device{UID: 1, DeviceID: "bad", LastSeen: 10, Blocked: true})
	if err := st.TrustDevice(1, "bad"); err != ErrDeviceBlocked {
		t.Fatalf("trust blocked device err=%v, want ErrDeviceBlocked", err)
	}
	if err := st.DeleteDevice(1, "bad"); err != ErrDeviceBlocked {
		t.Fatalf("delete blocked device err=%v, want ErrDeviceBlocked", err)
	}
	if d, ok := st.Device(1, "bad"); !ok || !d.Blocked || d.Trusted {
		t.Fatalf("blocked device must stay blocked: ok=%v %+v", ok, d)
	}
	mustDevice(t, st, Device{UID: 1, DeviceID: "good", LastSeen: 20})
	if err := st.TrustDevice(1, "good"); err != nil {
		t.Fatal(err)
	}
	if d, _ := st.Device(1, "good"); !d.Trusted {
		t.Fatalf("existing device should be trusted: %+v", d)
	}
}
