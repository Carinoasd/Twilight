package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/prejudice-studio/twilight/internal/store"
)

// Bounded conversations are an explicit opt-in for existing V2 consumers.
// The WebUI always requests message_limit=50. V1 and older V2 callers retain
// their complete historical response instead of silently losing old messages.
func ticketResponseDTO(r *http.Request, ticket store.Ticket, admin bool) map[string]any {
	if !strings.HasPrefix(r.URL.Path, "/api/v2/") {
		return ticketDTO(ticket, admin)
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("message_limit"))
	total := len(ticket.Replies)
	var page store.TicketMessagePage
	if limit > 0 {
		page = store.TicketMessages(ticket, 0, min(limit, 100))
		ticket.Replies = page.Items
	}
	var dto map[string]any
	if admin {
		dto = v2AdminTicketDTO(ticket)
	} else {
		dto = v2UserTicketDTO(ticket)
	}
	dto["reply_count"] = total
	if limit > 0 {
		dto["message_page"] = map[string]any{"has_more": page.HasMore, "next_before": page.NextBefore, "total": total}
	}
	return dto
}

func (a *App) handleV2TicketMessages(w http.ResponseWriter, r *http.Request, params Params) {
	a.handleTicketMessages(w, r, params, false)
}

func (a *App) handleV2AdminTicketMessages(w http.ResponseWriter, r *http.Request, params Params) {
	a.handleTicketMessages(w, r, params, true)
}

func (a *App) handleTicketMessages(w http.ResponseWriter, r *http.Request, params Params, admin bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	if !admin && !a.cfg().TicketSystemEnabled {
		failWithCode(w, http.StatusServiceUnavailable, ErrTicketDisabled, "工单系统未启用")
		return
	}
	id, err := int64Param(params, "ticket_id")
	if err != nil || id <= 0 {
		failWithCode(w, http.StatusBadRequest, ErrInvalidPayload, "无效的工单编号")
		return
	}
	before := int64(0)
	if raw := r.URL.Query().Get("before"); raw != "" {
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 1 {
			failWithCode(w, http.StatusBadRequest, ErrInvalidPayload, "无效的消息游标")
			return
		}
	}
	if a.refreshStoreForRequest(w, r) {
		return
	}
	ticket, found := a.store().Ticket(id)
	if !found || (!admin && ticket.UID != current(r).User.UID) {
		failWithCode(w, http.StatusNotFound, ErrTicketNotFound, "工单不存在")
		return
	}
	page := store.TicketMessages(ticket, before, clamp(queryInt(r, "limit", store.TicketMessagePageSize), 1, 100))
	ok(w, "OK", map[string]any{"items": ticketReplyDTOs(page.Items), "has_more": page.HasMore, "next_before": page.NextBefore, "total": page.Total, "revision": store.TicketRevision(ticket)})
}

func ticketExpectedRevision(w http.ResponseWriter, payload map[string]any) (*int64, bool) {
	raw, exists := payload["expected_revision"]
	if !exists {
		return nil, true
	}
	number, okNumber := raw.(float64)
	if !okNumber || number < 1 || number > 9007199254740991 || math.Trunc(number) != number {
		failWithCode(w, http.StatusBadRequest, ErrInvalidPayload, "无效的工单版本")
		return nil, false
	}
	value := int64(number)
	return &value, true
}

func writeTicketConflict(w http.ResponseWriter, err error) bool {
	if errors.Is(err, store.ErrTicketRevision) {
		failWithCode(w, http.StatusConflict, ErrTicketRevisionConflict, "工单已更新，请刷新后重试")
		return true
	}
	if errors.Is(err, store.ErrTicketReplyKey) {
		failWithCode(w, http.StatusConflict, ErrTicketReplyConflict, "此回复请求已用于其他内容")
		return true
	}
	return false
}

func ticketAttachmentResponseDTO(r *http.Request, id int64, attachment store.TicketAttachment) map[string]any {
	dto := ticketAttachmentDTO(id, attachment)
	if strings.HasPrefix(r.URL.Path, "/api/v2/") {
		dto["url"] = v2UserTicketAttachmentURL(id, attachment.Filename)
	}
	return dto
}

func ticketAttachmentResponseDTOs(r *http.Request, id int64, attachments []store.TicketAttachment) []map[string]any {
	result := make([]map[string]any, 0, len(attachments))
	for _, attachment := range attachments {
		result = append(result, ticketAttachmentResponseDTO(r, id, attachment))
	}
	return result
}

// Attachment responses include a bounded snapshot so an out-of-order attachment
// response cannot advance the revision while retaining stale status/metadata.
func ticketAttachmentMutationSnapshot(r *http.Request, ticket store.Ticket, admin bool) map[string]any {
	if !strings.HasPrefix(r.URL.Path, "/api/v2/") {
		return nil
	}
	copyRequest := r.Clone(r.Context())
	query := copyRequest.URL.Query()
	query.Set("message_limit", "50")
	copyRequest.URL.RawQuery = query.Encode()
	return ticketResponseDTO(copyRequest, ticket, admin)
}
