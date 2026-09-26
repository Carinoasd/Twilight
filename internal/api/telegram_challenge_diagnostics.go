package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"
)

// Log bounded classifications only. Driver/upstream error text can contain
// connection strings, private hosts, request bodies or Telegram tokens.
func logTelegramChallengeFailure(operation string, err error) {
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
	zap.L().Warn("telegram challenge operation failed", fields...)
}
