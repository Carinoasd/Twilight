package store

import (
	"errors"
	"testing"
)

func TestNormalizeIPBlacklistEntry(t *testing.T) {
	cases := map[string]string{
		" 1.2.3.4 ":           "1.2.3.4",
		"1.2.3.9/24":          "1.2.3.0/24",
		"1.2.3.4/32":          "1.2.3.4",
		"2001:DB8::0:1":       "2001:db8::1",
		"::ffff:10.0.0.1":     "10.0.0.1",
		"::ffff:10.0.0.0/120": "10.0.0.0/24",
		"2001:db8::/32":       "2001:db8::/32",
	}
	for input, want := range cases {
		if got, ok := NormalizeIPBlacklistEntry(input); !ok || got != want {
			t.Errorf("NormalizeIPBlacklistEntry(%q)=%q,%v want %q", input, got, ok, want)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "1.2.3.4/33", "fe80::1%eth0", "1.2.3.4 5"} {
		if _, ok := NormalizeIPBlacklistEntry(bad); ok {
			t.Errorf("NormalizeIPBlacklistEntry(%q) should be invalid", bad)
		}
	}
}

// CIDR、非规范 IPv6、带空白的条目都要能命中；格式错误的条目要被拒绝。
func TestIPBlacklistMatchesPrefixesAndNormalizedAddresses(t *testing.T) {
	st := newJSONStoreForTest(t)
	for _, entry := range []string{"198.51.100.0/24", " 203.0.113.7 ", "2001:DB8::0:1"} {
		if err := st.AddIPBlacklist(entry, "test", -1); err != nil {
			t.Fatalf("add %q: %v", entry, err)
		}
	}
	for _, ip := range []string{"198.51.100.77", "203.0.113.7", "2001:db8::1", "::ffff:198.51.100.1"} {
		if !st.IsIPBlacklisted(ip) {
			t.Errorf("%s should be blacklisted", ip)
		}
	}
	for _, ip := range []string{"198.51.101.1", "203.0.113.8", "2001:db8::2"} {
		if st.IsIPBlacklisted(ip) {
			t.Errorf("%s should not be blacklisted", ip)
		}
	}
	if err := st.AddIPBlacklist("not-an-ip", "", -1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid entry err=%v, want ErrInvalid", err)
	}
	if err := st.RemoveIPBlacklist("198.51.100.5/24"); err != nil {
		t.Fatal(err)
	}
	if st.IsIPBlacklisted("198.51.100.77") {
		t.Fatal("removing the same prefix in another spelling should unblock it")
	}
}
