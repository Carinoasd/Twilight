package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/prejudice-studio/twilight/internal/store"
	"go.uber.org/zap"
)

func (a *App) notifyTicketAdminsEmail(event string, ticket store.Ticket, actor store.User) {
	if !a.cfg().TicketNotifyEmailEnabled || !emailConfigured(a.cfg()) {
		return
	}
	a.enqueueTicketNotification("admin_email", event, ticket.ID, ticket.UID, func(ctx context.Context) ticketNotificationResult {
		result := ticketNotificationResult{}
		seen := map[string]bool{}
		body := stripTelegramHTML(a.ticketAdminNotificationText(event, ticket, actor))
		adminUIDs, _ := a.store().UserUIDsMatching(0, func(u store.User) bool {
			return u.Role == store.RoleAdmin && u.NotifyOnTicketEmail && a.notificationEmailAvailable(u) && !(actor.Role == store.RoleAdmin && actor.UID == u.UID)
		})
		admins := a.store().UsersByUIDs(adminUIDs)
		for _, uid := range adminUIDs {
			u, exists := admins[uid]
			if !exists {
				continue
			}
			if ctx.Err() != nil {
				break
			}
			if u.Role != store.RoleAdmin || !u.NotifyOnTicketEmail || !a.notificationEmailAvailable(u) || (actor.Role == store.RoleAdmin && actor.UID == u.UID) {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(u.Email))
			if seen[key] {
				continue
			}
			seen[key] = true
			result.Targets++
			if err := smtpDeliver(ctx, *a.cfg(), u.Email, fmt.Sprintf("工单更新 #%d", ticket.ID), body); err != nil {
				result.Failures++
				zap.L().Warn("发送管理员工单邮件失败", zap.Int64("ticket_id", ticket.ID), zap.Int64("uid", u.UID), zap.Error(err))
			}
		}
		return result
	})
}

func (a *App) notifyTicketOwnerEmail(updated, existing store.Ticket) {
	if !a.cfg().TicketNotifyEmailEnabled || !emailConfigured(a.cfg()) {
		return
	}
	a.enqueueTicketNotification("owner_email", "updated", updated.ID, updated.UID, func(ctx context.Context) ticketNotificationResult {
		owner, found := a.store().User(updated.UID)
		if !found || !owner.NotifyOnTicketEmail || !a.notificationEmailAvailable(owner) {
			return ticketNotificationResult{Skipped: "owner_email_disabled"}
		}
		// Internal AdminNote is never included in the owner's email.
		body := fmt.Sprintf("工单 #%d\n标题：%s\n状态：%s\n类型：%s", updated.ID, updated.Title, statusLabel(updated.Status), updated.Type)
		if reply := ticketOwnerNotificationNote(updated, existing); reply != "" {
			body += "\n\n回复内容：\n" + reply
		}
		result := ticketNotificationResult{Targets: 1}
		if err := smtpDeliver(ctx, *a.cfg(), owner.Email, fmt.Sprintf("工单更新 #%d", updated.ID), body); err != nil {
			result.Failures = 1
			zap.L().Warn("发送工单邮件失败", zap.Int64("ticket_id", updated.ID), zap.Int64("uid", owner.UID), zap.Error(err))
		}
		return result
	})
}
