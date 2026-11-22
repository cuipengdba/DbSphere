package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed trend_projection.sql
var trendProjectionSQL string

func migrateTrendProjection(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=34)").Scan(&applied); err != nil {
		return fmt.Errorf("check trend projection migration: %w", err)
	}
	if applied {
		return nil
	}
	if _, err := tx.Exec(ctx, trendProjectionSQL); err != nil {
		return fmt.Errorf("migrate trend projection: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(34)"); err != nil {
		return fmt.Errorf("record trend projection migration: %w", err)
	}
	return nil
}

// AdvanceTrendProjection copies three hours of earlier history in one transaction.
// Normal sample writes and deletes keep the projection current independently.
func (s *Postgres) AdvanceTrendProjection(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var acquired bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(1742034)`).Scan(&acquired); err != nil || !acquired {
		return false, err
	}
	var from, to, horizon time.Time
	if err := tx.QueryRow(ctx, `SELECT cursor-interval '3 hours', cursor, date_trunc('hour',now(),'UTC')-interval '31 days'
		FROM monitoring_trend_progress WHERE singleton`).Scan(&from, &to, &horizon); err != nil {
		return false, err
	}
	if !to.After(horizon) {
		return true, nil
	}
	if from.Before(horizon) {
		from = horizon
	}
	if _, err := tx.Exec(ctx, `INSERT INTO monitoring_trend_points
		SELECT s.asset_id,s.collected_at,s.id,s.status,f.* FROM assets a
		CROSS JOIN LATERAL (SELECT id,asset_id,collected_at,status,metrics_json FROM samples
			WHERE asset_id=a.id AND collected_at >= $1 AND collected_at < $2 OFFSET 0) s
		CROSS JOIN LATERAL dbsphere_trend_fields(s.status,s.metrics_json) f
		ON CONFLICT DO NOTHING`, from, to); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE monitoring_trend_progress SET cursor=$1 WHERE singleton`, from); err != nil {
		return false, err
	}
	return !from.After(horizon), tx.Commit(ctx)
}

func (s *Postgres) PruneTrendProjection(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM monitoring_trend_points WHERE ctid IN (
		SELECT ctid FROM monitoring_trend_points WHERE collected_at < date_trunc('hour',now(),'UTC')-interval '31 days'
		ORDER BY collected_at LIMIT 50000)`)
	return err
}
