package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const pgAuditLogTimeout = 10 * time.Second

// AuditLogQuery describes a bounded server-side audit log query. Offset and
// Limit are applied after filtering and sorting; Limit <= 0 returns no rows but
// still computes Total.
type AuditLogQuery struct {
	Category       string
	Action         string
	Source         string // http / telegram / scheduler / system；空表示不筛选
	UID            int64
	TargetUID      int64
	From           int64
	To             int64
	Search         string
	ActionKeywords []string
	SortBy         string
	Order          string
	Offset         int
	Limit          int
}

type AuditLogPage struct {
	Logs  []AuditLog
	Total int
}

// QueryAuditLogs filters, sorts, and paginates in PostgreSQL. Audit history is
// intentionally outside the single state document so listing it does not copy
// the complete in-memory State and appending it does not rewrite twilight_state.
func (s *Store) QueryAuditLogs(query AuditLogQuery) AuditLogPage {
	query = normalizeAuditLogQuery(query)
	page := AuditLogPage{Logs: []AuditLog{}}
	if s == nil || s.db == nil {
		return page
	}
	where, args := auditLogWhereSQL(query)
	ctx, cancel := context.WithTimeout(context.Background(), pgAuditLogTimeout)
	defer cancel()
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM twilight_audit_logs`+where, args...).Scan(&page.Total); err != nil {
		return AuditLogPage{Logs: []AuditLog{}}
	}
	if query.Limit <= 0 || query.Offset >= page.Total {
		return page
	}
	limitArg := len(args) + 1
	offsetArg := limitArg + 1
	args = append(args, query.Limit, query.Offset)
	rows, err := s.db.QueryContext(ctx, `
SELECT id, uid, username, action, category, source, method, target_uid,
       COALESCE(detail, '{}'::jsonb)::text, ip, created_at
FROM twilight_audit_logs`+where+`
ORDER BY `+auditLogOrderSQL(query.SortBy, query.Order)+`
LIMIT $`+strconv.Itoa(limitArg)+` OFFSET $`+strconv.Itoa(offsetArg), args...)
	if err != nil {
		return AuditLogPage{Logs: []AuditLog{}, Total: page.Total}
	}
	defer rows.Close()
	for rows.Next() {
		entry, scanErr := scanAuditLog(rows.Scan)
		if scanErr == nil {
			page.Logs = append(page.Logs, entry)
		}
	}
	return page
}

type auditLogScanner func(dest ...any) error

func scanAuditLog(scan auditLogScanner) (AuditLog, error) {
	var entry AuditLog
	var detailText string
	if err := scan(&entry.ID, &entry.UID, &entry.Username, &entry.Action, &entry.Category, &entry.Source, &entry.Method, &entry.TargetUID, &detailText, &entry.IP, &entry.CreatedAt); err != nil {
		return AuditLog{}, err
	}
	if detailText != "" && detailText != "{}" {
		if err := json.Unmarshal([]byte(detailText), &entry.Detail); err != nil {
			return AuditLog{}, err
		}
	}
	return entry, nil
}

func auditLogWhereSQL(query AuditLogQuery) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 8)
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if query.Category != "" {
		add("LOWER(category) = $%d", query.Category)
	}
	if query.Action != "" {
		add("LOWER(action) = $%d", query.Action)
	}
	if query.Source != "" {
		add("LOWER(source) = $%d", query.Source)
	}
	if query.UID > 0 {
		add("uid = $%d", query.UID)
	}
	if query.TargetUID > 0 {
		add("target_uid = $%d", query.TargetUID)
	}
	if query.From > 0 {
		add("created_at >= $%d", query.From)
	}
	if query.To > 0 {
		add("created_at <= $%d", query.To)
	}
	if len(query.ActionKeywords) > 0 {
		parts := make([]string, 0, len(query.ActionKeywords))
		for _, keyword := range query.ActionKeywords {
			if keyword == "" {
				continue
			}
			args = append(args, auditLogLikePattern(keyword))
			parts = append(parts, "LOWER(action) LIKE $"+strconv.Itoa(len(args))+` ESCAPE E'\\'`)
		}
		if len(parts) > 0 {
			clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
		}
	}
	if query.Search != "" {
		args = append(args, auditLogLikePattern(query.Search))
		placeholder := "$" + strconv.Itoa(len(args))
		if strings.Contains(query.Search, " ") {
			clauses = append(clauses, `LOWER(CONCAT_WS(' ', username, action, category, source, method, ip, uid::text, target_uid::text)) LIKE `+placeholder+` ESCAPE E'\\'`)
		} else {
			fields := []string{"username", "action", "category", "source", "method", "ip", "uid::text", "target_uid::text"}
			parts := make([]string, 0, len(fields))
			for _, field := range fields {
				parts = append(parts, "LOWER("+field+") LIKE "+placeholder+` ESCAPE E'\\'`)
			}
			clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
		}
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func auditLogLikePattern(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return "%" + strings.ToLower(value) + "%"
}

func auditLogOrderSQL(sortBy, order string) string {
	direction := "DESC"
	if order == "asc" {
		direction = "ASC"
	}
	field := "created_at"
	switch sortBy {
	case "id", "uid", "target_uid":
		field = sortBy
	case "action", "category", "source", "method", "username", "ip":
		field = "LOWER(" + sortBy + ")"
	}
	return field + " " + direction + ", id " + direction
}

func normalizeAuditLogQuery(query AuditLogQuery) AuditLogQuery {
	query.Category = strings.ToLower(strings.TrimSpace(query.Category))
	query.Action = strings.ToLower(strings.TrimSpace(query.Action))
	query.Source = strings.ToLower(strings.TrimSpace(query.Source))
	query.Search = strings.ToLower(strings.TrimSpace(query.Search))
	query.SortBy = normalizeAuditLogSortField(query.SortBy)
	if !strings.EqualFold(query.Order, "asc") {
		query.Order = "desc"
	} else {
		query.Order = "asc"
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	if query.Limit < 0 {
		query.Limit = 0
	}
	for i, keyword := range query.ActionKeywords {
		query.ActionKeywords[i] = strings.ToLower(strings.TrimSpace(keyword))
	}
	return query
}

func normalizeAuditLogSortField(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "id", "action", "category", "source", "method", "username", "uid", "target_uid", "ip":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "created_at"
	}
}

// 审计日志自保：删除单条、清空、裁剪（含排程 cleanup_audit_logs）会各写一条
// 以下 action 的系统记录。这些记录不能被单条删除、清空、按条数或按天数裁剪删掉，
// 否则被盗的管理员账号可以先删日志再删「删日志」这条记录，事后无从追查。
// action 名只由服务端代码写入（fallback 审计生成的 action 形如
// delete_admin_audit_logs_log_id，不会与此撞名）。
var protectedAuditActions = []string{"delete_audit_log", "clear_audit_logs", "prune_audit_logs", "cleanup_audit_logs"}

// ErrAuditLogProtected 表示目标是不可删除的审计自保记录。
var ErrAuditLogProtected = errors.New("audit log protected")

// IsProtectedAuditAction 报告 action 是否属于不可删除的审计自保记录。
func IsProtectedAuditAction(action string) bool {
	action = strings.ToLower(strings.TrimSpace(action))
	for _, candidate := range protectedAuditActions {
		if action == candidate {
			return true
		}
	}
	return false
}

// auditLogDeletableSQL 是所有删除语句共用的「可删除」条件。
func auditLogDeletableSQL() string {
	quoted := make([]string, 0, len(protectedAuditActions))
	for _, action := range protectedAuditActions {
		quoted = append(quoted, "'"+action+"'")
	}
	return "LOWER(action) NOT IN (" + strings.Join(quoted, ", ") + ")"
}

// auditLogLimitDeleteSQL 按条数保留最新 $1 条。preserveAdmin=true 时只在非管理员
// 记录里计数并删除：管理员操作不占名额，也不会被登录、fallback 等高频记录挤掉。
func auditLogLimitDeleteSQL(preserveAdmin bool) string {
	scope := auditLogDeletableSQL()
	if preserveAdmin {
		scope += " AND LOWER(category) <> 'admin'"
	}
	return `
WITH cutoff AS (
	SELECT MIN(id) AS min_id FROM (
		SELECT id FROM twilight_audit_logs WHERE ` + scope + ` ORDER BY id DESC LIMIT $1
	) latest
)
DELETE FROM twilight_audit_logs
WHERE ` + scope + ` AND id < COALESCE((SELECT min_id FROM cutoff), 0)`
}

type AuditLogPruneOptions struct {
	MaxEntries    int
	CutoffUnix    int64
	PreserveAdmin bool
}

type AuditLogPruneResult struct {
	RemovedByLimit int
	RemovedByAge   int
	Current        int
}

// PruneAuditLogsWithPolicy applies count and age retention in one mutation and
// one persistence cycle. Count retention runs first to preserve legacy behavior.
// PreserveAdmin 同时作用于按条数与按天数裁剪；审计自保记录永远不删。
func (s *Store) PruneAuditLogsWithPolicy(options AuditLogPruneOptions) (AuditLogPruneResult, error) {
	result := AuditLogPruneResult{}
	if options.MaxEntries <= 0 && options.CutoffUnix <= 0 {
		result.Current = s.AuditLogCount()
		return result, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgAuditLogTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if options.MaxEntries > 0 {
		res, execErr := tx.ExecContext(ctx, auditLogLimitDeleteSQL(options.PreserveAdmin), options.MaxEntries)
		if execErr != nil {
			return result, execErr
		}
		removed, _ := res.RowsAffected()
		result.RemovedByLimit = int(removed)
	}
	if options.CutoffUnix > 0 {
		query := `DELETE FROM twilight_audit_logs WHERE created_at < $1 AND ` + auditLogDeletableSQL()
		if options.PreserveAdmin {
			query += ` AND LOWER(category) <> 'admin'`
		}
		res, execErr := tx.ExecContext(ctx, query, options.CutoffUnix)
		if execErr != nil {
			return result, execErr
		}
		removed, _ := res.RowsAffected()
		result.RemovedByAge = int(removed)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM twilight_audit_logs`).Scan(&result.Current); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

// AddAuditLog appends one security audit row without touching twilight_state.
// Retention runs in the same transaction so a successful return means both the
// new event and the configured bound are durable.
// 写入时的条数上限只裁剪非管理员记录（见 auditLogLimitDeleteSQL），避免高频的
// 登录 / fallback 记录把管理员操作挤掉。
func (s *Store) AddAuditLog(entry AuditLog, limit int) error {
	if s == nil || s.db == nil {
		return ErrNotFound
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	detail, err := json.Marshal(entry.Detail)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgAuditLogTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO twilight_audit_logs
    (uid, username, action, category, source, method, target_uid, detail, ip, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10)`,
		entry.UID, entry.Username, entry.Action, entry.Category, entry.Source, entry.Method,
		entry.TargetUID, string(detail), entry.IP, entry.CreatedAt); err != nil {
		return err
	}
	if limit > 0 {
		if _, err := tx.ExecContext(ctx, auditLogLimitDeleteSQL(true), limit); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AuditActionCount 是审计表里出现过的一个 action 及其条数。
type AuditActionCount struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
}

// ListAuditActions 返回审计表里实际出现过的 action（按条数降序，最多 limit 个），
// 供前端筛选下拉使用，替代前端写死的少量 action。
func (s *Store) ListAuditActions(limit int) ([]AuditActionCount, error) {
	if s == nil || s.db == nil {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgAuditLogTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
SELECT LOWER(action) AS action, count(*) FROM twilight_audit_logs
GROUP BY LOWER(action) ORDER BY count(*) DESC, LOWER(action) ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditActionCount, 0)
	for rows.Next() {
		var item AuditActionCount
		if err := rows.Scan(&item.Action, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListAuditLogs returns all rows newest first. Runtime callers should prefer
// QueryAuditLogs so normal page reads remain bounded.
func (s *Store) ListAuditLogs() []AuditLog {
	if s == nil || s.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgAuditLogTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
SELECT id, uid, username, action, category, source, method, target_uid,
       COALESCE(detail, '{}'::jsonb)::text, ip, created_at
FROM twilight_audit_logs ORDER BY id DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]AuditLog, 0)
	for rows.Next() {
		entry, scanErr := scanAuditLog(rows.Scan)
		if scanErr == nil {
			out = append(out, entry)
		}
	}
	return out
}

// DeleteAuditLog 删除单条并返回被删记录（供调用方写自保审计）。审计自保记录
// 返回 ErrAuditLogProtected。
func (s *Store) DeleteAuditLog(id int64) (AuditLog, error) {
	if id <= 0 {
		return AuditLog{}, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgAuditLogTimeout)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `
DELETE FROM twilight_audit_logs WHERE id = $1 AND `+auditLogDeletableSQL()+`
RETURNING id, uid, username, action, category, source, method, target_uid,
       COALESCE(detail, '{}'::jsonb)::text, ip, created_at`, id)
	entry, err := scanAuditLog(row.Scan)
	if err == nil {
		return entry, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AuditLog{}, err
	}
	var exists bool
	if existsErr := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM twilight_audit_logs WHERE id = $1)`, id).Scan(&exists); existsErr == nil && exists {
		return AuditLog{}, ErrAuditLogProtected
	}
	return AuditLog{}, ErrNotFound
}

// ClearAuditLogs 清空全部可删除的审计记录并返回删除条数；审计自保记录保留。
// 原实现用 TRUNCATE ... RESTART IDENTITY，会连同「谁清空了日志」一起抹掉。
func (s *Store) ClearAuditLogs() (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pgAuditLogTimeout)
	defer cancel()
	result, err := s.db.ExecContext(ctx, `DELETE FROM twilight_audit_logs WHERE `+auditLogDeletableSQL())
	if err != nil {
		return 0, err
	}
	removed, _ := result.RowsAffected()
	return int(removed), nil
}

// PruneAuditLogs 按条数裁剪，preserveAdmin=true 时管理员记录不计数也不删除。
func (s *Store) PruneAuditLogs(keep int, preserveAdmin bool) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	result, err := s.PruneAuditLogsWithPolicy(AuditLogPruneOptions{MaxEntries: keep, PreserveAdmin: preserveAdmin})
	return result.RemovedByLimit, err
}

func (s *Store) PruneAuditLogsByAge(cutoffUnix int64, preserveAdmin bool) (int, error) {
	result, err := s.PruneAuditLogsWithPolicy(AuditLogPruneOptions{CutoffUnix: cutoffUnix, PreserveAdmin: preserveAdmin})
	return result.RemovedByAge, err
}

func (s *Store) AuditLogCount() int {
	if s == nil || s.db == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgAuditLogTimeout)
	defer cancel()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM twilight_audit_logs`).Scan(&count); err != nil {
		return 0
	}
	return count
}

// migrateLegacyAuditLogs moves historical state.audit_logs into the dedicated
// table. ON CONFLICT plus an exclusive table lock makes repeated or concurrent
// startup migrations safe; the state payload is cleared only after inserts are
// durable, so an interrupted migration can be retried without data loss.
func (s *Store) migrateLegacyAuditLogs(parent context.Context) error {
	if len(s.state.AuditLogs) == 0 {
		return nil
	}
	entries := append([]AuditLog(nil), s.state.AuditLogs...)
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `LOCK TABLE twilight_audit_logs IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	if err := insertAuditLogsTx(ctx, tx, entries); err != nil {
		return err
	}
	if err := syncAuditLogSequence(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mutateAndSaveLocked(func() error {
		s.state.AuditLogs = nil
		s.state.NextAuditLogID = 1
		return nil
	})
}

func insertAuditLogsTx(ctx context.Context, tx *sql.Tx, entries []AuditLog) error {
	if len(entries) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO twilight_audit_logs
    (id, uid, username, action, category, source, method, target_uid, detail, ip, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11)
ON CONFLICT (id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, entry := range entries {
		if entry.ID <= 0 {
			continue
		}
		detail, err := json.Marshal(entry.Detail)
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, entry.ID, entry.UID, entry.Username, entry.Action, entry.Category, entry.Source, entry.Method, entry.TargetUID, string(detail), entry.IP, entry.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func syncAuditLogSequence(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
SELECT setval(
	pg_get_serial_sequence('twilight_audit_logs', 'id'),
	COALESCE((SELECT MAX(id) FROM twilight_audit_logs), 1),
	EXISTS (SELECT 1 FROM twilight_audit_logs)
)`)
	return err
}
