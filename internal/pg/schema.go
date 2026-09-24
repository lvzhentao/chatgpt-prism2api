package pg

import (
	"context"
	"fmt"
	"strings"
)

var migrateStatements = []string{
	`CREATE TABLE IF NOT EXISTS request_logs (
		id BIGSERIAL PRIMARY KEY,
		time TIMESTAMPTZ NOT NULL,
		ip TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL DEFAULT '',
		path TEXT NOT NULL DEFAULT '',
		status INT NOT NULL DEFAULT 0,
		account TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		prompt_tokens INT NOT NULL DEFAULT 0,
		completion_tokens INT NOT NULL DEFAULT 0,
		total_tokens INT NOT NULL DEFAULT 0,
		latency_ms BIGINT NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		client_key_id BIGINT,
		fail_class TEXT NOT NULL DEFAULT '',
		retry_count INT NOT NULL DEFAULT 0,
		request_id TEXT NOT NULL DEFAULT '',
		stream BOOLEAN NOT NULL DEFAULT FALSE,
		headers JSONB,
		request_body TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS request_logs_time_idx ON request_logs (time DESC)`,
	`CREATE INDEX IF NOT EXISTS request_logs_account_time_idx ON request_logs (account, time DESC)`,
	`CREATE INDEX IF NOT EXISTS request_logs_request_id_idx ON request_logs (request_id)`,
	`CREATE TABLE IF NOT EXISTS batches (
		id TEXT PRIMARY KEY,
		processing_status TEXT NOT NULL,
		payload JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE INDEX IF NOT EXISTS batches_updated_idx ON batches (updated_at DESC)`,
	`CREATE TABLE IF NOT EXISTS documents (
		kind TEXT NOT NULL,
		id TEXT NOT NULL,
		payload JSONB NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (kind, id)
	)`,
}

// Migrate 幂等建表。
func (d *DB) Migrate(ctx context.Context) error {
	if d == nil || d.pool == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, stmt := range migrateStatements {
		if _, err := d.pool.Exec(ctx, strings.TrimSpace(stmt)); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}
