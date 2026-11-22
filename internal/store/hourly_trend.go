package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const trendCounterLookback = 10 * time.Minute

//go:embed hourly_trend.sql
var hourlyTrendSQL string

func migrateHourlyTrend(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=35)").Scan(&applied); err != nil {
		return fmt.Errorf("check hourly trend migration: %w", err)
	}
	if applied {
		return nil
	}
	if _, err := tx.Exec(ctx, hourlyTrendSQL); err != nil {
		return fmt.Errorf("migrate hourly trends: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO dbsphere_schema_migrations(version) VALUES(35)"); err != nil {
		return fmt.Errorf("record hourly trend migration: %w", err)
	}
	return nil
}

// AdvanceHourlyTrend materializes closed UTC hours with the same counter and gap
// calculations used for interactive reads. Its cursor and invalidations are durable.
func (s *Postgres) AdvanceHourlyTrend(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var acquired bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(1742035)`).Scan(&acquired); err != nil || !acquired {
		return false, err
	}
	var first, through, closed, horizon time.Time
	if err := tx.QueryRow(ctx, `SELECT first_hour,through_hour,date_trunc('hour',now(),'UTC'),
		date_trunc('hour',now(),'UTC')-interval '30 days' FROM monitoring_hourly_status WHERE singleton`).
		Scan(&first, &through, &closed, &horizon); err != nil {
		return false, err
	}
	// A long shutdown can put the entire old coverage window beyond retention.
	if through.Before(horizon) {
		first, through = closed, closed
	} else if first.Before(horizon) {
		first = horizon
	}
	var start, end, dirty time.Time
	err = tx.QueryRow(ctx, `SELECT bucket FROM monitoring_hourly_dirty
		WHERE bucket >= $1 AND bucket < $2 ORDER BY bucket DESC LIMIT 1`, first, through).Scan(&dirty)
	switch {
	case err == nil:
		start, end = dirty, dirty.Add(time.Hour)
	case !errors.Is(err, pgx.ErrNoRows):
		return false, err
	case through.Before(closed):
		start, end = through, earlierTime(through.Add(3*time.Hour), closed)
		through = end
	case first.After(horizon):
		start, end = laterTime(first.Add(-3*time.Hour), horizon), first
		first = start
	default:
		if _, err := tx.Exec(ctx, `WITH trimmed AS (
			DELETE FROM monitoring_hourly_buckets WHERE bucket < $1)
			DELETE FROM monitoring_hourly_dirty WHERE bucket < $1`, horizon); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	if err := rebuildHourlyTrend(ctx, tx, start, end); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE monitoring_hourly_status SET first_hour=$1,through_hour=$2 WHERE singleton`, first, through); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

func rebuildHourlyTrend(ctx context.Context, tx pgx.Tx, start, end time.Time) error {
	// Remove invalidations before taking the sample snapshot. A concurrent sample
	// write either becomes visible to the rebuild or commits a fresh invalidation.
	if _, err := tx.Exec(ctx, `DELETE FROM monitoring_hourly_dirty WHERE bucket >= $1 AND bucket < $2`, start, end); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM monitoring_hourly_buckets WHERE bucket >= $1 AND bucket < $2`, start, end); err != nil {
		return err
	}
	var ids []string
	if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(id::text),'{}') FROM assets`).Scan(&ids); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO monitoring_hourly_buckets(asset_id,bucket,value)
		SELECT (value->>'assetId')::uuid,(value->>'bucket')::timestamptz,value
		FROM (`+monitoringBucketsSQL+`) AS computed(value) WHERE (value->>'bucket')::timestamptz >= $5`,
		ids, start.Add(-trendCounterLookback), end, 60, start)
	return err
}

func laterTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlierTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

type trendBucketRow struct {
	AssetID string    `json:"assetId"`
	Bucket  time.Time `json:"bucket"`
	value   json.RawMessage
}

func (s *Postgres) HourlyTrendCoverage(ctx context.Context) (json.RawMessage, error) {
	var report json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object('from',first_hour,'through',through_hour,
		'pendingHours',greatest(0,720-extract(epoch FROM through_hour-first_hour)::int / 3600),
		'dirtyHours',(SELECT count(DISTINCT bucket) FROM monitoring_hourly_dirty WHERE bucket >= first_hour AND bucket < through_hour))
		FROM monitoring_hourly_status WHERE singleton`).Scan(&report)
	return report, err
}

func (s *Postgres) readHourlyTrend(ctx context.Context, ids []string, from, to time.Time, long bool) ([]json.RawMessage, error) {
	// Keep the coverage decision, late-sample markers and raw edge buckets on one snapshot.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if long {
		if _, err := tx.Exec(ctx, "SET LOCAL work_mem='24MB'"); err != nil {
			return nil, err
		}
	}
	var first, through time.Time
	if err := tx.QueryRow(ctx, `SELECT greatest(first_hour,date_trunc('hour',now(),'UTC')-interval '30 days'),
		least(through_hour,date_trunc('hour',now(),'UTC')) FROM monitoring_hourly_status WHERE singleton`).Scan(&first, &through); err != nil {
		return nil, err
	}
	// The first raw interval contains every bucket whose rate might depend on a
	// predecessor earlier than the caller's lower bound.
	earliest := from.UTC().Add(trendCounterLookback)
	start := earliest.Truncate(time.Hour)
	if start.Before(earliest) {
		start = start.Add(time.Hour)
	}
	start = laterTime(start, first)
	end := earlierTime(to.UTC().Truncate(time.Hour), through)
	if !end.After(start) {
		return readMonitoringBucketRows(ctx, tx, monitoringBucketsSQL, ids, from, to, 60)
	}
	var dirty bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM monitoring_hourly_dirty
		WHERE bucket >= $1 AND bucket < $2 AND asset_id=ANY($3::uuid[]))`, start, end, ids).Scan(&dirty); err != nil {
		return nil, err
	}
	if dirty {
		return readMonitoringBucketRows(ctx, tx, monitoringBucketsSQL, ids, from, to, 60)
	}
	head, err := readMonitoringBucketRows(ctx, tx, monitoringBucketsSQL, ids, from, start, 60)
	if err != nil {
		return nil, err
	}
	stored, err := readMonitoringBucketRows(ctx, tx, `SELECT value FROM monitoring_hourly_buckets
		WHERE asset_id=ANY($1::uuid[]) AND bucket >= $2 AND bucket < $3`, ids, start, end)
	if err != nil {
		return nil, err
	}
	var tail []json.RawMessage
	if end.Before(to) {
		tail, err = readMonitoringBucketRows(ctx, tx, monitoringBucketsSQL, ids, end.Add(-trendCounterLookback), to, 60)
		if err != nil {
			return nil, err
		}
	}
	return mergeTrendBuckets(head, stored, tail, end)
}

func mergeTrendBuckets(head, stored, tail []json.RawMessage, end time.Time) ([]json.RawMessage, error) {
	ordered := make([]trendBucketRow, 0, len(head)+len(stored)+len(tail))
	for segment, values := range [][]json.RawMessage{head, stored, tail} {
		for _, value := range values {
			entry := trendBucketRow{value: value}
			if err := json.Unmarshal(value, &entry); err != nil {
				return nil, err
			}
			if segment != 2 || !entry.Bucket.Before(end) {
				ordered = append(ordered, entry)
			}
		}
	}
	slices.SortFunc(ordered, func(a, b trendBucketRow) int {
		if order := strings.Compare(a.AssetID, b.AssetID); order != 0 {
			return order
		}
		return a.Bucket.Compare(b.Bucket)
	})
	result := make([]json.RawMessage, len(ordered))
	for i, entry := range ordered {
		result[i] = entry.value
	}
	return result, nil
}
