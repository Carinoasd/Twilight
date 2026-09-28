package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	"github.com/prejudice-studio/twilight/internal/security"
	"github.com/prejudice-studio/twilight/internal/store"
)

// Telegram 绑定链接（deep link）流程
//
//	网页  POST …/telegram/link            → 签发 {link_id, start_token, deep_link, link_secret?}
//	用户  打开 https://t.me/<bot>?start=<start_token>（或手动发送 /bind <start_token>）
//	Bot   /start <token>                  → confirmTelegramLink：加群校验 → 写库
//	网页  GET  …/telegram/link/:id/status  → 轮询，直到 confirmed / 终态失败
//	网页  注册场景再把 link_id + link_secret 一起提交注册；已登录场景 Bot 确认时已直接绑定。
//
// 注册场景的所有权凭据是 link_secret：签发时随 JSON 返回，浏览器在状态查询
// 用 X-Telegram-Link-Secret 头、在注册提交用 telegram_link_secret 字段带回。
// 不依赖 Cookie，因此与前后端是否跨站无关。已登录场景直接归属当前 UID。
//
// 所有进程都只读写 PostgreSQL 里的链接表；没有进程内 hub、长轮询或 WebSocket。

const (
	telegramLinkSecretHeader = "X-Telegram-Link-Secret"
	telegramLinkTTLSeconds   = int64(store.TelegramLinkTTL / time.Second)
	telegramLinkPollSeconds  = 3
	// 加群/频道校验总预算：给 Bot 留出回复用户的时间。
	telegramBindMembershipTimeout = 20 * time.Second
)

var (
	// telegramLinkTokenPattern 是 Bot 侧可接受的 token 形状（/bind、/start 参数）。
	telegramLinkTokenPattern = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
	// telegramLinkIDPattern 是网页侧资源 ID / 签发 token 的精确形状。
	telegramLinkIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// ---- 签发 ----

func (a *App) handleRegisterTelegramLink(w http.ResponseWriter, r *http.Request, _ Params) {
	if !requireWebUIIntent(w, r, twilightIntentCreateTelegramLink) {
		return
	}
	if !a.telegramAvailable() {
		failWithCode(w, http.StatusServiceUnavailable, ErrTGNotConfigured, "Telegram Bot 未配置")
		return
	}
	if !a.allowRate(r.Context(), rateKey("register-telegram-link:", a.clientIP(r)), a.cfg().RateLimitRegisterPer10m, 10*time.Minute) {
		failWithCode(w, http.StatusTooManyRequests, ErrBindCodeRateLimited, "绑定请求过于频繁")
		return
	}
	a.issueTelegramLink(w, r, 0, "register")
}

func (a *App) handleUserTelegramLink(w http.ResponseWriter, r *http.Request, _ Params) {
	if !requireWebUIIntent(w, r, twilightIntentCreateTelegramLink) {
		return
	}
	if !a.telegramAvailable() {
		failWithCode(w, http.StatusServiceUnavailable, ErrTGNotConfigured, "Telegram Bot 未配置")
		return
	}
	u := current(r).User
	if u.TelegramID != 0 {
		failWithCode(w, http.StatusConflict, ErrTGAlreadyBound, "当前账号已绑定 Telegram，请先完成换绑审批和解绑")
		return
	}
	if !a.allowRate(r.Context(), rateKey("user-telegram-link:", u.UID), a.cfg().RateLimitLoginPerMinute, time.Minute) {
		failWithCode(w, http.StatusTooManyRequests, ErrBindCodeRateLimited, "绑定请求过于频繁")
		return
	}
	a.issueTelegramLink(w, r, u.UID, "user")
}

func (a *App) issueTelegramLink(w http.ResponseWriter, r *http.Request, uid int64, scene string) {
	w.Header().Set("Cache-Control", "no-store, private")
	id, e1 := security.RandomHex(16)
	token, e2 := security.RandomHex(16)
	secret := ""
	var e3 error
	if scene == "register" {
		secret, e3 = security.RandomHex(32)
	}
	if e1 != nil || e2 != nil || e3 != nil {
		failWithCode(w, http.StatusInternalServerError, ErrBindCodeSaveFailed, "绑定链接生成失败")
		return
	}
	now := time.Now().Unix()
	link := store.TelegramLink{ID: id, StartHash: store.TelegramLinkStartHash(token), SecretHash: store.TelegramLinkSecretHash(secret), Scene: scene, UID: uid, CreatedAt: now, ExpiresAt: now + telegramLinkTTLSeconds}
	if err := a.store().CreateTelegramLink(r.Context(), link); err != nil {
		switch {
		case errors.Is(err, store.ErrTelegramLinkCapacity):
			failWithCode(w, http.StatusTooManyRequests, ErrRateLimited, "绑定请求过多，请稍后重试")
		case errors.Is(err, store.ErrTelegramAlreadyBound):
			failWithCode(w, http.StatusConflict, ErrTGAlreadyBound, "当前账号已绑定 Telegram")
		default:
			logTelegramLinkFailure("issue", err)
			failWithCode(w, http.StatusServiceUnavailable, ErrBindCodeSaveFailed, "绑定服务暂不可用，请稍后重试")
		}
		return
	}
	deepLink, botUsername := a.telegramDeepLink(r.Context(), token)
	data := map[string]any{
		"link_id":        id,
		"start_token":    token,
		"deep_link":      deepLink,
		"bot_username":   botUsername,
		"manual_command": "/bind " + token,
		"expires_in":     telegramLinkTTLSeconds,
		"poll_interval":  telegramLinkPollSeconds,
	}
	if scene == "register" {
		data["link_secret"] = secret
	}
	ok(w, "OK", data)
}

// telegramDeepLink 用 getMe 缓存的 Bot 用户名拼 t.me 深链接；Bot 身份暂不可用时
// 返回空串，网页退回“手动发送 /bind <token>”。
func (a *App) telegramDeepLink(ctx context.Context, token string) (string, string) {
	info := a.publicTelegramBotInfo(ctx)
	username, _ := info["username"].(string)
	if username == "" || !telegramPublicUsernamePattern.MatchString(username) {
		return "", ""
	}
	return "https://t.me/" + username + "?start=" + token, username
}

// ---- 状态查询 ----

type telegramLinkState struct {
	ID               string
	Status           string
	ErrorCode        ErrCode
	HTTPStatus       int
	Message          string
	Link             store.TelegramLink
	Confirmed        bool
	Invalid          bool
	Terminal         bool
	Retryable        bool
	ExpiresIn        int64
	TelegramID       int64
	TelegramUsername string
	TelegramBound    bool
}

func (s telegramLinkState) response() map[string]any {
	data := map[string]any{
		"link_id":        s.ID,
		"status":         s.Status,
		"confirmed":      s.Confirmed,
		"invalid":        s.Invalid,
		"terminal":       s.Terminal,
		"retryable":      s.Retryable,
		"telegram_bound": s.TelegramBound,
		"message":        s.Message,
		"poll_interval":  telegramLinkPollSeconds,
	}
	if s.ErrorCode != "" {
		data["error_code"] = s.ErrorCode
	}
	if s.ExpiresIn > 0 {
		data["expires_in"] = s.ExpiresIn
	}
	if s.TelegramID != 0 {
		data["telegram_id"] = s.TelegramID
	}
	if strings.TrimSpace(s.TelegramUsername) != "" {
		data["telegram_username"] = s.TelegramUsername
	}
	return data
}

func telegramLinkTerminal(status string, ec ErrCode, message string) telegramLinkState {
	return telegramLinkState{Status: status, ErrorCode: ec, HTTPStatus: http.StatusBadRequest, Message: message, Invalid: true, Terminal: true}
}

// telegramLinkStatus 从数据库投影一条链接的当前状态。所有权不匹配与不存在
// 都回 not_found，避免用资源 ID 探测他人的链接。
func (a *App) telegramLinkStatus(ctx context.Context, id string, uid int64, secretHash, scene string, now int64) telegramLinkState {
	id = strings.ToLower(strings.TrimSpace(id))
	if !telegramLinkIDPattern.MatchString(id) {
		return telegramLinkTerminal("invalid_format", ErrTGBindCodeFormat, "绑定链接格式不正确")
	}
	l, err := a.store().TelegramLink(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return telegramLinkTerminal("not_found", ErrTGBindCodeNotFound, "绑定链接不存在")
	}
	if err != nil {
		logTelegramLinkFailure("status", err)
		return telegramLinkState{Status: "unavailable", HTTPStatus: http.StatusServiceUnavailable, ErrorCode: ErrBindCodeSaveFailed, Message: "绑定服务暂不可用，请稍后重试"}
	}
	if !l.OwnedBy(uid, secretHash) {
		return telegramLinkTerminal("not_found", ErrTGBindCodeNotFound, "绑定链接不存在")
	}
	if scene != "" && l.Scene != scene {
		return telegramLinkTerminal("wrong_scene", ErrTGBindCodeSceneBad, "绑定链接场景无效")
	}
	if l.Expired(now) {
		return telegramLinkTerminal("expired", ErrTGBindCodeExpired, "绑定链接已过期，请重新获取")
	}
	if l.State == "cancelled" {
		return telegramLinkTerminal("cancelled", ErrTGBindCodeExpired, "绑定链接已被新的链接取代")
	}
	if l.Scene == "register" && l.State == "consumed" {
		return telegramLinkTerminal("consumed", ErrTGBindCodeExpired, "绑定链接已使用")
	}
	state := telegramLinkState{ID: l.ID, Link: l, Status: "pending", ExpiresIn: l.ExpiresAt - now, Message: "等待在 Telegram 中确认"}
	if l.Confirmed() {
		if l.Scene == "register" {
			if _, taken := a.store().FindUserByTelegramID(l.TelegramID); taken {
				return telegramLinkTerminal("telegram_taken", ErrTGBindTargetTaken, "该 Telegram 已绑定其他账号")
			}
		} else if l.CurrentTelegramID == nil || *l.CurrentTelegramID != l.TelegramID {
			return telegramLinkTerminal("telegram_taken", ErrTGBindTargetTaken, "绑定状态已变化，请刷新后重试")
		}
		state.Status, state.Confirmed, state.Terminal = "confirmed", true, true
		state.TelegramBound = l.Scene == "user"
		state.TelegramID, state.TelegramUsername = l.TelegramID, l.TelegramUsername
		state.Message = "Telegram 已确认"
		return state
	}
	if l.ErrorCode != "" {
		// 加群校验 / 上游失败：保持 pending，用户处理后在 Telegram 里重试即可。
		state.ErrorCode, state.Message, state.Retryable = ErrCode(l.ErrorCode), l.ErrorMessage, l.Retryable
	}
	return state
}

func writeTelegramLinkState(w http.ResponseWriter, state telegramLinkState) {
	w.Header().Set("Cache-Control", "no-store")
	if state.HTTPStatus == http.StatusServiceUnavailable {
		failWithCode(w, state.HTTPStatus, state.ErrorCode, state.Message)
		return
	}
	if state.Invalid {
		writeJSONWithCode(w, http.StatusOK, false, state.ErrorCode, state.Message, state.response())
		return
	}
	ok(w, "OK", state.response())
}

func (a *App) handleRegisterTelegramLinkStatus(w http.ResponseWriter, r *http.Request, p Params) {
	secret := strings.TrimSpace(r.Header.Get(telegramLinkSecretHeader))
	writeTelegramLinkState(w, a.telegramLinkStatus(r.Context(), p["id"], 0, store.TelegramLinkSecretHash(secret), "register", time.Now().Unix()))
}

func (a *App) handleUserTelegramLinkStatus(w http.ResponseWriter, r *http.Request, p Params) {
	writeTelegramLinkState(w, a.telegramLinkStatus(r.Context(), p["id"], current(r).User.UID, "", "user", time.Now().Unix()))
}

// ---- Bot 确认 ----

type telegramLinkResult struct {
	Success   bool
	Code      int
	ErrorCode ErrCode
	Message   string
	Scene     string
}

// confirmTelegramLink 是 Bot 收到 /start <token>、/bind <token> 或裸 token 后的
// 唯一入口。外部校验（加群）在数据库事务之前完成；写入本身不做网络调用。
func (a *App) confirmTelegramLink(ctx context.Context, token string, telegramID int64, username string) telegramLinkResult {
	fail := func(status int, ec ErrCode, msg string) telegramLinkResult {
		return telegramLinkResult{Code: status, ErrorCode: ec, Message: msg}
	}
	token = store.NormalizeTelegramLinkToken(token)
	if !a.telegramAvailable() {
		return fail(http.StatusServiceUnavailable, ErrTGNotConfigured, "Telegram Bot 未配置")
	}
	if !telegramLinkTokenPattern.MatchString(token) {
		return fail(http.StatusBadRequest, ErrTGBindCodeFormat, "绑定链接格式无效")
	}
	if telegramID <= 0 {
		return fail(http.StatusBadRequest, ErrTGBindTGIDInvalid, "Telegram ID 无效")
	}
	if !a.allowRate(ctx, rateKey("tg-link-confirm:", telegramID), a.cfg().RateLimitLoginPerMinute, time.Minute) {
		return fail(http.StatusTooManyRequests, ErrUploadRateLimited, "操作过于频繁，请稍后再试")
	}
	l, err := a.store().TelegramLinkByStartToken(ctx, token)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (l.Expired(time.Now().Unix()) || l.State == "cancelled")) {
		return fail(http.StatusNotFound, ErrTGBindCodeNotFound, "绑定链接不存在或已过期")
	}
	if err != nil {
		logTelegramLinkFailure("confirm_lookup", err)
		return fail(http.StatusServiceUnavailable, ErrBindCodeSaveFailed, "绑定服务暂不可用，请稍后重试")
	}
	if l.Confirmed() && l.TelegramID != telegramID {
		return fail(http.StatusConflict, ErrTGBindTargetTaken, "该绑定链接已由其他 Telegram 确认")
	}
	if !l.Confirmed() {
		var result telegramLinkResult
		if missing, err := a.telegramBindRequirementMissing(ctx, telegramID); err != nil {
			// Telegram 协议层已脱敏 Bot Token，这里记录上游原因供管理员定位
			// （Bot 不在群 / 无权限 / chat_id 配错 / 接口超时）。
			zap.L().Warn("telegram link membership check failed", zap.Int64("telegram_id", telegramID), zap.Error(err))
			result = fail(http.StatusBadGateway, ErrTGBindGroupCheckFailed, "Telegram 加群/频道校验失败，请稍后重试")
		} else if len(missing) > 0 {
			result = fail(http.StatusForbidden, ErrTGBindGroupMembershipRequired, "绑定前需要先加入指定 Telegram 群组/频道: "+strings.Join(missing, ", "))
		}
		if result.Code != 0 {
			if err := a.store().SetTelegramLinkFailure(ctx, l.ID, string(result.ErrorCode), result.Message, true); err != nil {
				logTelegramLinkFailure("record_failure", err)
			}
			return result
		}
	}
	confirmed, user, bound, err := a.store().ConfirmTelegramLink(ctx, token, telegramID, username)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrExpired):
		return fail(http.StatusNotFound, ErrTGBindCodeNotFound, "绑定链接不存在或已过期")
	case errors.Is(err, store.ErrTelegramAlreadyBound):
		return fail(http.StatusConflict, ErrTGAlreadyBound, "当前账号已绑定 Telegram，请先完成换绑审批和解绑")
	case errors.Is(err, store.ErrConflict):
		return fail(http.StatusConflict, ErrTGBindTargetTaken, "该 Telegram 已被占用或绑定状态已变化")
	case err != nil:
		logTelegramLinkFailure("confirm_write", err)
		return fail(http.StatusServiceUnavailable, ErrBindCodeSaveFailed, "绑定服务暂不可用，请稍后重试")
	}
	if bound {
		a.auditTelegramAction(telegramID, "bind_telegram_via_telegram", "user", user.UID, map[string]any{"scene": confirmed.Scene})
	}
	return telegramLinkResult{Success: true, Code: http.StatusOK, Scene: confirmed.Scene, Message: "绑定已确认"}
}

// telegramLinkResultMessage 把确认结果翻译成给用户看的 Bot 回复。失败时区分
// “用户自己能处理”（过期 / 未加群 / 频繁）与“需要管理员处理”（Bot 未配置 /
// 群组校验出错 / 数据库不可用），并附带错误码，方便按运行日志定位。
func telegramLinkResultMessage(result telegramLinkResult) string {
	if result.Success {
		if result.Scene == "register" {
			return "Telegram 已确认，请回到网页继续完成注册。"
		}
		return "Telegram 绑定完成，可以回到网页继续。"
	}
	var text string
	switch {
	case result.ErrorCode == ErrTGNotConfigured:
		text = "Bot 未启用或未配置 Token，无法完成绑定，请联系管理员检查 [Telegram] 配置。"
	case result.ErrorCode == ErrTGBindGroupCheckFailed:
		text = "Telegram 群组/频道资格校验失败：Bot 可能不在目标群组/频道，或没有读取成员的权限，也可能是 Telegram 接口暂时异常。请稍后重试；若持续出现请联系管理员核对 force_bind_group / group_ids / channel_ids 配置。"
	case result.ErrorCode == ErrTGBindGroupMembershipRequired:
		text = firstNonEmpty(result.Message, "绑定前需要先加入指定 Telegram 群组/频道。") + "\n加入后再次点击网页上的绑定链接即可。"
	case result.ErrorCode == ErrTGAlreadyBound:
		text = "当前账号已绑定 Telegram，请先在网页完成换绑审批和解绑。"
	case result.ErrorCode == ErrTGBindTargetTaken:
		text = "该 Telegram 已绑定到其他账号，或这条绑定链接已被其他 Telegram 确认 / 已被新链接取代。请回到网页重新获取绑定链接。"
	case result.ErrorCode == ErrBindCodeSaveFailed:
		text = "绑定服务暂时不可用（数据库读写失败），请稍后重试；若持续出现请联系管理员查看运行日志中的 telegram link operation failed。"
	case result.Code == http.StatusNotFound:
		text = "绑定链接无效或已过期，请在网页重新获取。"
	case result.Code == http.StatusTooManyRequests:
		text = "操作过于频繁，请稍后再试。"
	case result.Code == http.StatusBadRequest:
		text = "绑定链接格式无效，请在网页重新获取后再试。"
	default:
		text = "绑定失败，请回到网页检查绑定状态后重试。"
	}
	if result.ErrorCode != "" {
		text += "\n\n错误码：" + string(result.ErrorCode)
	}
	return text
}

// telegramConfirmLinkFromChat 是 Bot 消息处理侧的薄封装：确认并回复。
func (a *App) telegramConfirmLinkFromChat(ctx context.Context, chatID, telegramID int64, username, token string) {
	result := a.confirmTelegramLink(ctx, token, telegramID, username)
	_ = a.telegramSendMessage(ctx, chatID, telegramLinkResultMessage(result))
}

// ---- 清理与诊断 ----

func (a *App) cleanupExpiredTelegramLinks(now int64) int {
	n, err := a.store().CleanupTelegramLinks(context.Background(), now, 0, 0)
	logTelegramLinkFailure("cleanup_expired", err)
	return n
}

func (a *App) cleanupOrphanedTelegramLinks() int {
	n, err := a.store().CleanupOrphanedTelegramLinks(context.Background())
	logTelegramLinkFailure("cleanup_orphaned", err)
	return n
}

// cleanupUserTelegramResidue 在账号解绑 / 管理员改绑 / 删号后清掉该账号与该
// Telegram 身份名下的所有链接。
func (a *App) cleanupUserTelegramResidue(uid, telegramID int64) int {
	n, err := a.store().CleanupTelegramLinks(context.Background(), 0, uid, telegramID)
	logTelegramLinkFailure("cleanup_identity", err)
	return n
}

// 只记录分类，不记录驱动 / 上游原文：错误文本可能带连接串或 token。
func logTelegramLinkFailure(operation string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	kind := "unavailable"
	if errors.Is(err, context.DeadlineExceeded) {
		kind = "timeout"
	}
	fields := []zap.Field{zap.String("operation", operation), zap.String("failure_kind", kind)}
	var pgerr *pgconn.PgError
	if errors.As(err, &pgerr) && len(pgerr.Code) == 5 {
		fields = append(fields, zap.String("sqlstate", pgerr.Code))
	}
	zap.L().Warn("telegram link operation failed", fields...)
}
