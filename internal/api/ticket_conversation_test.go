package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

func ticketPostWithKey(app *App, path, content, key string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"content": content})
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Twilight-Client", "webui")
	req.Header.Set("Idempotency-Key", key)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	return rr
}

func TestTicketReplyKeyAndRevisionHTTP(t *testing.T) {
	app := newTestApp(t)
	enableTicketSystem(t, app, nil)
	rt := *app.runtime.Load()
	rt.cfg.AuditLogEnabled = true
	app.runtime.Store(&rt)
	admin := registerAdmin(t, app, "conversation-admin", "Admin123456")
	owner := registerAndLogin(t, app, "conversation-owner", "Owner123456")
	id := createTicket(t, app, "conversation", "opening", owner)
	ticket, _ := app.store().Ticket(id)
	path := "/api/v2/admin/tickets/" + strconv.FormatInt(id, 10)
	key := "ticket-idempotency-key-1234"
	beforeAudit := app.store().QueryAuditLogs(store.AuditLogQuery{}).Total
	for i := range 2 {
		rr := ticketPostWithKey(app, path+"/replies?message_limit=50", "answer", key, admin)
		if rr.Code != http.StatusOK {
			t.Fatalf("reply=%d %s", rr.Code, rr.Body.String())
		}
		var response struct {
			Data struct {
				Replayed bool
				Message  map[string]any
				Ticket   map[string]any
				Item     map[string]any
			}
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Data.Replayed != (i == 1) || response.Data.Message["id"] != float64(1) || response.Data.Item["id"] != response.Data.Ticket["id"] {
			t.Fatalf("reply contract: %+v", response.Data)
		}
	}
	if count := app.store().QueryAuditLogs(store.AuditLogQuery{}).Total; count != beforeAudit+1 {
		t.Fatalf("duplicate/replay fallback audits: before=%d after=%d", beforeAudit, count)
	}
	rr := ticketPostWithKey(app, path+"/replies", "changed content", key, admin)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "TICKET_REPLY_CONFLICT") {
		t.Fatalf("key conflict: %d %s", rr.Code, rr.Body.String())
	}
	rr = doJSON(app, http.MethodPatch, path, fmt.Sprintf(`{"status":"open","expected_revision":%d}`, store.TicketRevision(ticket)), admin)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "TICKET_REVISION_CONFLICT") {
		t.Fatalf("revision: %d %s", rr.Code, rr.Body.String())
	}
	got, _ := app.store().Ticket(id)
	if len(got.Replies) != 1 || got.Status != store.TicketStatusInProgress {
		t.Fatalf("mutated after conflict: %+v", got)
	}
	for _, invalid := range []string{`"1"`, `1.5`, `null`, `0`, `true`} {
		rr = doJSON(app, http.MethodPatch, path, `{"priority":"urgent","expected_revision":`+invalid+`}`, admin)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("accepted revision %s: %d", invalid, rr.Code)
		}
	}
}

func TestTicketMessagePagingCompatibilityAndAuthorization(t *testing.T) {
	app := newTestApp(t)
	enableTicketSystem(t, app, nil)
	admin := registerAdmin(t, app, "paging-admin", "Admin123456")
	owner := registerAndLogin(t, app, "paging-owner", "Owner123456")
	other := registerAndLogin(t, app, "paging-other", "Other123456")
	id := createTicket(t, app, "history", "opening", owner)
	ticket, _ := app.store().Ticket(id)
	for i := range 123 {
		if _, err := app.store().AddTicketReply(id, store.TicketReply{UID: ticket.UID, Role: store.RoleNormal, Content: fmt.Sprintf("reply-%03d", i), CreatedAt: 100}); err != nil {
			t.Fatal(err)
		}
	}
	note := "internal-only-marker"
	if _, err := app.store().UpdateTicket(id, store.TicketUpdate{AdminNote: &note}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/tickets/" + strconv.FormatInt(id, 10)
	for _, test := range []struct {
		suffix string
		want   int
	}{{"", 123}, {"?message_limit=50", 50}, {"?message_limit=100000", 100}} {
		rr := doJSON(app, http.MethodGet, path+test.suffix, "", owner)
		var response struct {
			Data struct {
				Item struct {
					Replies    []store.TicketReply
					ReplyCount int `json:"reply_count"`
				}
			}
		}
		if rr.Code != http.StatusOK {
			t.Fatalf("detail=%d", rr.Code)
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Data.Item.Replies) != test.want || response.Data.Item.ReplyCount != 123 || strings.Contains(rr.Body.String(), note) {
			t.Fatalf("detail contract: %s", rr.Body.String())
		}
		t.Logf("detail suffix=%q bytes=%d replies=%d", test.suffix, rr.Body.Len(), len(response.Data.Item.Replies))
	}
	before := int64(0)
	seen := map[int64]bool{}
	for {
		url := path + "/messages?limit=50"
		if before > 0 {
			url += "&before=" + strconv.FormatInt(before, 10)
		}
		rr := doJSON(app, http.MethodGet, url, "", owner)
		var response struct {
			Data struct {
				Items      []store.TicketReply
				HasMore    bool  `json:"has_more"`
				NextBefore int64 `json:"next_before"`
			}
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if rr.Code != http.StatusOK || len(response.Data.Items) > 50 {
			t.Fatalf("page: %s", rr.Body.String())
		}
		for _, reply := range response.Data.Items {
			if seen[reply.ID] {
				t.Fatalf("duplicate %d", reply.ID)
			}
			seen[reply.ID] = true
		}
		if !response.Data.HasMore {
			break
		}
		before = response.Data.NextBefore
	}
	if len(seen) != 123 {
		t.Fatalf("lost messages: %d", len(seen))
	}
	for _, cookies := range [][]*http.Cookie{other, admin} {
		if rr := doJSON(app, http.MethodGet, path+"/messages", "", cookies); rr.Code != http.StatusNotFound {
			t.Fatalf("user resource expanded access: %d", rr.Code)
		}
	}
	if rr := doJSON(app, http.MethodGet, "/api/v2/admin/tickets/"+strconv.FormatInt(id, 10)+"/messages", "", other); rr.Code != http.StatusForbidden {
		t.Fatalf("admin resource: %d", rr.Code)
	}
}

func TestTicketAttachmentPrivateCacheAndBooleanValidation(t *testing.T) {
	app := newTestApp(t)
	enableTicketSystem(t, app, nil)
	owner := registerAndLogin(t, app, "cache-owner", "Owner123456")
	id := createTicket(t, app, "attachment", "opening", owner)
	upload := uploadTicketImage(t, app, id, "a.png", pngBytes(), owner)
	if upload.Code != http.StatusCreated {
		t.Fatal(upload.Body.String())
	}
	var uploaded struct {
		Data struct{ Attachment struct{ Filename string } }
	}
	if err := json.Unmarshal(upload.Body.Bytes(), &uploaded); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/tickets/" + strconv.FormatInt(id, 10)
	rr := doJSON(app, http.MethodGet, path+"/attachments/"+uploaded.Data.Attachment.Filename, "", owner)
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "private, no-store" || rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("image status=%d headers=%v", rr.Code, rr.Header())
	}
	for _, value := range []string{`"false"`, `1`, `null`} {
		rr = doJSON(app, http.MethodPut, path+"/notify-telegram", `{"enabled":`+value+`}`, owner)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("accepted boolean %s", value)
		}
	}
	if rr := doJSON(app, http.MethodPost, "/api/v2/tickets", `{"title":"bad","content":"bad","notify_telegram":"false"}`, owner); rr.Code != http.StatusBadRequest {
		t.Fatalf("create boolean=%d", rr.Code)
	}
}

func TestTicketV2AttachmentResponseCarriesBoundedCurrentSnapshot(t *testing.T) {
	app := newTestApp(t)
	enableTicketSystem(t, app, nil)
	owner := registerAndLogin(t, app, "attachment-snapshot", "Owner123456")
	id := createTicket(t, app, "snapshot", "opening", owner)
	ticket, _ := app.store().Ticket(id)
	for range 53 {
		if _, err := app.store().AddTicketReply(id, store.TicketReply{UID: ticket.UID, Role: store.RoleNormal, Content: "history"}); err != nil {
			t.Fatal(err)
		}
	}
	note := "private attachment test note"
	if _, err := app.store().UpdateTicket(id, store.TicketUpdate{AdminNote: &note}); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "test.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(pngBytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/tickets/" + strconv.FormatInt(id, 10) + "/attachments"
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Twilight-Client", "webui")
	for _, cookie := range owner {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	app.ServeHTTP(rr, req)
	var result struct {
		Data struct {
			Revision int64
			Ticket   struct {
				Revision   int64
				Replies    []store.TicketReply
				ReplyCount int `json:"reply_count"`
			}
			Attachment struct {
				Filename  string
				URL       string
				CreatedAt int64 `json:"created_at"`
			}
		}
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusCreated || result.Data.Revision != result.Data.Ticket.Revision || len(result.Data.Ticket.Replies) != 50 || result.Data.Ticket.ReplyCount != 53 || result.Data.Attachment.CreatedAt == 0 || !strings.HasPrefix(result.Data.Attachment.URL, path+"/") || strings.Contains(rr.Body.String(), note) {
		t.Fatalf("upload contract: %d %s", rr.Code, rr.Body.String())
	}
	deleted := doJSON(app, http.MethodDelete, path+"/"+result.Data.Attachment.Filename, "", owner)
	var updated struct {
		Data struct {
			Revision    int64
			Ticket      store.Ticket
			Attachments []store.TicketAttachment
		}
	}
	if err := json.Unmarshal(deleted.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if deleted.Code != http.StatusOK || updated.Data.Revision <= result.Data.Revision || updated.Data.Ticket.Revision != updated.Data.Revision || len(updated.Data.Attachments) != 0 || len(updated.Data.Ticket.Replies) != 50 {
		t.Fatalf("delete contract: %d %s", deleted.Code, deleted.Body.String())
	}
}
