package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const TicketReplyMaxBytes = 5000
const TicketMessagePageSize = 50

var (
	ErrTicketRevision        = errors.New("ticket revision conflict")
	ErrTicketReplyKey        = errors.New("ticket reply idempotency conflict")
	ErrTicketAttachmentLimit = errors.New("ticket attachment limit reached")
)

// TicketRevision projects old snapshots without rewriting historical data.
func TicketRevision(t Ticket) int64 {
	if t.Revision < 1 {
		return 1
	}
	return t.Revision
}

func bumpTicketRevision(t *Ticket) { t.Revision = TicketRevision(*t) + 1 }

type TicketMessagePage struct {
	Items      []TicketReply `json:"items"`
	HasMore    bool          `json:"has_more"`
	NextBefore int64         `json:"next_before"`
	Total      int           `json:"total"`
}

// TicketMessages uses append order, never timestamps or account lookups. Legacy
// replies therefore keep a stable ticket-local ID even after import/restart.
// AdminNote and ticket-level attachments are deliberately outside this timeline.
func TicketMessages(t Ticket, before int64, limit int) TicketMessagePage {
	if limit < 1 || limit > 100 {
		limit = TicketMessagePageSize
	}
	end := len(t.Replies)
	if before > 0 && before <= int64(end) {
		end = int(before - 1)
	}
	start := max(0, end-limit)
	page := TicketMessagePage{Items: make([]TicketReply, 0, end-start), HasMore: start > 0, Total: len(t.Replies)}
	if start > 0 {
		page.NextBefore = int64(start + 1)
	}
	for i := start; i < end; i++ {
		reply := t.Replies[i]
		reply.ID = int64(i + 1)
		reply.RequestKeyHash = ""
		page.Items = append(page.Items, reply)
	}
	return page
}

type TicketReplyResult struct {
	Ticket   Ticket
	Previous Ticket
	Reply    TicketReply
	Replayed bool
}

// AppendTicketMessage owns authorization, validation, deduplication and status
// transitions in the version-guarded state mutation. A database retry repeats
// every check against the latest state, including ownership and closed status.
func (s *Store) AppendTicketMessage(ticketID int64, actor User, content, requestKey string, openLimits ...int) (TicketReplyResult, error) {
	content = strings.TrimSpace(content)
	if content == "" || len(content) > TicketReplyMaxBytes || !validTicketRequestKey(requestKey) {
		return TicketReplyResult{}, ErrInvalid
	}
	keyHash := ""
	if requestKey != "" {
		digest := sha256.Sum256([]byte(requestKey))
		keyHash = hex.EncodeToString(digest[:])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result TicketReplyResult
	err := s.mutateAndSaveLocked(func() error {
		result = TicketReplyResult{}
		t, ok := s.state.Tickets[ticketID]
		if !ok || (actor.Role != RoleAdmin && t.UID != actor.UID) {
			return ErrNotFound
		}
		// Re-check the actor when available; deleted/demoted principals cannot
		// use an earlier HTTP authorization snapshot to post as an administrator.
		user, ok := s.state.Users[actor.UID]
		if !ok || !user.Active {
			return ErrNotFound
		}
		if user.Role != RoleAdmin && t.UID != user.UID {
			return ErrNotFound
		}
		result.Previous = t
		if keyHash != "" {
			for i, reply := range t.Replies {
				if reply.UID != user.UID || reply.RequestKeyHash != keyHash {
					continue
				}
				if reply.Content != content {
					return ErrTicketReplyKey
				}
				reply.ID = int64(i + 1)
				result.Ticket, result.Reply, result.Replayed = t, reply, true
				// 幂等重放：state 没有任何改动，跳过整份落盘。
				return errNoChange
			}
		}
		if !TicketStatusAllowsConversation(t.Status) && user.Role != RoleAdmin {
			return ErrTicketClosed
		}
		if user.Role != RoleAdmin && NormalizeTicketStatus(t.Status) == TicketStatusResolved {
			if len(openLimits) > 0 && openLimits[0] > 0 && s.countOpenTicketsLocked(t.UID) >= openLimits[0] {
				return ErrTicketUserOpenLimit
			}
			if len(openLimits) > 1 && openLimits[1] > 0 && s.countOpenTicketsLocked(0) >= openLimits[1] {
				return ErrTicketGlobalOpenLimit
			}
		}
		now := time.Now().Unix()
		reply := TicketReply{ID: int64(len(t.Replies) + 1), UID: user.UID, Username: user.Username, Role: user.Role, Content: content, CreatedAt: now, RequestKeyHash: keyHash}
		applyTicketReplyLocked(&t, reply, now)
		t.UpdatedAt = now
		bumpTicketRevision(&t)
		s.state.Tickets[ticketID] = t
		result.Ticket, result.Reply = t, reply
		return nil
	})
	if err != nil {
		return TicketReplyResult{}, err
	}
	// 回传的工单副本深拷贝，调用方在锁外使用时不与 s.state 共用底层数组。
	result.Ticket = cloneTicket(result.Ticket)
	result.Previous = cloneTicket(result.Previous)
	return result, nil
}

func validTicketRequestKey(key string) bool {
	if key == "" {
		return true
	} // Existing V1/V2 clients remain compatible.
	if len(key) < 16 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// Missing author-role evidence in old JSON must not become RoleAdmin (zero).
func (r *TicketReply) UnmarshalJSON(data []byte) error {
	type plain TicketReply
	value := plain{Role: RoleUnrecognized}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*r = TicketReply(value)
	return nil
}

// DetachExpiredTicketAttachments claims exactly the inspected version. A reopen,
// new admin attachment, or any other intervening write makes cleanup a no-op.
// Files are removed only after this transaction succeeds, by exact filename.
func (s *Store) DetachExpiredTicketAttachments(id, revision, cutoff int64) ([]TicketAttachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []TicketAttachment
	err := s.mutateAndSaveLocked(func() error {
		removed = nil
		ticket, exists := s.state.Tickets[id]
		if !exists || TicketRevision(ticket) != revision || NormalizeTicketStatus(ticket.Status) != TicketStatusClosed || ticket.ClosedAt <= 0 || ticket.ClosedAt >= cutoff {
			// 条件不符即无事可做：返回 errNoChange 跳过整份落盘（不递增 version）。
			return errNoChange
		}
		removed = append([]TicketAttachment(nil), ticket.Attachments...)
		if len(removed) == 0 {
			return errNoChange
		}
		ticket.Attachments = nil
		ticket.UpdatedAt = time.Now().Unix()
		bumpTicketRevision(&ticket)
		s.state.Tickets[id] = ticket
		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}
