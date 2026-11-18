package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// attachDatabaseGrowth adds a one-day observed growth rate to the stored sample.
// It does not estimate filesystem free space or predict when a disk will fill.
func attachDatabaseGrowth(ctx context.Context, tx pgx.Tx, assetID string, metrics map[string]any, now time.Time) error {
	delete(metrics, "databaseGrowth")
	if stringValue(object(metrics["collector"])["pgStatus"]) != "ok" {
		return nil
	}
	current, ok := numeric(metrics["database_bytes"])
	if !ok || current < 0 || current > math.MaxInt64 || math.IsNaN(current) || math.IsInf(current, 0) {
		return nil
	}
	var baselineID, raw string
	var baselineAt time.Time
	err := tx.QueryRow(ctx, `SELECT id::text,metrics_json->>'database_bytes',collected_at
		FROM samples WHERE asset_id=$1 AND status='ok'
		AND metrics_json->'collector'->>'pgStatus'='ok'
		AND metrics_json->>'database_bytes' ~ '^[0-9]{1,19}$'
		AND collected_at BETWEEN $2::timestamptz - interval '25 hours'
			AND $2::timestamptz - interval '23 hours'
		ORDER BY abs(extract(epoch FROM collected_at - ($2::timestamptz - interval '24 hours'))),
			collected_at DESC LIMIT 1`, assetID, now).Scan(&baselineID, &raw, &baselineAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read PostgreSQL capacity baseline: %w", err)
	}
	baseline, err := strconv.ParseFloat(raw, 64)
	if err != nil || baseline < 0 || baseline > math.MaxInt64 || math.IsNaN(baseline) || math.IsInf(baseline, 0) {
		return nil
	}
	hours := now.Sub(baselineAt).Hours()
	if hours < 23 || hours > 25 {
		return nil
	}
	growth := math.Max(0, (current-baseline)*24/hours)
	metrics["databaseGrowth"] = map[string]any{
		"status": "ok", "growthBytesPerDay": math.Round(growth),
		"currentBytes": current, "baselineBytes": baseline,
		"baselineSampleId": baselineID, "baselineAt": baselineAt,
		"windowHours": hours, "method": "24 小时附近有效 PG 容量样本的实际增长速率",
	}
	return nil
}
