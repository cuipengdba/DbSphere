package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Postgres struct{ pool *pgxpool.Pool }

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
	return store, nil
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

func (s *Postgres) Close() { s.pool.Close() }
