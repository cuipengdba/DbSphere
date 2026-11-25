package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
)

const mysqlLockGroup = "mysql_lock_wait"
const mysqlReplicaGroup = "mysql_ha_replication"

type mysqlInterval struct {
	QuestionsPerSecond *float64 `json:"questionsPerSecond,omitempty"`
	BufferHitPercent   *float64 `json:"bufferHitPercent,omitempty"`
}

func readMySQLInterval(current collector.MySQLMetrics, previousJSON []byte, elapsed, interval time.Duration) *mysqlInterval {
	maximum := 15 * time.Minute
	if interval > 0 {
		maximum = 2 * interval
	}
	var previous struct {
		MySQL collector.MySQLMetrics `json:"mysql"`
	}
	if elapsed <= 0 || elapsed > maximum || json.Unmarshal(previousJSON, &previous) != nil {
		return nil
	}
	old := previous.MySQL
	if current.ServerUUID == "" || current.ServerUUID != old.ServerUUID || current.UptimeSeconds == nil || old.UptimeSeconds == nil ||
		*current.UptimeSeconds < *old.UptimeSeconds || float64(*current.UptimeSeconds-*old.UptimeSeconds)+5 < elapsed.Seconds() {
		return nil
	}
	result := &mysqlInterval{}
	if before, after := old.Questions, current.Questions; before != nil && after != nil && *after >= *before {
		rate := float64(*after-*before) / elapsed.Seconds()
		result.QuestionsPerSecond = &rate
	}
	a, b := current.BufferPool, old.BufferPool
	if a != nil && b != nil && a.PhysicalReads != nil && b.PhysicalReads != nil && a.ReadRequests != nil && b.ReadRequests != nil &&
		*a.PhysicalReads >= *b.PhysicalReads && *a.ReadRequests > *b.ReadRequests {
		misses, reads := *a.PhysicalReads-*b.PhysicalReads, *a.ReadRequests-*b.ReadRequests
		if misses <= reads {
			hits := 100 * float64(reads-misses) / float64(reads)
			result.BufferHitPercent = &hits
		}
	}
	if result.QuestionsPerSecond == nil && result.BufferHitPercent == nil {
		return nil
	}
	return result
}

func (s *Postgres) checkMySQL(ctx context.Context, id string, target collector.Target, password string, lease *CollectionLease, interval time.Duration, infra nativeInfraTarget) (CheckResult, error) {
	snapshot, err := s.collectNativeObservation(ctx, id, target.Port, infra, lease, func() (collectoradapter.Snapshot, error) {
		return (collector.MySQLAdapter{}).Collect(ctx, target, password)
	})
	group, reason := "", ""
	metrics := snapshot.Metrics
	if err != nil {
		group, reason = mysqlCollectionError(err)
		metrics["collector"] = map[string]any{"mysqlStatus": group}
	} else {
		metrics["adapter"] = snapshot.Report
		metrics["collector"] = map[string]any{"mysqlStatus": "ok"}
	}
	return s.recordCheckWithLease(ctx, id, metrics, group, reason, time.Now().UTC(), lease, interval)
}

func recordMySQLReplicationEvent(ctx context.Context, tx pgx.Tx, assetID, sampleID string, metrics map[string]any, now time.Time) error {
	details, ok := metrics["mysql"].(collector.MySQLMetrics)
	report, haveReport := metrics["adapter"].(collectoradapter.Report)
	if !ok || !haveReport || len(details.Replication) == 0 {
		return nil
	}
	source, workObserved := "", false
	for _, signal := range report.Signals {
		if signal.Key == "replication" && signal.Status == "ok" {
			source = signal.Source
		}
		if signal.Key == "replicaWorkers" && signal.Status == "ok" {
			workObserved = true
		}
	}
	if source == "" {
		return nil
	}
	abnormal := make([]collector.MySQLReplicationChannel, 0)
	allHealthy := !details.ReplicationTruncated
	for _, channel := range details.Replication {
		workerError := false
		for _, worker := range channel.Workers {
			workerError = workerError || (worker.LastError != 0 && worker.State == "OFF")
		}
		if channel.Receiver != "ON" || (channel.Applier != "ON" && channel.Applier != "UNKNOWN") || workerError {
			abnormal = append(abnormal, channel)
		}
		allHealthy = allHealthy && channel.Receiver == "ON" && channel.Applier == "ON" && !workerError
	}
	if len(abnormal) == 0 && (!allHealthy || !workObserved) {
		return nil
	}
	return recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, mysqlReplicaGroup, "MySQL 复制通道异常", source,
		len(abnormal) > 0, map[string]any{"channels": abnormal, "serverUuid": details.ServerUUID},
		map[string]any{"channels": details.Replication, "serverUuid": details.ServerUUID})
}

func mysqlCollectionError(err error) (string, string) {
	var certificate *tls.CertificateVerificationError
	var remote *mysql.MySQLError
	switch {
	case errors.Is(err, collector.ErrMySQLVersion), errors.Is(err, collector.ErrMySQLReadOnly):
		return "telemetry", err.Error()
	case errors.As(err, &certificate):
		return "telemetry", "MySQL TLS certificate or hostname verification failed"
	case errors.As(err, &remote):
		if remote.Number == 1040 || remote.Number == 1053 {
			return "availability", fmt.Sprintf("MySQL temporarily unavailable (error %d)", remote.Number)
		}
		return "telemetry", fmt.Sprintf("MySQL authentication or collection failed (error %d)", remote.Number)
	default:
		return "availability", "MySQL connection or collection failed"
	}
}

func recordMySQLLockEvent(ctx context.Context, tx pgx.Tx, assetID, sampleID string, metrics map[string]any, now time.Time) error {
	report, ok := metrics["adapter"].(collectoradapter.Report)
	if !ok {
		return nil
	}
	var lockSignal collectoradapter.Signal
	for _, signal := range report.Signals {
		if signal.Key == "lockWaits" {
			lockSignal = signal
			break
		}
	}
	details, ok := metrics["mysql"].(collector.MySQLMetrics)
	if !ok || lockSignal.Status != "ok" {
		return nil
	}
	return recordEngineLockEvent(ctx, tx, assetID, sampleID, now, engineLockEvidence{
		group: mysqlLockGroup, title: "MySQL InnoDB 锁等待", source: lockSignal.Source,
		waits: details.LockWaits, count: len(details.LockWaits), truncated: details.LocksTruncated,
	})
}

type engineLockEvidence struct {
	group, title, source string
	waits                any
	count                int
	truncated            bool
}

func recordEngineLockEvent(ctx context.Context, tx pgx.Tx, assetID, sampleID string, now time.Time, lock engineLockEvidence) error {
	return recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, lock.group, lock.title, lock.source, lock.count != 0,
		map[string]any{"waits": lock.waits, "truncated": lock.truncated}, map[string]any{"waitCount": 0})
}

func recordEngineObservationEvent(ctx context.Context, tx pgx.Tx, assetID, sampleID string, now time.Time,
	group, title, source string, active bool, occurrence, recovery map[string]any) error {
	var eventID string
	err := tx.QueryRow(ctx, `SELECT event_id::text FROM open_event_groups WHERE asset_id=$1 AND event_group=$2`,
		assetID, group).Scan(&eventID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !active {
		if eventID == "" {
			return nil
		}
		var forecastActive bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM risk_active WHERE event_id=$1)`, eventID).Scan(&forecastActive); err != nil {
			return err
		}
		if forecastActive {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET status='resolved',ended_at=$1 WHERE id=$2`, now, eventID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM open_event_groups WHERE asset_id=$1 AND event_group=$2`, assetID, group); err != nil {
			return err
		}
		recovery["sampleId"], recovery["source"] = sampleID, source
		payload, _ := json.Marshal(recovery)
		_, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'collection_recovered',$2,$3)`, eventID, now, payload)
		return err
	}
	if eventID != "" {
		occurrence["sampleId"], occurrence["source"] = sampleID, source
		payload, _ := json.Marshal(occurrence)
		_, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			SELECT gen_random_uuid(),$1,$2,$3,$4 WHERE EXISTS(SELECT 1 FROM risk_active WHERE event_id=$1)
			AND NOT EXISTS(SELECT 1 FROM evidence WHERE event_id=$1 AND kind=$2)`, eventID, group, now, payload)
		return err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at) VALUES(gen_random_uuid(),$1,$2)
		RETURNING id::text`, assetID, now).Scan(&eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,$3,$4,'open',$5,$3)`, eventID, assetID, group, title, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO open_event_groups(asset_id,event_group,event_id) VALUES($1,$2,$3)`, assetID, group, eventID); err != nil {
		return err
	}
	occurrence["sampleId"], occurrence["source"] = sampleID, source
	payload, _ := json.Marshal(occurrence)
	_, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,$2,$3,$4)`, eventID, group, now, payload)
	return err
}
