package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
)

const kingbaseLockGroup = "kingbase_lock_wait"

func (s *Postgres) checkKingbase(ctx context.Context, id string, target collector.Target, password string, lease *CollectionLease, interval time.Duration) (CheckResult, error) {
	started := time.Now()
	if lease != nil {
		lease.startedAt = started
	}
	snapshot, err := (collector.KingbaseAdapter{}).Collect(ctx, target, password)
	lease.recordQueryDuration(started)
	group, reason := "", ""
	metrics := snapshot.Metrics
	if err != nil {
		if errors.Is(err, collector.ErrKingbaseVersion) || errors.Is(err, collector.ErrKingbaseReadOnly) {
			group, reason = "telemetry", err.Error()
		} else {
			group, reason = collectionError(err)
			reason = strings.Replace(reason, "PostgreSQL", "KingbaseES", 1)
		}
		metrics = map[string]any{"collector": map[string]any{"kingbaseStatus": group}}
	} else {
		metrics["adapter"] = snapshot.Report
		metrics["collector"] = map[string]any{"kingbaseStatus": "ok"}
	}
	return s.recordCheckWithLease(ctx, id, metrics, group, reason, time.Now().UTC(), lease, interval)
}

func recordKingbaseLockEvent(ctx context.Context, tx pgx.Tx, assetID, sampleID string, metrics map[string]any, now time.Time) error {
	report, ok := metrics["adapter"].(collectoradapter.Report)
	details, valid := metrics["kingbase"].(collector.KingbaseMetrics)
	if !ok || !valid {
		return nil
	}
	for _, signal := range report.Signals {
		if signal.Key == "lockWaits" && signal.Status == "ok" {
			return recordEngineLockEvent(ctx, tx, assetID, sampleID, now, engineLockEvidence{
				group: kingbaseLockGroup, title: "金仓 V9 锁等待", source: signal.Source,
				waits: details.Waits, count: len(details.Waits), truncated: details.WaitsTruncated,
			})
		}
	}
	return nil
}
