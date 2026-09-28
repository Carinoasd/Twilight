package store

import (
	"testing"
	"time"
)

func TestAuditAPIKeyExpiryLookup(t *testing.T) {
	st := newJSONStoreForTest(t)
	u, err := st.CreateUser(User{Username: "expiry-owner", Active: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, tc := range []struct {
		name   string
		expiry int64
		want   bool
	}{
		{"expired", now - 60, false}, {"boundary", now, false},
		{"future", now + 3600, true}, {"unset", 0, true}, {"permanent", -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := st.CreateAPIKey(APIKey{UID: u.UID, Hash: tc.name, ExpiredAt: tc.expiry})
			if err != nil {
				t.Fatal(err)
			}
			for _, indexed := range []bool{true, false} {
				st.mu.Lock()
				if indexed {
					st.rebuildUserIndexes()
				} else {
					st.apiKeyHashMap = nil
				}
				st.mu.Unlock()
				if _, _, ok := st.FindAPIKeyByHash(key.Hash); ok != tc.want {
					t.Errorf("indexed=%t authenticated=%t want=%t", indexed, ok, tc.want)
				}
			}
		})
	}
}
