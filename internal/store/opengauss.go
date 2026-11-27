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

const openGaussLockGroup = "opengauss_lock_wait"

func (s *Postgres) checkOpenGauss(ctx context.Context, id string, target collector.Target, password string, lease *CollectionLease, interval time.Duration, infra nativeInfraTarget) (CheckResult, error) {
	snapshot, err := s.collectNativeObservation(ctx, id, target.Port, infra, lease, func() (collectoradapter.Snapshot, error) {
		return (collector.OpenGaussAdapter{}).Collect(ctx, target, password)
	})
	group, reason := "", ""
	status := "ok"
	switch {
	case errors.Is(err, collector.ErrOpenGaussVersion), errors.Is(err, collector.ErrOpenGaussReadOnly):
		group, reason = "telemetry", err.Error()
	case err != nil:
		group, reason = collectionError(err)
		reason = strings.Replace(reason, "PostgreSQL", "openGauss", 1)
	default:
		snapshot.Metrics["adapter"] = snapshot.Report
	}
	if group != "" {
		status = group
	}
	snapshot.Metrics["collector"] = map[string]any{"opengaussStatus": status}
	return s.recordCheckWithLease(ctx, id, snapshot.Metrics, group, reason, time.Now().UTC(), lease, interval)
}

func recordOpenGaussLockEvent(ctx context.Context, tx pgx.Tx, assetID, sampleID string, metrics map[string]any, now time.Time) error {
	report, ok := metrics["adapter"].(collectoradapter.Report)
	details, valid := metrics["opengauss"].(collector.OpenGaussMetrics)
	if !ok || !valid {
		return nil
	}
	for _, signal := range report.Signals {
		if signal.Key == "lockWaits" && signal.Status == "ok" {
			return recordEngineLockEvent(ctx, tx, assetID, sampleID, now, engineLockEvidence{
				group: openGaussLockGroup, title: "openGauss 锁等待", source: signal.Source,
				waits: details.Waits, count: len(details.Waits), truncated: details.WaitsTruncated,
			})
		}
	}
	return nil
}
