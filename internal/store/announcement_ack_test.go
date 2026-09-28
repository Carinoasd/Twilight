package store

import "testing"

func TestMarkAnnouncementsSeenIgnoresUnknownIDs(t *testing.T) {
	st := newJSONStoreForTest(t)
	u, err := st.CreateUser(User{Username: "reader", Role: RoleNormal})
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.UpsertAnnouncement(Announcement{Title: "t", Visible: true, ForceRead: true})
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]int64, 0, 1000)
	for i := int64(1); i <= 1000; i++ {
		junk = append(junk, ann.ID+i*7919)
	}
	if err := st.MarkAnnouncementsSeen(u.UID, append(junk, ann.ID)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.User(u.UID)
	if len(got.SeenAnnouncementIDs) != 1 || got.SeenAnnouncementIDs[0] != ann.ID {
		t.Fatalf("unknown announcement ids must be dropped, got %v", got.SeenAnnouncementIDs)
	}
}
