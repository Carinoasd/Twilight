package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/prejudice-studio/twilight/internal/migration"
)

func conversationFixture(t *testing.T) (*Store, User, Ticket) {
	t.Helper()
	st := newJSONStoreForTest(t)
	u, err := st.CreateUser(User{Username: "ticket-owner", Role: RoleNormal, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := st.CreateTicket(Ticket{UID: u.UID, Username: u.Username, Title: "Historical ticket", Content: "Opening"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return st, u, ticket
}

func TestTicketReplyIdempotencyAcrossStores(t *testing.T) {
	st, u, ticket := conversationFixture(t)
	other := reopenTestStore(t)
	start := make(chan struct{})
	results := make(chan TicketReplyResult, 2)
	errs := make(chan error, 2)
	for _, writer := range []*Store{st, other} {
		go func(writer *Store) {
			<-start
			result, err := writer.AppendTicketMessage(ticket.ID, u, "same reply", "same-request-key-1234")
			results <- result
			errs <- err
		}(writer)
	}
	close(start)
	replayed := 0
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		result := <-results
		if result.Replayed {
			replayed++
		}
		if result.Reply.ID != 1 {
			t.Fatalf("unstable ID: %d", result.Reply.ID)
		}
	}
	if replayed != 1 {
		t.Fatalf("replayed=%d", replayed)
	}
	if _, err := other.AppendTicketMessage(ticket.ID, u, "different", "same-request-key-1234"); !errors.Is(err, ErrTicketReplyKey) {
		t.Fatalf("conflict: %v", err)
	}
	closed := TicketStatusClosed
	if _, err := st.UpdateTicket(ticket.ID, TicketUpdate{Status: &closed}); err != nil {
		t.Fatal(err)
	}
	result, err := other.AppendTicketMessage(ticket.ID, u, "same reply", "same-request-key-1234")
	if err != nil || !result.Replayed || len(result.Ticket.Replies) != 1 {
		t.Fatalf("closed replay=%+v err=%v", result, err)
	}
	if _, err := other.AppendTicketMessage(ticket.ID, u, "new reply", "new-request-key-1234"); !errors.Is(err, ErrTicketClosed) {
		t.Fatalf("closed: %v", err)
	}
}

func TestTicketRevisionRejectsStaleMetadataAndPreservesReply(t *testing.T) {
	st, u, ticket := conversationFixture(t)
	other := reopenTestStore(t)
	revision := TicketRevision(ticket)
	result, err := st.AppendTicketMessage(ticket.ID, u, "message", "")
	if err != nil {
		t.Fatal(err)
	}
	note := "internal"
	if _, err = other.UpdateTicket(ticket.ID, TicketUpdate{ExpectedRevision: &revision, AdminNote: &note}); !errors.Is(err, ErrTicketRevision) {
		t.Fatalf("stale patch: %v", err)
	}
	revision = TicketRevision(result.Ticket)
	updated, err := other.UpdateTicket(ticket.ID, TicketUpdate{ExpectedRevision: &revision, AdminNote: &note})
	if err != nil || len(updated.Replies) != 1 || updated.AdminNote != note {
		t.Fatalf("update=%+v err=%v", updated, err)
	}
}

func TestTicketAttachmentQuotaAcrossStores(t *testing.T) {
	st, u, ticket := conversationFixture(t)
	other := reopenTestStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	start := make(chan struct{})
	for i := range 6 {
		writer := []*Store{st, other}[i%2]
		wg.Add(1)
		go func(i int, writer *Store) {
			defer wg.Done()
			<-start
			_, err := writer.CommitTicketAttachment(ticket.ID, TicketAttachment{Filename: fmt.Sprintf("%016x.png", i), UploadedUID: u.UID}, u, 2)
			errs <- err
		}(i, writer)
	}
	close(start)
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrTicketAttachmentLimit) {
			t.Fatal(err)
		}
	}
	if success != 2 {
		t.Fatalf("committed %d attachments", success)
	}
	if err := st.Refresh(); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Ticket(ticket.ID)
	if len(got.Attachments) != 2 {
		t.Fatalf("attachments=%d", len(got.Attachments))
	}
}

func TestTicketHistoricalMessagesAndArchiveRoundTrip(t *testing.T) {
	st, _, ticket := conversationFixture(t)
	// Legacy records intentionally have no revision, message IDs, or role for
	// the first message. Preserve order even when timestamps run backwards.
	var legacy Ticket
	if err := json.Unmarshal([]byte(`{"id":1,"uid":1,"title":"old","content":"opening","admin_note":"private legacy note","replies":[{"uid":99,"content":"unknown author","created_at":20},{"uid":1,"role":1,"content":"owner","created_at":10}],"attachments":[{"filename":"0000000000000001.png","created_at":8}],"notify_telegram":false}`), &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.ID, legacy.UID = ticket.ID, ticket.UID
	st.mu.Lock()
	err := st.mutateAndSaveLocked(func() error { st.state.Tickets[ticket.ID] = legacy; return nil })
	st.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	page := TicketMessages(legacy, 0, 1)
	if page.Items[0].ID != 2 || !page.HasMore || page.NextBefore != 2 {
		t.Fatalf("latest: %+v", page)
	}
	older := TicketMessages(legacy, page.NextBefore, 1)
	if older.Items[0].Role != RoleUnrecognized || older.Items[0].ID != 1 || older.HasMore {
		t.Fatalf("older: %+v", older)
	}
	ctx := context.Background()
	files, err := st.ExportMigrationFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	archiveBytes, _, err := migration.Create(migration.Input{TwilightVersion: "test", DatabaseSchemaVersion: "postgres-state-v1", Files: files})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := migration.Open(archiveBytes, "", migration.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := st.ImportMigrationArchive(ctx, archive); err != nil {
			t.Fatal(err)
		}
		got, _ := st.Ticket(ticket.ID)
		if !reflect.DeepEqual(got, legacy) {
			t.Fatalf("historical data changed: got=%+v want=%+v", got, legacy)
		}
		if !reflect.DeepEqual(TicketMessages(got, 0, 1), page) {
			t.Fatal("message IDs/order changed after import")
		}
	}
}

func TestTicketReplyRejectsRemovedOrDemotedActor(t *testing.T) {
	st, u, ticket := conversationFixture(t)
	admin, err := st.CreateUser(User{Username: "was-admin", Role: RoleAdmin, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	err = st.mutateAndSaveLocked(func() error {
		current := st.state.Users[admin.UID]
		current.Role = RoleNormal
		st.state.Users[admin.UID] = current
		return nil
	})
	st.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTicketMessage(ticket.ID, admin, "cannot post", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("demoted actor: %v", err)
	}
	if _, err := st.AppendTicketMessage(ticket.ID, u, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty: %v", err)
	}
}

func TestTicketResolvedReplyCannotBypassOpenQuota(t *testing.T) {
	st, owner, ticket := conversationFixture(t)
	resolved := TicketStatusResolved
	if _, err := st.UpdateTicket(ticket.ID, TicketUpdate{Status: &resolved}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTicket(Ticket{UID: owner.UID, Title: "another", Content: "open"}, 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTicketMessage(ticket.ID, owner, "reopen via reply", "", 1, 0); !errors.Is(err, ErrTicketUserOpenLimit) {
		t.Fatalf("user quota: %v", err)
	}
	if _, err := st.AppendTicketMessage(ticket.ID, owner, "reopen via reply", "", 0, 1); !errors.Is(err, ErrTicketGlobalOpenLimit) {
		t.Fatalf("global quota: %v", err)
	}
	got, _ := st.Ticket(ticket.ID)
	if got.Status != resolved || len(got.Replies) != 0 {
		t.Fatalf("quota failure mutated ticket: %+v", got)
	}
}

func TestTicketCleanupRechecksRevisionAndClosedState(t *testing.T) {
	st, owner, ticket := conversationFixture(t)
	admin, err := st.CreateUser(User{Username: "cleanup-admin", Role: RoleAdmin, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	ticket, err = st.CommitTicketAttachment(ticket.ID, TicketAttachment{Filename: "0000000000000001.png"}, owner, 5)
	if err != nil {
		t.Fatal(err)
	}
	closed := TicketStatusClosed
	ticket, err = st.UpdateTicket(ticket.ID, TicketUpdate{Status: &closed})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := ticket.ClosedAt + 1
	inspectedRevision := TicketRevision(ticket)
	if _, err := st.ReopenTicket(ticket.ID, owner.UID, 0, 0); err != nil {
		t.Fatal(err)
	}
	removed, err := st.DetachExpiredTicketAttachments(ticket.ID, inspectedRevision, cutoff)
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed reopened evidence: %v %v", removed, err)
	}
	ticket, err = st.UpdateTicket(ticket.ID, TicketUpdate{Status: &closed})
	if err != nil {
		t.Fatal(err)
	}
	inspectedRevision = TicketRevision(ticket)
	ticket, err = st.CommitTicketAttachment(ticket.ID, TicketAttachment{Filename: "0000000000000002.png"}, admin, 5)
	if err != nil {
		t.Fatal(err)
	}
	removed, err = st.DetachExpiredTicketAttachments(ticket.ID, inspectedRevision, ticket.ClosedAt+1)
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed new attachment: %v %v", removed, err)
	}
	removed, err = st.DetachExpiredTicketAttachments(ticket.ID, TicketRevision(ticket), ticket.ClosedAt+1)
	if err != nil || len(removed) != 2 {
		t.Fatalf("cleanup: %v %v", removed, err)
	}
}

func TestTicketMessageIdempotencyAndNotificationTriStateSurviveArchive(t *testing.T) {
	st, owner, ticket := conversationFixture(t)
	first, err := st.AppendTicketMessage(ticket.ID, owner, "persisted reply", "archive-request-key-1234")
	if err != nil {
		t.Fatal(err)
	}
	for _, preference := range []*bool{nil, ptrTicketBool(false), ptrTicketBool(true)} {
		st.mu.Lock()
		err = st.mutateAndSaveLocked(func() error {
			ticket := st.state.Tickets[ticket.ID]
			ticket.NotifyTelegram = preference
			st.state.Tickets[ticket.ID] = ticket
			return nil
		})
		st.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		files, err := st.ExportMigrationFiles(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		archiveBytes, _, err := migration.Create(migration.Input{TwilightVersion: "test", DatabaseSchemaVersion: "postgres-state-v1", Files: files})
		if err != nil {
			t.Fatal(err)
		}
		archive, err := migration.Open(archiveBytes, "", migration.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ImportMigrationArchive(context.Background(), archive); err != nil {
			t.Fatal(err)
		}
		result, err := st.AppendTicketMessage(ticket.ID, owner, "persisted reply", "archive-request-key-1234")
		if err != nil || !result.Replayed || result.Reply.ID != first.Reply.ID || len(result.Ticket.Replies) != 1 || !reflect.DeepEqual(result.Ticket.NotifyTelegram, preference) {
			t.Fatalf("roundtrip=%+v err=%v", result, err)
		}
	}
}

func ptrTicketBool(value bool) *bool { return &value }
