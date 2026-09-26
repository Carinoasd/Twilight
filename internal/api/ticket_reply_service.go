package api

import (
	"errors"
	"strings"

	"github.com/prejudice-studio/twilight/internal/store"
)

var (
	errTicketReplyForbidden = errors.New("ticket reply forbidden")
	errTicketReplyEmpty     = errors.New("ticket reply empty")
	errTicketReplyTooLong   = errors.New("ticket reply too long")
)

// appendTicketReply is the application operation shared by legacy and V2
// transport handlers. It performs the ownership/status check against the
// latest Store snapshot and appends through the Store's atomic mutation;
// callers remain responsible for rate limits, audit and notifications.
func (a *App) appendTicketReply(ticketID int64, actor store.User, content string) (store.Ticket, store.Ticket, error) {
	result, err := a.appendTicketMessage(ticketID, actor, content, "")
	return result.Ticket, result.Previous, err
}

func (a *App) appendTicketMessage(ticketID int64, actor store.User, content, requestKey string) (store.TicketReplyResult, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return store.TicketReplyResult{}, errTicketReplyEmpty
	}
	if len(content) > store.TicketReplyMaxBytes {
		return store.TicketReplyResult{}, errTicketReplyTooLong
	}
	if existing, found := a.store().Ticket(ticketID); found && actor.Role != store.RoleAdmin && existing.UID != actor.UID {
		return store.TicketReplyResult{}, errTicketReplyForbidden
	}
	cfg := a.cfg()
	return a.store().AppendTicketMessage(ticketID, actor, content, requestKey, cfg.TicketUserOpenLimit, cfg.TicketGlobalOpenLimit)
}
