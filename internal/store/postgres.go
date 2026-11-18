package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct{ pool *pgxpool.Pool }

//go:embed schema.sql
var schemaSQL string

func Open(ctx context.Context, url string) (*Postgres, error) {
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL 18 URL: %w", err)
	}
	config.MaxConns = 5
	config.ConnConfig.ConnectTimeout = 5 * time.Second
	config.ConnConfig.RuntimeParams["application_name"] = "dbsphere-go"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL 18 pool: %w", err)
	}
	store := &Postgres{pool: pool}
	if err := store.Health(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := store.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (s *Postgres) Migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin DbSphere schema migration: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1742026)"); err != nil {
		return fmt.Errorf("lock DbSphere schema migration: %w", err)
	}
	var migrationTable *string
	if err = tx.QueryRow(ctx, "SELECT to_regclass('public.dbsphere_schema_migrations')::text").Scan(&migrationTable); err != nil {
		return fmt.Errorf("check DbSphere migration table: %w", err)
	}
	current := false
	if migrationTable != nil {
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=13)").Scan(&current); err != nil {
			return fmt.Errorf("check DbSphere schema version: %w", err)
		}
	}
	if !current {
		if _, err = tx.Exec(ctx, schemaSQL); err != nil {
			return fmt.Errorf("apply DbSphere schema: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(13) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record DbSphere Go schema migration: %w", err)
		}
	}
	var tracePIDMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=14)").Scan(&tracePIDMigrated); err != nil {
		return fmt.Errorf("check trace backend PID migration: %w", err)
	}
	if !tracePIDMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE trace_spans ADD COLUMN IF NOT EXISTS backend_pid integer"); err != nil {
			return fmt.Errorf("migrate trace backend PID: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(14) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record trace backend PID migration: %w", err)
		}
	}
	var aiBudgetMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=15)").Scan(&aiBudgetMigrated); err != nil {
		return fmt.Errorf("check AI budget migration: %w", err)
	}
	if !aiBudgetMigrated {
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS evidence_ai_invocation_time ON evidence(observed_at) WHERE kind='ai_invocation'"); err != nil {
			return fmt.Errorf("index AI invocation budget: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(15) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record AI budget migration: %w", err)
		}
	}
	var auditMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=16)").Scan(&auditMigrated); err != nil {
		return fmt.Errorf("check admin audit migration: %w", err)
	}
	if !auditMigrated {
		if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS admin_audit (
			id uuid PRIMARY KEY, observed_at timestamptz NOT NULL DEFAULT now(),
			actor text NOT NULL, method text NOT NULL, path text NOT NULL,
			outcome text NOT NULL, status_code integer)`); err != nil {
			return fmt.Errorf("create admin audit: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS admin_audit_time ON admin_audit(observed_at DESC)"); err != nil {
			return fmt.Errorf("index admin audit: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(16) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record admin audit migration: %w", err)
		}
	}
	var eventAuditMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=17)").Scan(&eventAuditMigrated); err != nil {
		return fmt.Errorf("check event audit migration: %w", err)
	}
	if !eventAuditMigrated {
		if _, err = tx.Exec(ctx, "ALTER TABLE admin_audit ADD COLUMN IF NOT EXISTS event_id uuid"); err != nil {
			return fmt.Errorf("migrate event audit: %w", err)
		}
		if _, err = tx.Exec(ctx, "CREATE INDEX IF NOT EXISTS admin_audit_event_time ON admin_audit(event_id,observed_at DESC) WHERE event_id IS NOT NULL"); err != nil {
			return fmt.Errorf("index event audit: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(17) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record event audit migration: %w", err)
		}
	}
	var traceParentMigrated bool
	if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=18)").Scan(&traceParentMigrated); err != nil {
		return fmt.Errorf("check trace parent lookup migration: %w", err)
	}
	if !traceParentMigrated {
		if _, err = tx.Exec(ctx, `CREATE INDEX IF NOT EXISTS trace_spans_parent_lookup
			ON trace_spans(trace_id,parent_span_id) WHERE parent_span_id IS NOT NULL`); err != nil {
			return fmt.Errorf("index trace parent lookup: %w", err)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(18) ON CONFLICT DO NOTHING"); err != nil {
			return fmt.Errorf("record trace parent lookup migration: %w", err)
		}
	}
	if err := seedGoRules(ctx, tx); err != nil {
		return fmt.Errorf("seed supported Go alert rules: %w", err)
	}
	if err := seedGoInspections(ctx, tx); err != nil {
		return fmt.Errorf("seed Go inspection templates: %w", err)
	}
	for _, table := range []string{"samples", "events"} {
		now := time.Now().UTC()
		thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		for offset := range 2 {
			start := thisMonth.AddDate(0, offset, 0)
			end := start.AddDate(0, 1, 0)
			name := fmt.Sprintf("%s_%04d%02d", table, start.Year(), int(start.Month()))
			var present *string
			if err = tx.QueryRow(ctx, "SELECT to_regclass($1)::text", "public."+name).Scan(&present); err != nil {
				return fmt.Errorf("check %s partition: %w", name, err)
			}
			if present != nil {
				continue
			}
			query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
				name, table, start.Format(time.RFC3339), end.Format(time.RFC3339))
			if _, err = tx.Exec(ctx, query); err != nil {
				return fmt.Errorf("create %s partition: %w", name, err)
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit DbSphere schema migration: %w", err)
	}
	return nil
}

func (s *Postgres) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var version int
	if err := s.pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		return fmt.Errorf("check PostgreSQL 18 storage: %w", err)
	}
	if version/10000 != 18 {
		return errors.New("DbSphere long-term store requires PostgreSQL 18")
	}
	return nil
}

func (s *Postgres) AdminPasswordHash(ctx context.Context, bootstrapPassword string) (string, error) {
	var stored string
	err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='admin_password_hash'").Scan(&stored)
	if err == nil {
		return stored, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("read administrator password hash: %w", err)
	}
	if len(bootstrapPassword) < 12 {
		return "", errors.New("DBSPHERE_ADMIN_PASSWORD is required for first start")
	}
	hash, err := security.HashPassword(bootstrapPassword)
	if err != nil {
		return "", fmt.Errorf("hash administrator password: %w", err)
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO settings(key,value) VALUES('admin_password_hash',$1) ON CONFLICT(key) DO NOTHING", hash); err != nil {
		return "", fmt.Errorf("initialize administrator password: %w", err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='admin_password_hash'").Scan(&stored); err != nil {
		return "", fmt.Errorf("read initialized administrator password hash: %w", err)
	}
	return stored, nil
}

// ViewerPasswordHash reconciles the optional global read-only account with
// startup configuration. Removing the password disables the account.
func (s *Postgres) ViewerPasswordHash(ctx context.Context, bootstrapPassword string) (string, error) {
	var stored string
	err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='viewer_password_hash'").Scan(&stored)
	if err == nil {
		if bootstrapPassword == "" {
			_, err := s.pool.Exec(ctx, "DELETE FROM settings WHERE key='viewer_password_hash'")
			return "", err
		}
		if security.VerifyPassword(bootstrapPassword, stored) {
			return stored, nil
		}
		hash, err := security.HashPassword(bootstrapPassword)
		if err != nil {
			return "", err
		}
		if _, err = s.pool.Exec(ctx, "UPDATE settings SET value=$1 WHERE key='viewer_password_hash'", hash); err != nil {
			return "", err
		}
		return hash, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if bootstrapPassword == "" {
		return "", nil
	}
	if len(bootstrapPassword) < 12 {
		return "", errors.New("DBSPHERE_VIEWER_PASSWORD must contain at least 12 characters")
	}
	hash, err := security.HashPassword(bootstrapPassword)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO settings(key,value) VALUES('viewer_password_hash',$1) ON CONFLICT(key) DO NOTHING", hash); err != nil {
		return "", err
	}
	if err := s.pool.QueryRow(ctx, "SELECT value FROM settings WHERE key='viewer_password_hash'").Scan(&stored); err != nil {
		return "", err
	}
	return stored, nil
}

func (s *Postgres) Close() { s.pool.Close() }
