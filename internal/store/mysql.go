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

func (s *Postgres) checkMySQL(ctx context.Context, id string, target collector.Target, password string, lease *CollectionLease, interval time.Duration) (CheckResult, error) {
	snapshot, err := (collector.MySQLAdapter{}).Collect(ctx, target, password)
	group, reason := "", ""
	metrics := snapshot.Metrics
	if err != nil {
		group, reason = mysqlCollectionError(err)
		metrics = map[string]any{"collector": map[string]any{"mysqlStatus": group}}
	} else {
		metrics["adapter"] = snapshot.Report
		metrics["collector"] = map[string]any{"mysqlStatus": "ok"}
	}
	return s.recordCheckWithLease(ctx, id, metrics, group, reason, time.Now().UTC(), lease, interval)
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
	var eventID string
	err := tx.QueryRow(ctx, `SELECT event_id::text FROM open_event_groups WHERE asset_id=$1 AND event_group=$2`,
		assetID, mysqlLockGroup).Scan(&eventID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if len(details.LockWaits) == 0 {
		if eventID == "" {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET status='resolved',ended_at=$1 WHERE id=$2`, now, eventID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM open_event_groups WHERE asset_id=$1 AND event_group=$2`, assetID, mysqlLockGroup); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"sampleId": sampleID, "source": lockSignal.Source, "waitCount": 0})
		_, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'collection_recovered',$2,$3)`, eventID, now, payload)
		return err
	}
	if eventID != "" {
		return nil
	}
	if err := tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at) VALUES(gen_random_uuid(),$1,$2)
		RETURNING id::text`, assetID, now).Scan(&eventID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,$3,'MySQL InnoDB 锁等待','open',$4,$3)`, eventID, assetID, mysqlLockGroup, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO open_event_groups(asset_id,event_group,event_id) VALUES($1,$2,$3)`, assetID, mysqlLockGroup, eventID); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"sampleId": sampleID, "source": lockSignal.Source,
		"waits": details.LockWaits, "truncated": details.LocksTruncated})
	_, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,$2,$3,$4)`, eventID, mysqlLockGroup, now, payload)
	return err
}
