package store

import (
	"context"
	"errors"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
)

const dm8LockGroup = "dm8_lock_wait"

func (s *Postgres) checkDM8(ctx context.Context, id string, target collector.Target, password string, lease *CollectionLease, interval time.Duration, infra nativeInfraTarget) (CheckResult, error) {
	snapshot, err := s.collectNativeObservation(ctx, id, target.Port, infra, lease, func() (collectoradapter.Snapshot, error) {
		return (collector.DM8Adapter{}).Collect(ctx, target, password)
	})
	if err != nil {
		reason := "达梦探针或实例暂不可达"
		if errors.Is(err, collector.ErrDM8Unconfigured) {
			reason = "达梦采集探针未配置"
		} else if errors.Is(err, collector.ErrDM8Connection) {
			reason = "达梦连接或监控授权未通过"
		}
		snapshot.Metrics["collector"] = map[string]any{"dm8Status": "telemetry"}
		return s.recordCheckWithLease(ctx, id, snapshot.Metrics, "telemetry", reason, time.Now().UTC(), lease, interval)
	}
	snapshot.Metrics["adapter"] = snapshot.Report
	snapshot.Metrics["collector"] = map[string]any{"dm8Status": "ok"}
	return s.recordCheckWithLease(ctx, id, snapshot.Metrics, "", "", time.Now().UTC(), lease, interval)
}

func recordDM8LockEvent(ctx context.Context, tx pgx.Tx, assetID, sampleID string, metrics map[string]any, now time.Time) error {
	details, ok := metrics["dm8"].(collector.DM8Metrics)
	if !ok {
		return nil
	}
	for _, signal := range details.Signals {
		if signal.Key == "lockWaits" && signal.Status == "ok" {
			return recordEngineLockEvent(ctx, tx, assetID, sampleID, now, engineLockEvidence{
				group: dm8LockGroup, title: "达梦 DM8 事务锁等待", source: signal.Source,
				waits: details.Waits, count: len(details.Waits), truncated: details.WaitsTruncated,
			})
		}
	}
	return nil
}
