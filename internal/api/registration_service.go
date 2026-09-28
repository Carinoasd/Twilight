package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/prejudice-studio/twilight/internal/security"
	"github.com/prejudice-studio/twilight/internal/store"
	"github.com/prejudice-studio/twilight/internal/validate"
	"go.uber.org/zap"
)

// registrationInput is independent of HTTP so the WebUI and legacy
// compatibility route can share the same validation and state transition.
type registrationInput struct {
	Username           string
	Email              string
	Password           string
	RegCode            string
	TelegramLinkID     string
	TelegramLinkSecret string
	Context            context.Context
}

type registrationResult struct {
	User       store.User
	RegCode    store.RegCode
	FirstAdmin bool
}

type registrationFailure struct {
	Status  int
	Code    ErrCode
	Message string
}

// handleRegistration is the transport adapter shared by the V1 compatibility
// route and native V2 registration. It owns rate limiting and the response
// envelope; registerUser owns validation and state transitions.
func (a *App) handleRegistration(w http.ResponseWriter, r *http.Request) {
	if !a.allowRate(r.Context(), rateKey("register:", a.clientIP(r)), a.cfg().RateLimitRegisterPer10m, 10*time.Minute) {
		failWithCode(w, http.StatusTooManyRequests, ErrRegisterRateLimited, "注册过于频繁，请稍后再试")
		return
	}
	payload := decodeMap(r)
	input := registrationInput{
		Username:           stringValue(payload, "username"),
		Password:           stringValue(payload, "password"),
		Email:              stringValue(payload, "email"),
		RegCode:            firstNonEmpty(stringValue(payload, "reg_code"), stringValue(payload, "code")),
		TelegramLinkID:     stringValue(payload, "telegram_link_id"),
		TelegramLinkSecret: stringValue(payload, "telegram_link_secret"),
		Context:            r.Context(),
	}
	if input.RegCode != "" && !a.allowRate(r.Context(), rateKey("register:regcode:", a.clientIP(r)), 10, time.Minute) {
		failWithCode(w, http.StatusTooManyRequests, ErrRegisterRateLimited, "注册码注册尝试过于频繁")
		return
	}
	result, err := a.registerUser(input, time.Now().Unix())
	if err != nil {
		if failure, ok := err.(*registrationFailure); ok {
			failWithCode(w, failure.Status, failure.Code, failure.Message)
			return
		}
		if statusFromError(w, err) {
			return
		}
		return
	}
	// 公开注册路由不走 fallback 审计（AuthPublic），这里以新用户身份显式记一笔。
	regDetail := map[string]any{
		"telegram_bound": result.User.TelegramID != 0, "promoted_admin": result.FirstAdmin,
		"pending_emby": result.User.PendingEmby, "has_email": strings.TrimSpace(input.Email) != "",
	}
	if result.RegCode.Code != "" {
		regDetail["code_hint"] = regcodeAuditHint(result.RegCode.Code)
		regDetail["days"] = result.RegCode.Days
	}
	a.auditWithUser(r, result.User.UID, result.User.Username, "register", "user", result.User.UID, regDetail)
	created(w, "注册成功", map[string]any{
		"user": publicUser(result.User), "first_admin": result.FirstAdmin,
		"reg_code_used":           result.RegCode.Code,
		"email_verification_sent": sendRegistrationEmailVerification(a, r, result.User, input.Email),
	})
}

func (a *App) handleRegistrationAvailability(w http.ResponseWriter, r *http.Request) {
	if !a.allowRate(r.Context(), rateKey("register-availability:", a.clientIP(r)), checkAvailableRatePerMin, time.Minute) {
		failWithCode(w, http.StatusTooManyRequests, ErrRateLimited, "请求过于频繁，请稍后再试")
		return
	}
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	available := true
	message := ""
	if username != "" {
		_, found := a.store().FindUserByUsername(username)
		available = !found
		if !available {
			message = "用户名已被占用，请换一个用户名"
		}
	}
	currentUsers := a.store().UserCount()
	canRegister := a.cfg().RegisterEnabled || currentUsers == 0
	if reached, current, limit := a.systemUserLimitReached(); reached {
		canRegister = false
		available = false
		message = fmt.Sprintf("系统用户数量已达上限 %d/%d", current, limit)
	}
	embyBoundUsers := 0
	for _, user := range a.store().ListUsers() {
		if user.EmbyID != "" {
			embyBoundUsers++
		}
	}
	directDays := a.cfg().EmbyDirectRegisterDays
	if directDays == 0 {
		directDays = 30
	}
	ok(w, "OK", map[string]any{
		"enabled":                      a.cfg().RegisterEnabled,
		"register_mode":                a.cfg().RegisterEnabled,
		"can_register":                 canRegister,
		"requires_reg_code":            a.cfg().RegisterCodeLimit,
		"available":                    available,
		"message":                      message,
		"current_users":                currentUsers,
		"max_users":                    a.cfg().UserLimit,
		"allow_pending_register":       a.cfg().AllowPendingRegister,
		"emby_direct_register_enabled": a.cfg().EmbyDirectRegisterEnabled,
		"emby_direct_register_days":    directDays,
		"emby_user_limit":              a.cfg().EmbyUserLimit,
		"emby_bound_users":             embyBoundUsers,
	})
}

func sendRegistrationEmailVerification(a *App, r *http.Request, u store.User, email string) string {
	if email == "" || !emailConfigured(a.cfg()) {
		return ""
	}
	vid, status, _, message := a.issueEmailCode(r.Context(), a.clientIP(r), "bind", email, u.UID)
	if status != http.StatusOK {
		zap.L().Warn("register email auto-send failed", zap.Int64("uid", u.UID), zap.String("message", message))
		return ""
	}
	return vid
}

func (e *registrationFailure) Error() string { return e.Message }

func registrationFail(status int, code ErrCode, message string) error {
	return &registrationFailure{Status: status, Code: code, Message: message}
}

// registerUser performs local validation and the atomic Store transition. Mail
// and other notifications are intentionally outside this operation so a
// delivery failure cannot leave a half-created account.
func (a *App) registerUser(input registrationInput, now int64) (registrationResult, error) {
	// Setup and ordinary registration share the empty-system boundary. Keeping
	// this lock in the application service prevents a setup rollback from
	// interleaving with a registration request in the same process.
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	return a.registerUserLocked(input, now)
}

func (a *App) registerUserLocked(input registrationInput, now int64) (registrationResult, error) {
	if input.Context == nil {
		input.Context = context.Background()
	}
	input.Username = strings.TrimSpace(input.Username)
	input.Email = strings.TrimSpace(input.Email)
	input.RegCode = strings.TrimSpace(input.RegCode)
	input.TelegramLinkID = strings.ToLower(strings.TrimSpace(input.TelegramLinkID))
	input.TelegramLinkSecret = strings.TrimSpace(input.TelegramLinkSecret)
	if now == 0 {
		now = time.Now().Unix()
	}

	currentUsers := a.store().UserCount()
	if !a.cfg().RegisterEnabled && currentUsers > 0 {
		return registrationResult{}, registrationFail(403, ErrRegisterDisabled, "系统注册未开启")
	}
	if err := validate.ValidateUsername(input.Username); err != nil {
		return registrationResult{}, registrationFail(400, ErrUsernameInvalid, err.Error())
	}
	if err := validate.ValidatePasswordStrength(input.Password); err != nil {
		return registrationResult{}, registrationFail(400, ErrPasswordWeak, err.Error())
	}
	if _, exists := a.store().FindUserByUsername(input.Username); exists {
		return registrationResult{}, registrationFail(409, ErrUsernameTaken, "用户名已被占用，请换一个用户名")
	}
	if input.Email != "" {
		if err := validate.ValidateEmailFormat(input.Email); err != nil {
			return registrationResult{}, registrationFail(400, ErrEmailInvalid, err.Error())
		}
		cfg := a.cfg()
		if len(cfg.EmailBlacklist) > 0 && validate.CheckEmailBlacklist(input.Email, cfg.EmailBlacklist) {
			return registrationResult{}, registrationFail(400, ErrEmailInvalid, "该邮箱域名不在允许范围内")
		}
		if len(cfg.EmailWhitelist) > 0 && !validate.CheckEmailWhitelist(input.Email, cfg.EmailWhitelist) {
			return registrationResult{}, registrationFail(400, ErrEmailInvalid, "该邮箱域名不在允许范围内")
		}
		if a.store().EmailAlreadyUsed(input.Email) {
			return registrationResult{}, registrationFail(409, ErrEmailConflict, "该邮箱已被其他账号使用")
		}
	}
	bootstrapMode := currentUsers == 0
	var registerReg store.RegCode
	if a.cfg().RegisterCodeLimit && !bootstrapMode {
		if input.RegCode == "" {
			return registrationResult{}, registrationFail(400, ErrCodeEmpty, "注册需要提供注册码")
		}
		reg, ok := a.store().RegCode(input.RegCode)
		if !ok || reg.IsDecoy || reg.Type != 1 || regcodeStatus(reg) != "available" {
			return registrationResult{}, registrationFail(400, ErrRegcodeInvalid, "注册码无效、已用完或已过期")
		}
		registerReg = reg
	}
	if reached, current, limit := a.systemUserLimitReachedExcluding(registerReg.Code, ""); reached {
		return registrationResult{}, registrationFail(409, ErrUserLimitReached, fmt.Sprintf("系统用户数量已达上限 %d/%d", current, limit))
	}

	var telegramID int64
	var telegramUsername string
	if a.cfg().ForceBindTelegram || input.TelegramLinkID != "" {
		if input.TelegramLinkID == "" {
			return registrationResult{}, registrationFail(400, ErrTGBindRequired, "需要先完成 Telegram 绑定")
		}
		if !telegramLinkIDPattern.MatchString(input.TelegramLinkID) {
			return registrationResult{}, registrationFail(400, ErrTGBindCodeFormat, "Telegram 绑定链接格式不正确")
		}
		// 所有权由 link_secret 决定：不带或带错 secret 的提交一律视为链接不存在。
		state := a.telegramLinkStatus(input.Context, input.TelegramLinkID, 0, store.TelegramLinkSecretHash(input.TelegramLinkSecret), "register", now)
		if state.Status != "confirmed" {
			status := state.HTTPStatus
			if status == 0 {
				status = 400
			}
			return registrationResult{}, registrationFail(status, state.ErrorCode, state.Message)
		}
		telegramID = state.TelegramID
		telegramUsername = state.TelegramUsername
	}
	if registerReg.Code != "" && regcodeTargetMismatchReason(registerReg, store.User{
		Username: input.Username, TelegramID: telegramID, TelegramUsername: telegramUsername,
	}) != "" {
		return registrationResult{}, registrationFail(400, ErrRegcodeInvalid, "注册码无效、已用完或已过期")
	}
	if registerReg.Code != "" {
		if reached, current, limit := a.embyCapacityReachedExcluding(0, input.RegCode, ""); reached {
			return registrationResult{}, registrationFail(409, ErrEmbyCapacityReached, fmt.Sprintf("Emby 用户数量已达上限 %d/%d", current, limit))
		}
	}

	hash, err := security.HashPassword(input.Password)
	if err != nil {
		return registrationResult{}, registrationFail(500, ErrPasswordHashFailed, "密码处理失败")
	}
	newUser := store.User{
		Username: input.Username, Email: input.Email, PasswordHash: hash,
		Role: store.RoleNormal, TelegramID: telegramID, TelegramUsername: telegramUsername,
	}
	applyGrant := func(user *store.User, consumed store.RegCode, _ store.BindCode) error {
		if consumed.Code == "" {
			return nil
		}
		// 锁内复核 Emby 名额（新用户尚未写入 state，无需排除）。
		if err := a.embyCapacityExceededHeldLock(0, consumed.Code); err != nil {
			return err
		}
		days := normalizeRegCodeDays(consumed.Days)
		user.PendingEmby = true
		user.PendingEmbyDays = &days
		user.EmbyUsername = input.Username
		markRegistrationGrant(user, registrationSourceRegCode, consumed.Code)
		return nil
	}
	createUser := func(base store.User) (store.User, store.RegCode, error) {
		if registerReg.Code == "" {
			user, err := a.store().CreateUser(base)
			return user, store.RegCode{}, err
		}
		user, consumed, _, err := a.store().CreateUserForRegistration(base, registerReg.Code, "", now, applyGrant)
		return user, consumed, err
	}

	var user store.User
	var consumed store.RegCode
	if input.TelegramLinkID != "" {
		var consumedBind store.BindCode
		user, consumed, consumedBind, err = a.store().RegisterWithTelegramLink(input.Context, newUser, registerReg.Code, input.TelegramLinkID, store.TelegramLinkSecretHash(input.TelegramLinkSecret), applyGrant)
		if err != nil {
			return registrationResult{}, a.mapRegistrationStoreError(err, input, consumedBind, registerReg)
		}
	} else {
		user, consumed, err = createUser(newUser)
		if err != nil {
			return registrationResult{}, a.mapRegistrationStoreError(err, input, store.BindCode{}, registerReg)
		}
	}

	firstAdmin := false
	if a.configuredAdminMatch(user.UID, user.Username) {
		if promoted, promoteErr := a.store().UpdateUser(user.UID, func(existing *store.User) error {
			existing.Role = store.RoleAdmin
			existing.Active = true
			return nil
		}); promoteErr == nil {
			user = promoted
			firstAdmin = true
		}
	}
	return registrationResult{User: user, RegCode: consumed, FirstAdmin: firstAdmin}, nil
}

func (a *App) mapRegistrationStoreError(err error, input registrationInput, bind store.BindCode, reg store.RegCode) error {
	if errors.Is(err, store.ErrEmbyCapacityReached) {
		return registrationFail(409, ErrEmbyCapacityReached, "Emby 用户数量已达上限")
	}
	if errors.Is(err, store.ErrTelegramLinkOwner) {
		return registrationFail(400, ErrTGBindCodeNotFound, "绑定链接不存在或不属于当前浏览器")
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrExpired) {
		if input.TelegramLinkID != "" && bind.Code == "" {
			return registrationFail(400, ErrTGBindCodeExpired, "绑定链接无效或已过期")
		}
		if bind.Code != "" || reg.Code != "" {
			return registrationFail(400, ErrRegcodeInvalid, "注册码无效、已用完或已过期")
		}
		return registrationFail(400, ErrRegcodeInvalid, "注册码无效、已用完或已过期")
	}
	if errors.Is(err, store.ErrConflict) {
		if _, exists := a.store().FindUserByUsername(input.Username); exists {
			return registrationFail(409, ErrUsernameTaken, "用户名已被占用，请换一个用户名")
		}
		if input.Email != "" && a.store().EmailAlreadyUsed(input.Email) {
			return registrationFail(409, ErrEmailConflict, "该邮箱已被其他账号使用")
		}
		if bind.TelegramID != 0 {
			if _, exists := a.store().FindUserByTelegramID(bind.TelegramID); exists {
				return registrationFail(409, ErrTGBindTargetTaken, "该 Telegram 已绑定到其他账号或绑定码状态已变化")
			}
		}
		if reg.Code != "" {
			return registrationFail(400, ErrRegcodeInvalid, "注册码无效、已用完或已过期")
		}
		if input.TelegramLinkID != "" {
			return registrationFail(409, ErrTGBindTargetTaken, "该 Telegram 已绑定到其他账号或绑定码状态已变化")
		}
		return registrationFail(409, ErrUsernameTaken, "账号信息已被占用，请检查后重试")
	}
	if input.TelegramLinkID != "" {
		logTelegramLinkFailure("registration_commit", err)
		return registrationFail(503, ErrBindCodeSaveFailed, "注册暂未完成，请稍后重试")
	}
	return err
}
