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
	if _, err = tx.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply DbSphere schema: %w", err)
	}
	if err := seedGoRules(ctx, tx); err != nil {
		return fmt.Errorf("seed supported Go alert rules: %w", err)
	}
	for _, table := range []string{"samples", "events"} {
		now := time.Now().UTC()
		thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		for offset := range 2 {
			start := thisMonth.AddDate(0, offset, 0)
			end := start.AddDate(0, 1, 0)
			name := fmt.Sprintf("%s_%04d%02d", table, start.Year(), int(start.Month()))
			query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
				name, table, start.Format(time.RFC3339), end.Format(time.RFC3339))
			if _, err = tx.Exec(ctx, query); err != nil {
				return fmt.Errorf("create %s partition: %w", name, err)
			}
		}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(13) ON CONFLICT DO NOTHING"); err != nil {
		return fmt.Errorf("record DbSphere Go schema migration: %w", err)
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

func (s *Postgres) Close() { s.pool.Close() }
