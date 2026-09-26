package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Challenges are ephemeral authentication state, never migration/backup data.
// Version 1 intentionally does not backfill legacy plaintext State.BindCodes.
func prepareTelegramChallengeSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize concurrent API/Bot startup DDL for this module.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(724193820)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS twilight_telegram_challenge_schema (
 id smallint PRIMARY KEY CHECK (id = 1), version integer NOT NULL
);
INSERT INTO twilight_telegram_challenge_schema (id, version) VALUES (1, 1) ON CONFLICT DO NOTHING;`); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT version FROM twilight_telegram_challenge_schema WHERE id = 1`).Scan(&version); err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("unsupported Telegram challenge schema version %d", version)
	}
	if _, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS twilight_telegram_challenges (
 id text PRIMARY KEY,
 token_hash text NOT NULL UNIQUE,
 owner_hash text NOT NULL DEFAULT '',
 uid bigint NOT NULL DEFAULT 0,
 scene text NOT NULL CHECK (scene IN ('register', 'user')),
 state text NOT NULL CHECK (state IN ('pending', 'verified', 'consumed', 'cancelled')),
 telegram_id bigint NOT NULL DEFAULT 0,
 telegram_username text NOT NULL DEFAULT '',
 created_at bigint NOT NULL,
 expires_at bigint NOT NULL,
 consumed_at bigint NOT NULL DEFAULT 0,
 result_uid bigint NOT NULL DEFAULT 0,
 revision bigint NOT NULL DEFAULT 1,
 error_code text NOT NULL DEFAULT '',
 error_message text NOT NULL DEFAULT '',
 error_status integer NOT NULL DEFAULT 0,
 retryable boolean NOT NULL DEFAULT false,
 expected_rebind_since bigint NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS twilight_telegram_challenges_expiry_idx ON twilight_telegram_challenges (expires_at);
CREATE INDEX IF NOT EXISTS twilight_telegram_challenges_uid_idx ON twilight_telegram_challenges (uid) WHERE uid <> 0;
CREATE INDEX IF NOT EXISTS twilight_telegram_challenges_owner_idx ON twilight_telegram_challenges (owner_hash) WHERE owner_hash <> '';
`); err != nil {
		return err
	}
	return tx.Commit()
}
