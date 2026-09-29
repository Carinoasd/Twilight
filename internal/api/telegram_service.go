package api

import (
	"context"
	"errors"
	"go.uber.org/zap"
	"strings"

	"github.com/prejudice-studio/twilight/internal/store"
)

type telegramService struct {
	app *App
}

type telegramStatusResult struct {
	Bound                bool   `json:"bound"`
	TelegramID           any    `json:"telegram_id"`
	TelegramIDFull       any    `json:"telegram_id_full"`
	TelegramUsername     string `json:"telegram_username"`
	ForceBind            bool   `json:"force_bind"`
	CanUnbind            bool   `json:"can_unbind"`
	CanChange            bool   `json:"can_change"`
	RebindApproved       bool   `json:"rebind_approved"`
	PendingRebindRequest bool   `json:"pending_rebind_request"`
	RebindRequestStatus  any    `json:"rebind_request_status"`
	RebindRequestID      any    `json:"rebind_request_id"`
	RebindingInProgress  bool   `json:"rebinding_in_progress"`
}

type telegramUnbindResult struct {
	User    *store.User `json:"user"`
	Message string      `json:"message"`
	// EmbySuspended / EmbySuspendError 描述解绑后停用 Emby 的结果，供审计与响应使用。
	EmbySuspended    bool  `json:"-"`
	EmbySuspendError error `json:"-"`
}

type telegramRosterStatsResult struct {
	Total     int  `json:"total"`
	Active    int  `json:"active"`
	Bound     int  `json:"bound"`
	Unbound   int  `json:"unbound"`
	KnownOnly bool `json:"known_only"`
}

func (s *telegramService) status(u store.User) telegramStatusResult {
	forceBind := s.app.cfg().ForceBindTelegram
	admin := u.Role == store.RoleAdmin
	canUnbind := admin
	canChange := true
	pendingRebind := false
	rebindApproved := false
	var rebindStatus any
	var rebindID any

	if latestReq, hasReq := s.app.store().UserLatestRebindRequest(u.UID); hasReq {
		rebindStatus = latestReq.Status
		rebindID = latestReq.ID
		switch latestReq.Status {
		case "pending":
			pendingRebind = true
			if !admin {
				canChange = false
			}
		case "approved":
			rebindApproved = u.TelegramID != 0 && latestReq.OldTelegramID == u.TelegramID
			canUnbind = admin || rebindApproved
		}
	}

	return telegramStatusResult{
		Bound:                u.TelegramID != 0,
		TelegramID:           nullableInt(u.TelegramID),
		TelegramIDFull:       nullableInt(u.TelegramID),
		TelegramUsername:     u.TelegramUsername,
		ForceBind:            forceBind,
		CanUnbind:            canUnbind,
		CanChange:            canChange,
		RebindApproved:       rebindApproved,
		PendingRebindRequest: pendingRebind,
		RebindRequestStatus:  rebindStatus,
		RebindRequestID:      rebindID,
		RebindingInProgress:  u.RebindingInProgress,
	}
}

func (s *telegramService) unbind(ctx context.Context, u store.User) (telegramUnbindResult, error) {
	updated, err := s.app.store().UnbindUserTelegram(u.UID, u.TelegramID)
	if err != nil {
		return telegramUnbindResult{}, err
	}
	s.app.cleanupUserTelegramResidue(u.UID, u.TelegramID)
	result := telegramUnbindResult{Message: "Telegram unbound. rebinding required"}
	result.EmbySuspended, result.EmbySuspendError = s.app.suspendEmbyForTelegramRebind(ctx, updated)
	if latest, ok := s.app.store().User(updated.UID); ok {
		updated = latest
	}
	result.User = &updated
	return result, nil
}

func (s *telegramService) rosterStats() (telegramRosterStatsResult, error) {
	chats := telegramChatIDs(s.app.cfg().TelegramGroupIDs)
	chatID := ""
	if len(chats) > 0 {
		chatID = chats[0]
	}

	stats, err := s.app.store().TelegramRosterStats(chatID)
	if err != nil {
		return telegramRosterStatsResult{}, err
	}

	entries, err := s.app.store().TelegramRoster(chatID, true)
	if err != nil {
		return telegramRosterStatsResult{}, err
	}

	bound := 0
	unbound := 0

	if len(entries) > 0 {
		// Resolve the whole roster in one indexed lookup instead of one
		// FindUserByTelegramID call per member (N round trips through the store
		// lock for a list that can hold thousands of entries).
		ids := make([]int64, 0, len(entries))
		for _, entry := range entries {
			if entry.IsBot || entry.TelegramID == 0 {
				continue
			}
			ids = append(ids, entry.TelegramID)
		}
		boundUsers := s.app.store().UsersByTelegramIDs(ids)
		for _, entry := range entries {
			if entry.IsBot || entry.TelegramID == 0 {
				continue
			}
			if _, okUser := boundUsers[entry.TelegramID]; okUser {
				bound++
			} else {
				unbound++
			}
		}
	} else {
		for _, u := range s.app.store().ListUsers() {
			if u.TelegramID == 0 {
				continue
			}
			bound++
		}
		stats["total"] = bound
		stats["active"] = bound
		stats["known_only"] = true
	}

	stats["bound"] = bound
	stats["unbound"] = unbound

	return telegramRosterStatsResult{
		Total:     int(numeric(stats["total"])),
		Active:    int(numeric(stats["active"])),
		Bound:     bound,
		Unbound:   unbound,
		KnownOnly: boolish(stats["known_only"]),
	}, nil
}

func (a *App) telegram() *telegramService {
	return &telegramService{app: a}
}

// suspendEmbyForTelegramRebind 在解绑进入换绑状态后立即停用远端 Emby。解绑本身已经
// 提交，远端失败不回滚解绑（否则用户会卡在“已批准但解不掉”）；失败会写日志并由
// 调用方记入审计，换绑期间 embyShouldEnableUser 仍会挡住所有启用路径。
func (a *App) suspendEmbyForTelegramRebind(ctx context.Context, u store.User) (bool, error) {
	if !u.RebindingInProgress || strings.TrimSpace(u.EmbyID) == "" || !a.embyConfigured() {
		return false, nil
	}
	sideCtx, sideCancel := schedulerSideEffectContext(ctx)
	defer sideCancel()
	var suspended bool
	err := embyRetryOn5xx(sideCtx, func(ctx context.Context) error {
		var err error
		suspended, err = a.disableRemoteEmbyForWebState(ctx, u)
		return err
	})
	if err != nil {
		zap.L().Warn("suspend emby for telegram rebind failed", zap.Int64("uid", u.UID), zap.Error(err))
	}
	return suspended, err
}

// finishTelegramRebind 结束换绑：核对群组/频道资格（checkMembership=false 表示调用方
// 刚做过）、清除换绑状态，并只对“本次换绑停用的 Emby”恢复启用。
func (a *App) finishTelegramRebind(ctx context.Context, u store.User, checkMembership bool) (store.User, bool, error) {
	if !u.RebindingInProgress {
		return u, false, nil
	}
	if u.TelegramID == 0 {
		return u, false, errTelegramRebindNotBound
	}
	if checkMembership {
		missing, err := a.telegramBindRequirementMissing(ctx, u.TelegramID)
		if err != nil {
			return u, false, errTelegramRebindMembershipCheck
		}
		if len(missing) > 0 {
			return u, false, telegramRebindMissingError{missing: missing}
		}
	}
	updated, changed, embySuspended, err := a.store().CompleteUserTelegramRebind(u.UID, u.TelegramID, u.RebindingSince)
	if err != nil || !changed {
		return updated, changed, err
	}
	if embySuspended && updated.EmbyID != "" && a.embyShouldEnableUser(updated) {
		sideCtx, sideCancel := schedulerSideEffectContext(ctx)
		if err := embyRetryOn5xx(sideCtx, func(ctx context.Context) error {
			return a.embyApplyEnabledState(ctx, updated.UID, updated.EmbyID, true)
		}); err != nil {
			zap.L().Warn("re-enable emby after telegram rebind failed", zap.Int64("uid", updated.UID), zap.Error(err))
		}
		sideCancel()
		if latest, ok := a.store().User(updated.UID); ok {
			updated = latest
		}
	}
	return updated, true, nil
}

var (
	errTelegramRebindNotBound        = errors.New("telegram rebind: not bound")
	errTelegramRebindMembershipCheck = errors.New("telegram rebind: membership check failed")
)

type telegramRebindMissingError struct{ missing []string }

func (e telegramRebindMissingError) Error() string {
	return "telegram rebind: missing membership " + strings.Join(e.missing, ",")
}
