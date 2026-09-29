package store

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// 第 4 条：以下测试需要 -race 才能证明。读者在释放读锁后编码回传的副本，写者随后在
// 写锁下改写同一条记录；若副本与 s.state 共用底层数组或 map，race detector 会报
// DATA RACE（map 的情况还可能直接 fatal）。
func raceReadThenWrite(t *testing.T, read func() any, write func()) {
	t.Helper()
	got := make(chan any)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		v := read()
		got <- v
		deadline := time.Now().Add(150 * time.Millisecond)
		for time.Now().Before(deadline) {
			if _, err := json.Marshal(v); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	<-got
	write()
	wg.Wait()
}

func TestTicketReadCopyDoesNotRaceAttachmentRemoval(t *testing.T) {
	st, owner, ticket := conversationFixture(t)
	for _, name := range []string{"0000000000000001.png", "0000000000000002.png", "0000000000000003.png", "0000000000000004.png"} {
		if _, err := st.CommitTicketAttachment(ticket.ID, TicketAttachment{Filename: name}, owner, 10); err != nil {
			t.Fatal(err)
		}
	}
	raceReadThenWrite(t, func() any {
		got, _ := st.Ticket(ticket.ID)
		return got
	}, func() {
		if _, err := st.RemoveTicketAttachment(ticket.ID, "0000000000000001.png", RoleAdmin); err != nil {
			t.Error(err)
		}
	})
}

func TestSchedulerRunCopyDoesNotRaceInterruptMark(t *testing.T) {
	st := newJSONStoreForTest(t)
	if err := st.AddSchedulerRun(SchedulerRun{JobID: "race-job", Status: "running", StartedAt: 1, Summary: map[string]any{"seed": 1}}); err != nil {
		t.Fatal(err)
	}
	raceReadThenWrite(t, func() any {
		return st.SchedulerRuns("race-job", 10)
	}, func() {
		if n, err := st.MarkInterruptedSchedulerRuns("race-job", time.Now().Unix(), time.Now().Unix()); err != nil || n != 1 {
			t.Errorf("mark interrupted: %d %v", n, err)
		}
	})
}

func TestRegCodeCopyDoesNotRaceUserDeletion(t *testing.T) {
	st := newJSONStoreForTest(t)
	var uids []int64
	for _, name := range []string{"rc-a", "rc-b", "rc-c"} {
		u, err := st.CreateUser(User{Username: name, Role: RoleNormal, Active: true})
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, u.UID)
	}
	if err := st.UpsertRegCode(RegCode{Code: "RACE-CODE", Type: 1, UseCountLimit: 10, UseCount: 3, UsedByUIDs: uids, Active: true}); err != nil {
		t.Fatal(err)
	}
	raceReadThenWrite(t, func() any {
		rc, _ := st.RegCode("RACE-CODE")
		return rc
	}, func() {
		if err := st.DeleteUser(uids[0]); err != nil {
			t.Error(err)
		}
	})
}

func TestMediaRequestCopyDoesNotRaceMediaInfoUpdate(t *testing.T) {
	st := newJSONStoreForTest(t)
	req, err := st.CreateMediaRequest(MediaRequest{UID: 1, Source: "tmdb", MediaID: 42, Title: "m", MediaInfo: map[string]any{"title": "m"}})
	if err != nil {
		t.Fatal(err)
	}
	raceReadThenWrite(t, func() any {
		got, _ := st.MediaRequest(req.ID)
		return got
	}, func() {
		if _, err := st.UpdateMediaRequest(req.ID, func(r *MediaRequest) error {
			r.MediaInfo["poster"] = "p.jpg"
			return nil
		}); err != nil {
			t.Error(err)
		}
	})
}
