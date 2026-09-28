package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prejudice-studio/twilight/internal/store"
	"go.uber.org/zap"
)

const (
	auditDetailMaxBytes       = 8 * 1024
	auditDetailMaxNodes       = 512
	auditDetailMaxDepth       = 6
	auditDetailMaxItems       = 64
	auditDetailMaxStringRunes = 512
	auditRedactedValue        = "[REDACTED]"
	auditTruncatedValue       = "[TRUNCATED]"
)

var (
	destructiveAuditKeywords = []string{
		"delete", "disable", "clear", "prune", "revoke", "ban", "kick",
		"terminate", "reset_password", "force_unbind", "unbind", "detach",
	}
	securityAuditKeywords = []string{
		"login", "logout", "password", "role", "telegram", "developer",
		"security", "audit", "violation", "ip", "device", "apikey",
	}
	// fallbackAuditActionReplacer 把 fallback 审计 action 名里的路径分隔符与
	// 花括号替换为下划线 / 空串。原实现在每次调用 fallbackAuditAction 时新建
	// Replacer（内部会分配字符串切片），现提升为包级变量。
	fallbackAuditActionReplacer = strings.NewReplacer("/", "_", "-", "_", ":", "", "{", "", "}", "")
	// auditSensitiveDetailKeyReplacer 是 auditSensitiveDetailKey 的字段名归一化表：
	// 除 sensitiveLogKeyReplacer 剥的 `_`/`-`/`.` 外还额外剥空格（如 "Verification Code"）。
	// 原实现在每次调用时新建 Replacer，现提升为包级变量。
	auditSensitiveDetailKeyReplacer = strings.NewReplacer("_", "", "-", "", ".", "", " ", "")
)

// audit 是写入操作审计日志的便捷方法。category 为 "admin" / "user" / "system"。
// AuditLog.enabled=false 时静默跳过记录。从 current(r) 提取操作者身份。
func (a *App) audit(r *http.Request, action, category string, targetUID int64, detail map[string]any) {
	p := current(r)
	a.auditEntry(r, p.User.UID, p.User.Username, action, category, targetUID, detail)
}

// auditWithUser 用于登录等尚无会话上下文但已知用户身份的路径。
// 避免因为 AuthPublic 接口中 current(r) 返回零值导致审计日志 uid=0 / username=""。
func (a *App) auditWithUser(r *http.Request, uid int64, username, action, category string, targetUID int64, detail map[string]any) {
	a.auditEntry(r, uid, username, action, category, targetUID, detail)
}

func (a *App) auditEntry(r *http.Request, uid int64, username, action, category string, targetUID int64, detail map[string]any) {
	markRequestAuditWritten(r)
	// 用户自助操作的 target 记成本人，按 target_uid 筛某个用户时才能查到他的自助记录。
	if targetUID <= 0 && uid > 0 && normalizeAuditCategory(category) == "user" {
		targetUID = uid
	}
	entry := store.AuditLog{
		UID:       uid,
		Username:  username,
		Action:    action,
		Category:  category,
		Source:    "http",
		Method:    r.Method,
		TargetUID: targetUID,
		Detail:    detail,
		IP:        a.clientIP(r),
	}
	a.writeAuditEntry(entry)
}

// markAuditDryRun 让 handler 显式声明本次请求是否为预览（dry-run）。
// payload 未带 dry_run、而 handler 默认按预览执行时应调用它，fallback 审计才能区分。
func markAuditDryRun(r *http.Request, dryRun bool) {
	if r == nil {
		return
	}
	state, _ := r.Context().Value(auditRequestKey).(*auditRequestState)
	if state == nil {
		return
	}
	if dryRun {
		state.dryRun.Store(2)
	} else {
		state.dryRun.Store(1)
	}
}

// noteAuditDryRunFromPayload 在 decodeMap 解出 payload 后，若显式带了 dry_run 就记下。
func noteAuditDryRunFromPayload(r *http.Request, payload map[string]any) {
	if raw, exists := payload["dry_run"]; exists {
		markAuditDryRun(r, auditTruthy(raw))
	}
}

func auditTruthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

// requestAuditDryRun 返回本次请求的 dry_run 状态；known=false 表示无法判断。
// 优先 handler / payload 的标记，其次 query 的 dry_run。
func requestAuditDryRun(r *http.Request) (dryRun bool, known bool) {
	if r == nil {
		return false, false
	}
	if state, _ := r.Context().Value(auditRequestKey).(*auditRequestState); state != nil {
		switch state.dryRun.Load() {
		case 1:
			return false, true
		case 2:
			return true, true
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("dry_run")); raw != "" {
		return auditTruthy(raw), true
	}
	return false, false
}

func markRequestAuditWritten(r *http.Request) {
	if r == nil {
		return
	}
	state, _ := r.Context().Value(auditRequestKey).(*auditRequestState)
	if state != nil {
		state.wrote.Store(true)
	}
}

// skipAuditForDryRun 用于预览 / dry-run 分支：它们不改任何数据，不写审计，
// 同时标记本请求"已处理审计"，避免 fallback 把预览记成与真实操作同名的动作。
func skipAuditForDryRun(r *http.Request) {
	markRequestAuditWritten(r)
}

func requestAuditWritten(r *http.Request) bool {
	if r == nil {
		return false
	}
	state, _ := r.Context().Value(auditRequestKey).(*auditRequestState)
	return state != nil && state.wrote.Load()
}

// auditHTTPMutationConfigured reports whether HTTP mutation fallback audit is
// enabled this request. It's the cheap gate used by maybeAuditHTTPMutation before
// any route-shape work; writeAuditEntry enforces the same switch at persist time.
func (a *App) auditHTTPMutationConfigured() bool {
	return a.cfg().AuditLogEnabled
}

func (a *App) maybeAuditHTTPMutation(r *http.Request, route *Route, params Params, p *principal, status int, errorCode string) {
	// 审计总开关关闭时，本次请求的 fallback 审计已无意义，直接短路。
	// 配置在每请求只读一次（auditHTTPMutationConfigured，见 writeAuditEntry 对
	// AuditLogEnabled 的同口径）；关闭期间热路径不再为「是否该补审计」反复装配
	// safeAuditTargetUID / fallbackAuditAction 的临时对象。
	if !a.auditHTTPMutationConfigured() {
		return
	}
	if route == nil || requestAuditWritten(r) {
		return
	}
	kind := fallbackAuditKind(r, route, status)
	if kind == fallbackAuditSkip {
		return
	}
	uid, username := int64(0), ""
	category := "system"
	targetUID := int64(0)
	if p != nil {
		uid = p.User.UID
		username = p.User.Username
		if p.User.Role == store.RoleAdmin || route.Auth == AuthAdmin {
			category = "admin"
		} else {
			category = "user"
		}
		// 自助路由（User / APIKey）的操作对象就是本人，管理员自己改自己的设置也一样。
		if route.Auth == AuthUser || route.Auth == AuthAPIKey {
			targetUID = p.User.UID
		}
	}
	if target := safeAuditTargetUID(params); target > 0 {
		targetUID = target
	}
	action := fallbackAuditAction(route)
	detail := map[string]any{
		"fallback":      true,
		"path_template": route.Pattern,
		"status":        status,
		"params":        safeAuditRouteParams(params),
	}
	if dryRun, known := requestAuditDryRun(r); known {
		detail["dry_run"] = dryRun
	}
	if kind == fallbackAuditFailed {
		// 管理员写操作失败（403/409/5xx 等）也留痕，action 加 _failed 后缀。
		action += "_failed"
		if errorCode != "" {
			detail["error_code"] = errorCode
		}
	}
	a.auditEntry(r, uid, username, action, category, targetUID, detail)
}

type fallbackAuditDecision int

const (
	fallbackAuditSkip fallbackAuditDecision = iota
	fallbackAuditSuccess
	fallbackAuditFailed
)

// fallbackAuditKind 判断 handler 返回后是否需要补写 fallback 审计：
//   - 2xx：User / Admin / APIKey 路由的写请求记成功；
//   - 4xx / 5xx：只有 AuthAdmin 路由记失败（401 未登录、429 限流除外），
//     普通用户的失败请求量大且多为输入错误，不记。
func fallbackAuditKind(r *http.Request, route *Route, status int) fallbackAuditDecision {
	if r == nil || route == nil {
		return fallbackAuditSkip
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return fallbackAuditSkip
	}
	pattern := route.Pattern
	// V1 and V2 share the audit store, so both prefixes must be exempted.
	// 审计日志维护接口在成功时已写不可删的自保记录；失败（如缺确认短语）不再补记，
	// 避免裁剪 / 清空时产生新的待裁剪记录。
	if strings.HasPrefix(pattern, "/api/v1/admin/audit-logs") ||
		strings.HasPrefix(pattern, "/api/v2/admin/audit-logs") {
		return fallbackAuditSkip
	}
	if pattern == "/api/v1/auth/refresh" || pattern == "/api/v2/auth/refresh" {
		return fallbackAuditSkip
	}
	if status >= 200 && status < 300 {
		if route.Auth == AuthUser || route.Auth == AuthAdmin || route.Auth == AuthAPIKey {
			return fallbackAuditSuccess
		}
		return fallbackAuditSkip
	}
	if status >= 400 && status != http.StatusUnauthorized && status != http.StatusTooManyRequests && route.Auth == AuthAdmin {
		return fallbackAuditFailed
	}
	return fallbackAuditSkip
}

func fallbackAuditAction(route *Route) string {
	pattern := route.Pattern
	// Normalise both API versions to the same unprefixed action name so V1 and
	// V2 mutations of one resource share a single audit action identifier.
	pattern = strings.TrimPrefix(pattern, "/api/v1/")
	pattern = strings.TrimPrefix(pattern, "/api/v2/")
	return strings.ToLower(route.Method) + "_" + fallbackAuditActionReplacer.Replace(strings.Trim(pattern, "/"))
}

func safeAuditTargetUID(params Params) int64 {
	for _, key := range []string{"uid", "user_id", "target_uid"} {
		if raw := strings.TrimSpace(params[key]); raw != "" {
			if value, err := strconv.ParseInt(raw, 10, 64); err == nil && value > 0 {
				return value
			}
		}
	}
	return 0
}

func safeAuditRouteParams(params Params) map[string]any {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]any, len(params))
	for key, raw := range params {
		normalized := strings.ToLower(strings.TrimSpace(key))
		value := strings.TrimSpace(raw)
		if normalized == "" || value == "" {
			continue
		}
		if normalized == "uid" || strings.HasSuffix(normalized, "_uid") || normalized == "id" || strings.HasSuffix(normalized, "_id") {
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				out[normalized] = parsed
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// auditEntryIP 是不依赖 *http.Request 的审计写入入口，供没有 HTTP 上下文的路径
// （如 Telegram Bot 命令）使用，IP 由调用方显式传入（如 "telegram"）。
func (a *App) auditEntryIP(ip string, uid int64, username, action, category string, targetUID int64, detail map[string]any) {
	source := "system"
	if strings.EqualFold(strings.TrimSpace(ip), "telegram") {
		source = "telegram"
	}
	a.writeAuditEntry(store.AuditLog{
		UID:       uid,
		Username:  username,
		Action:    action,
		Category:  category,
		Source:    source,
		TargetUID: targetUID,
		Detail:    detail,
		IP:        ip,
	})
}

// auditSystem records non-HTTP work such as scheduler and background-service
// mutations through the same normalization, redaction, and retention path.
func (a *App) auditSystem(source, action string, targetUID int64, detail map[string]any) {
	source = normalizeAuditSource(source)
	a.writeAuditEntry(store.AuditLog{
		Username:  source,
		Action:    action,
		Category:  "system",
		Source:    source,
		TargetUID: targetUID,
		Detail:    detail,
	})
}

func (a *App) auditTelegramAction(telegramID int64, action, category string, targetUID int64, detail map[string]any) {
	uid, username := a.telegramAdminIdentity(telegramID)
	if username == "" && telegramID != 0 {
		username = fmt.Sprintf("telegram:%d", telegramID)
	}
	if detail == nil {
		detail = map[string]any{}
	}
	if telegramID != 0 {
		detail["telegram_id"] = telegramID
	}
	a.auditEntryIP("telegram", uid, username, action, category, targetUID, detail)
}

func (a *App) writeAuditEntry(entry store.AuditLog) {
	cfg := a.cfg()
	if !cfg.AuditLogEnabled {
		return
	}
	if entry.UID < 0 {
		entry.UID = 0
	}
	if entry.TargetUID < 0 {
		entry.TargetUID = 0
	}
	entry.Username = truncateString(redactSensitiveText(strings.TrimSpace(entry.Username)), 128)
	entry.Action = normalizeAuditAction(entry.Action)
	entry.Category = normalizeAuditCategory(entry.Category)
	entry.Source = normalizeAuditSource(entry.Source)
	entry.Method = truncateString(strings.ToUpper(strings.TrimSpace(entry.Method)), 16)
	entry.IP = truncateString(strings.TrimSpace(entry.IP), 128)
	entry.Detail = sanitizeAuditDetail(entry.Detail)
	limit := cfg.AuditLogMaxEntries
	if limit <= 0 {
		limit = 10000
	}
	if err := a.store().AddAuditLog(entry, limit); err != nil {
		// 审计写入失败不能静默吞掉：走 zap 运行日志并累计计数，计数经
		// /system/stats 与数据库健康检查暴露。日志里只放 action 等元数据，不放 detail。
		auditWriteFailures.Add(1)
		auditWriteLastFailureAt.Store(time.Now().Unix())
		zap.L().Error("audit log persistence failed",
			zap.String("action", entry.Action),
			zap.String("category", entry.Category),
			zap.String("source", entry.Source),
			zap.String("error", redactSensitiveText(err.Error())))
	}
}

// 审计写入失败计数（进程级）。多实例部署时各实例各自计数。
var (
	auditWriteFailures      atomic.Int64
	auditWriteLastFailureAt atomic.Int64
)

// auditWriteFailureStats 返回审计写入失败的累计次数与最近一次失败时间（unix 秒，0 表示从未失败）。
func auditWriteFailureStats() map[string]any {
	return map[string]any{
		"failures":        auditWriteFailures.Load(),
		"last_failure_at": auditWriteLastFailureAt.Load(),
	}
}

func normalizeAuditAction(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastSeparator := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			lastSeparator = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.' || r == '/':
			if out.Len() > 0 && !lastSeparator {
				out.WriteByte('_')
				lastSeparator = true
			}
		}
		if out.Len() >= 80 {
			break
		}
	}
	action := strings.Trim(out.String(), "_")
	if action == "" {
		return "unknown_action"
	}
	return action
}

func normalizeAuditCategory(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "admin":
		return "admin"
	case "user":
		return "user"
	default:
		return "system"
	}
}

func normalizeAuditSource(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "http", "api":
		return "http"
	case "telegram", "bot":
		return "telegram"
	case "scheduler", "schedule", "job":
		return "scheduler"
	default:
		return "system"
	}
}

type auditDetailSanitizer struct {
	nodesLeft int
	truncated bool
}

func sanitizeAuditDetail(detail map[string]any) map[string]any {
	if len(detail) == 0 {
		return nil
	}
	sanitizer := &auditDetailSanitizer{nodesLeft: auditDetailMaxNodes}
	out := sanitizer.sanitizeMap(detail, 0)
	if sanitizer.truncated {
		out["_truncated"] = true
	}
	return fitAuditDetail(out)
}

func (s *auditDetailSanitizer) sanitizeMap(value map[string]any, depth int) map[string]any {
	out := make(map[string]any, min(len(value), auditDetailMaxItems)+1)
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		if index >= auditDetailMaxItems || s.nodesLeft <= 0 {
			s.truncated = true
			break
		}
		cleanKey := truncateString(strings.ToValidUTF8(strings.TrimSpace(key), ""), 64)
		if cleanKey == "" {
			cleanKey = "field"
		}
		out[cleanKey] = s.sanitizeValue(cleanKey, value[key], depth+1)
	}
	return out
}

func (s *auditDetailSanitizer) sanitizeValue(key string, value any, depth int) any {
	if auditSensitiveDetailKey(key) {
		return auditRedactedValue
	}
	if s.nodesLeft <= 0 || depth > auditDetailMaxDepth {
		s.truncated = true
		return auditTruncatedValue
	}
	s.nodesLeft--
	switch typed := value.(type) {
	case nil, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return typed
	case string:
		return sanitizeAuditString(typed, s)
	case error:
		return sanitizeAuditString(typed.Error(), s)
	case map[string]any:
		return s.sanitizeMap(typed, depth)
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			converted[childKey] = childValue
		}
		return s.sanitizeMap(converted, depth)
	case []any:
		return s.sanitizeSlice(typed, depth)
	case []string:
		converted := make([]any, len(typed))
		for i, item := range typed {
			converted[i] = item
		}
		return s.sanitizeSlice(converted, depth)
	case []int64:
		converted := make([]any, len(typed))
		for i, item := range typed {
			converted[i] = item
		}
		return s.sanitizeSlice(converted, depth)
	case []int:
		converted := make([]any, len(typed))
		for i, item := range typed {
			converted[i] = item
		}
		return s.sanitizeSlice(converted, depth)
	default:
		encoded, err := json.Marshal(value)
		if err == nil {
			var generic any
			if json.Unmarshal(encoded, &generic) == nil {
				return s.sanitizeValue(key, generic, depth)
			}
		}
		return sanitizeAuditString(fmt.Sprint(value), s)
	}
}

func (s *auditDetailSanitizer) sanitizeSlice(value []any, depth int) []any {
	limit := min(len(value), auditDetailMaxItems)
	out := make([]any, 0, limit+1)
	for index := 0; index < limit; index++ {
		if s.nodesLeft <= 0 {
			s.truncated = true
			break
		}
		out = append(out, s.sanitizeValue("", value[index], depth+1))
	}
	if len(value) > limit {
		s.truncated = true
	}
	return out
}

func sanitizeAuditString(value string, sanitizer *auditDetailSanitizer) string {
	value = strings.ToValidUTF8(redactSensitiveText(value), "")
	if utf8.RuneCountInString(value) > auditDetailMaxStringRunes {
		sanitizer.truncated = true
		value = truncateString(value, auditDetailMaxStringRunes) + "..."
	}
	return value
}

func auditSensitiveDetailKey(key string) bool {
	if sensitiveLogKey(key) {
		return true
	}
	normalized := auditSensitiveDetailKeyReplacer.Replace(strings.ToLower(key))
	if normalized == "code" || normalized == "codes" || normalized == "credential" || normalized == "credentials" {
		return true
	}
	if strings.Contains(normalized, "regcode") || strings.Contains(normalized, "invitecode") || strings.Contains(normalized, "bindcode") || strings.Contains(normalized, "verificationcode") {
		return true
	}
	if strings.HasSuffix(normalized, "code") {
		switch normalized {
		case "errorcode", "statuscode", "codetype", "sourcecode":
			return false
		default:
			return true
		}
	}
	return false
}

func fitAuditDetail(detail map[string]any) map[string]any {
	encoded, err := json.Marshal(detail)
	if err == nil && len(encoded) <= auditDetailMaxBytes {
		return detail
	}
	keys := make([]string, 0, len(detail))
	for key := range detail {
		if key != "_truncated" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := map[string]any{"_truncated": true}
	for _, key := range keys {
		out[key] = detail[key]
		candidate, marshalErr := json.Marshal(out)
		if marshalErr == nil && len(candidate) <= auditDetailMaxBytes {
			continue
		}
		out[key] = auditTruncatedValue
		candidate, marshalErr = json.Marshal(out)
		if marshalErr != nil || len(candidate) > auditDetailMaxBytes {
			delete(out, key)
		}
	}
	return out
}

func (a *App) handleListAuditLogs(w http.ResponseWriter, r *http.Request, _ Params) {
	page := clamp(queryInt(r, "page", 1), 1, 1000000)
	perPage := clamp(queryInt(r, "per_page", 50), 1, 200)
	presetFilter := strings.ToLower(r.URL.Query().Get("preset"))
	categoryFilter := strings.ToLower(r.URL.Query().Get("category"))
	actionFilter := strings.ToLower(r.URL.Query().Get("action"))
	sourceFilter := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	search := strings.ToLower(r.URL.Query().Get("search"))
	from := auditLogUnixQuery(r, "from", "start")
	to := auditLogUnixQuery(r, "to", "end")
	sortBy := normalizeAuditLogSort(r.URL.Query().Get("sort"))
	order := normalizeSortOrder(r.URL.Query().Get("order"))

	// 0 表示"不过滤"：这两个是可选筛选条件，非法值回退 0 等价于不筛选，
	// 而不是筛出 UID 0（该系统不存在 UID 0 的用户）。
	uid := queryInt64(r, "uid", 0)
	targetUID := queryInt64(r, "target_uid", 0)
	if categoryFilter == "all" {
		categoryFilter = ""
	}
	if actionFilter == "all" {
		actionFilter = ""
	}
	// source 只接受已知取值，其它值（含 all）视为不筛选。
	switch sourceFilter {
	case "http", "telegram", "scheduler", "system":
	default:
		sourceFilter = ""
	}
	actionKeywords := []string(nil)
	switch presetFilter {
	case "admin", "user", "system":
		if categoryFilter == "" {
			categoryFilter = presetFilter
		} else if categoryFilter != presetFilter {
			categoryFilter = "__no_matching_category__"
		}
	case "destructive":
		actionKeywords = destructiveAuditKeywords
	case "security":
		actionKeywords = securityAuditKeywords
	case "today":
		now := time.Now()
		presetFrom := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
		if presetFrom > from {
			from = presetFrom
		}
	case "week":
		presetFrom := time.Now().Add(-7 * 24 * time.Hour).Unix()
		if presetFrom > from {
			from = presetFrom
		}
	}

	result := a.store().QueryAuditLogs(store.AuditLogQuery{
		Category:       categoryFilter,
		Action:         actionFilter,
		Source:         sourceFilter,
		UID:            uid,
		TargetUID:      targetUID,
		From:           from,
		To:             to,
		Search:         truncateString(search, 200),
		ActionKeywords: actionKeywords,
		SortBy:         sortBy,
		Order:          order,
		Offset:         (page - 1) * perPage,
		Limit:          perPage,
	})
	dto := make([]map[string]any, 0, len(result.Logs))
	for _, log := range result.Logs {
		dto = append(dto, auditLogDTO(log))
	}
	ok(w, "OK", map[string]any{
		"logs":     dto,
		"total":    result.Total,
		"page":     page,
		"per_page": perPage,
		"sort":     sortBy,
		"order":    order,
	})
}

// handleListAuditActions 返回审计表中出现过的 action 列表，前端据此生成筛选下拉。
func (a *App) handleListAuditActions(w http.ResponseWriter, _ *http.Request, _ Params) {
	w.Header().Set("Cache-Control", "private, no-store")
	actions, err := a.store().ListAuditActions(2000)
	if err != nil {
		failWithCode(w, http.StatusInternalServerError, ErrInternal, "读取审计 action 失败")
		return
	}
	ok(w, "OK", map[string]any{
		"actions": actions,
		"sources": []string{"http", "telegram", "scheduler", "system"},
	})
}

func auditLogUnixQuery(r *http.Request, names ...string) int64 {
	for _, name := range names {
		raw := strings.TrimSpace(r.URL.Query().Get(name))
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && value > 0 {
			return value
		}
	}
	return 0
}

func isDestructiveAuditAction(action string) bool {
	action = strings.ToLower(action)
	for _, keyword := range destructiveAuditKeywords {
		if strings.Contains(action, keyword) {
			return true
		}
	}
	return false
}

func isSecurityAuditAction(action string) bool {
	action = strings.ToLower(action)
	for _, keyword := range securityAuditKeywords {
		if strings.Contains(action, keyword) {
			return true
		}
	}
	return false
}

func normalizeAuditLogSort(value string) string {
	switch strings.ToLower(value) {
	case "id", "action", "category", "source", "method", "username", "uid", "target_uid", "ip":
		return strings.ToLower(value)
	default:
		return "created_at"
	}
}

func normalizeSortOrder(value string) string {
	if strings.EqualFold(value, "asc") {
		return "asc"
	}
	return "desc"
}

func (a *App) handleDeleteAuditLog(w http.ResponseWriter, r *http.Request, params Params) {
	id, _ := strconv.ParseInt(firstNonEmpty(params["log_id"], params["id"]), 10, 64)
	if id <= 0 {
		failWithCode(w, http.StatusBadRequest, ErrBadRequest, "无效的日志 ID")
		return
	}
	// 单条删除也要求确认短语（body 的 confirm 或 query 的 confirm 均可）。
	if firstNonEmpty(stringValue(decodeMap(r), "confirm"), r.URL.Query().Get("confirm")) != confirmDeleteAuditLog {
		failWithCode(w, http.StatusBadRequest, ErrBadRequest, "需要确认短语 confirm="+confirmDeleteAuditLog)
		return
	}
	deleted, err := a.store().DeleteAuditLog(id)
	if errors.Is(err, store.ErrAuditLogProtected) {
		failWithCode(w, http.StatusForbidden, ErrForbidden, "该审计记录不可删除")
		return
	}
	if err != nil {
		failWithCode(w, http.StatusNotFound, ErrNotFound, "日志不存在")
		return
	}
	// 删除成功后再写一条不可删除的自保记录，记下被删记录的关键信息。
	a.auditAuditLogMaintenance(r, "delete_audit_log", deleted.UID, map[string]any{
		"log_id":             deleted.ID,
		"deleted_action":     deleted.Action,
		"deleted_category":   deleted.Category,
		"deleted_uid":        deleted.UID,
		"deleted_username":   deleted.Username,
		"deleted_target_uid": deleted.TargetUID,
		"deleted_created_at": deleted.CreatedAt,
	})
	ok(w, "已删除", nil)
}

func (a *App) handleClearAuditLogs(w http.ResponseWriter, r *http.Request, _ Params) {
	payload := decodeMap(r)
	if stringValue(payload, "confirm") != confirmClearAuditLogs {
		failWithCode(w, http.StatusBadRequest, ErrBadRequest, "需要确认短语 confirm="+confirmClearAuditLogs)
		return
	}
	removed, err := a.store().ClearAuditLogs()
	if err != nil {
		failWithCode(w, http.StatusInternalServerError, ErrInternal, "清空失败")
		return
	}
	a.auditAuditLogMaintenance(r, "clear_audit_logs", 0, map[string]any{"removed": removed})
	ok(w, "审计日志已清空", map[string]any{"removed": removed})
}

// handlePruneAuditLogs 条件清理审计日志：支持按条数裁剪（max_entries）和按天数裁剪（retention_days），
// 两者可同时指定。需要确认短语。preserve_admin 控制是否保留管理员操作日志（对条数与天数裁剪都生效）。
func (a *App) handlePruneAuditLogs(w http.ResponseWriter, r *http.Request, _ Params) {
	payload := decodeMap(r)
	if stringValue(payload, "confirm") != confirmPruneAuditLogs {
		failWithCode(w, http.StatusBadRequest, ErrBadRequest, "需要确认短语 confirm="+confirmPruneAuditLogs)
		return
	}

	maxEntries := clamp(intValue(payload, "max_entries", 0), 0, 100000)
	retentionDays := clamp(intValue(payload, "retention_days", 0), 0, 3650)
	if maxEntries == 0 && retentionDays == 0 {
		failWithCode(w, http.StatusBadRequest, ErrBadRequest, "请指定 max_entries 或 retention_days")
		return
	}
	preserveAdmin := boolValue(payload, "preserve_admin", true)
	cutoff := int64(0)
	if retentionDays > 0 {
		cutoff = time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
	}
	result, err := a.store().PruneAuditLogsWithPolicy(store.AuditLogPruneOptions{
		MaxEntries:    maxEntries,
		CutoffUnix:    cutoff,
		PreserveAdmin: preserveAdmin,
	})
	if err != nil {
		failWithCode(w, http.StatusInternalServerError, ErrInternal, "裁剪失败")
		return
	}
	a.auditAuditLogMaintenance(r, "prune_audit_logs", 0, map[string]any{
		"max_entries":      maxEntries,
		"retention_days":   retentionDays,
		"preserve_admin":   preserveAdmin,
		"removed_by_limit": result.RemovedByLimit,
		"removed_by_age":   result.RemovedByAge,
	})
	logs := []string{}
	if maxEntries > 0 {
		logs = append(logs, fmt.Sprintf("保留最近 %d 条，删除 %d 条（保留管理员=%v）", maxEntries, result.RemovedByLimit, preserveAdmin))
	}
	if retentionDays > 0 {
		logs = append(logs, fmt.Sprintf("删除 %d 天前 %d 条（保留管理员=%v）", retentionDays, result.RemovedByAge, preserveAdmin))
	}
	ok(w, "审计日志已清理", map[string]any{
		"current": a.store().AuditLogCount(),
		"logs":    logs,
	})
}

// auditAuditLogMaintenance 在删除 / 清空 / 裁剪审计日志之后写自保记录。这些 action
// 在 store 层受保护，不会被后续任何删除操作带走。审计总开关关闭时仍写一条 zap
// 运行日志，保证至少留下操作者与范围。
func (a *App) auditAuditLogMaintenance(r *http.Request, action string, targetUID int64, detail map[string]any) {
	p := current(r)
	zap.L().Warn("audit log maintenance",
		zap.String("action", action),
		zap.Int64("operator_uid", p.User.UID),
		zap.String("operator", p.User.Username),
		zap.Any("detail", detail))
	a.audit(r, action, "admin", targetUID, detail)
}

func auditLogDTO(log store.AuditLog) map[string]any {
	source := log.Source
	if source == "" {
		switch {
		case strings.EqualFold(log.IP, "telegram"):
			source = "telegram"
		case strings.EqualFold(log.Category, "system") && log.UID == 0:
			source = "system"
		default:
			source = "http"
		}
	}
	return map[string]any{
		"id":         log.ID,
		"uid":        log.UID,
		"username":   log.Username,
		"action":     log.Action,
		"category":   log.Category,
		"source":     source,
		"method":     log.Method,
		"target_uid": zeroNil(log.TargetUID),
		"detail":     log.Detail,
		"ip":         log.IP,
		"created_at": log.CreatedAt,
	}
}
