package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func migrateSQLEvidenceIdentity(ctx context.Context, tx pgx.Tx) error {
	var ready bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM dbsphere_schema_migrations WHERE version=38)`).Scan(&ready); err != nil {
		return fmt.Errorf("check SQL evidence identity migration: %w", err)
	}
	if ready {
		return nil
	}
	_, err := tx.Exec(ctx, `ALTER TABLE slow_log_entries ADD COLUMN database_name text, ADD COLUMN record_key text;
	ALTER TABLE sql_plan_snapshots ADD COLUMN database_name text;
	CREATE UNIQUE INDEX slow_log_session_record ON slow_log_entries(asset_id,record_key) WHERE record_key IS NOT NULL;
	INSERT INTO dbsphere_schema_migrations(version) VALUES(38)`)
	if err != nil {
		return fmt.Errorf("migrate SQL evidence identity: %w", err)
	}
	return nil
}

func canonicalSQLQueryID(input string) string {
	if !slowQueryID.MatchString(input) {
		return ""
	}
	if n, err := strconv.ParseInt(input, 10, 64); err == nil {
		if n != 0 {
			return strconv.FormatInt(n, 10)
		}
		return ""
	}
	if n, err := strconv.ParseUint(input, 10, 64); err == nil {
		return strconv.FormatInt(int64(n), 10)
	}
	return ""
}
