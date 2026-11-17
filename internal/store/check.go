package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrAssetNotFound = errors.New("asset not found")
var ErrCollectionUnavailable = errors.New("cross-layer Go collector is not yet available")

type CheckResult struct {
	SampleID    string    `json:"sampleId"`
	Status      string    `json:"status"`
	CollectedAt time.Time `json:"collectedAt"`
}

// DueAssetIDs returns only assets for which the Go collector has all enabled adapters.
// It intentionally leaves K8S, Linux exporter, and PgBouncer assets to their existing collector.
func (s *Postgres) DueAssetIDs(ctx context.Context, interval time.Duration, limit int) ([]string, error) {
	if interval < 15*time.Second || interval > time.Hour || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid Go collection schedule")
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM assets
		WHERE runtime='linux' AND linux_exporter_port IS NULL AND pooler_url_enc IS NULL
		AND (last_sample_at IS NULL OR last_sample_at <= now() - make_interval(secs=>$1))
		ORDER BY last_sample_at NULLS FIRST,id LIMIT $2`, int(interval.Seconds()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func collectionError(err error) (string, string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if strings.HasPrefix(pgErr.Code, "28") {
			return "telemetry", "PostgreSQL authentication failed (SQLSTATE " + pgErr.Code + ")"
		}
		if pgErr.Code == "42501" {
			return "telemetry", "PostgreSQL monitoring role lacks permission (SQLSTATE 42501)"
		}
		return "telemetry", "PostgreSQL collection query failed (SQLSTATE " + pgErr.Code + ")"
	}
	if strings.Contains(err.Error(), "outside the supported") {
		return "telemetry", err.Error()
	}
	if strings.Contains(strings.ToLower(err.Error()), "connection refused") {
		return "availability", "PostgreSQL connection refused"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "availability", "PostgreSQL connection timed out"
	}
	return "availability", "PostgreSQL connection or collection failed"
}

func (s *Postgres) CheckAsset(ctx context.Context, id string, key []byte) (CheckResult, error) {
	var target collector.Target
	var encrypted string
	var runtime string
	var hasExporter, hasPooler bool
	err := s.pool.QueryRow(ctx, `SELECT host,port,database_name,username,ssl_mode,password_enc,
		runtime,linux_exporter_port IS NOT NULL,pooler_url_enc IS NOT NULL
		FROM assets WHERE id=$1`, id).Scan(&target.Host, &target.Port, &target.Database,
		&target.Username, &target.SSLMode, &encrypted, &runtime, &hasExporter, &hasPooler)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckResult{}, ErrAssetNotFound
	}
	if err != nil {
		return CheckResult{}, fmt.Errorf("load monitoring target: %w", err)
	}
	if runtime != "linux" || hasExporter || hasPooler {
		return CheckResult{}, ErrCollectionUnavailable
	}
	password, err := security.Decrypt(key, encrypted)
	if err != nil {
		return CheckResult{}, fmt.Errorf("decrypt monitoring credential: %w", err)
	}
	metrics, collectErr := collector.PostgreSQL(ctx, target, password)
	group, reason := "", ""
	if collectErr != nil {
		group, reason = collectionError(collectErr)
	}
	result, err := s.recordCheck(ctx, id, metrics, group, reason)
	if err != nil {
		return CheckResult{}, err
	}
	return result, nil
}

func (s *Postgres) recordCheck(ctx context.Context, id string, metrics map[string]any, group, reason string) (CheckResult, error) {
	now := time.Now().UTC()
	if err := s.ensureCollectionPartitions(ctx, now); err != nil {
		return CheckResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	defer tx.Rollback(ctx)
	var locked string
	if err := tx.QueryRow(ctx, "SELECT id FROM assets WHERE id=$1 FOR UPDATE", id).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CheckResult{}, ErrAssetNotFound
		}
		return CheckResult{}, err
	}
	status := "ok"
	if reason != "" {
		status = "error"
	}
	encoded, err := json.Marshal(metrics)
	if err != nil {
		return CheckResult{}, err
	}
	var sampleID string
	err = tx.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,$5) RETURNING id`, id, now, status, encoded, nullable(reason)).Scan(&sampleID)
	if err != nil {
		return CheckResult{}, fmt.Errorf("save monitoring sample: %w", err)
	}
	var version, role any
	if metrics != nil {
		version = metrics["version_num"]
		role = metrics["role"]
	}
	_, err = tx.Exec(ctx, `UPDATE assets SET last_sample_at=$1,last_status=$2,last_error=$3,
		version_num=coalesce($4,version_num),role=coalesce($5,role) WHERE id=$6`,
		now, status, nullable(reason), version, role, id)
	if err != nil {
		return CheckResult{}, fmt.Errorf("update asset status: %w", err)
	}
	if reason != "" {
		var eventID string
		err = tx.QueryRow(ctx, `SELECT id FROM events WHERE asset_id=$1 AND event_group=$2
			AND status='open' ORDER BY started_at DESC LIMIT 1 FOR UPDATE`, id, group).Scan(&eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
				VALUES(gen_random_uuid(),$1,$2) RETURNING id`, id, now).Scan(&eventID)
			if err != nil {
				return CheckResult{}, err
			}
			title := "PostgreSQL 采集失败"
			if group == "availability" {
				title = "PostgreSQL 无法连接"
			}
			_, err = tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
				VALUES($1,$2,'collection_failure',$3,'open',$4,$5)`, eventID, id, title, now, group)
			if err != nil {
				return CheckResult{}, err
			}
			_, err = tx.Exec(ctx, `INSERT INTO open_event_groups(asset_id,event_group,event_id)
				VALUES($1,$2,$3) ON CONFLICT(asset_id,event_group)
				DO UPDATE SET event_id=excluded.event_id`, id, group, eventID)
			if err != nil {
				return CheckResult{}, err
			}
		} else if err != nil {
			return CheckResult{}, err
		}
		payload, _ := json.Marshal(map[string]any{"sampleId": sampleID, "error": reason, "source": "go_postgres_collector"})
		_, err = tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'collection_error',$2,$3)`, eventID, now, payload)
		if err != nil {
			return CheckResult{}, err
		}
	} else {
		rows, err := tx.Query(ctx, `SELECT id FROM events WHERE asset_id=$1 AND status='open'
			AND kind='collection_failure' FOR UPDATE`, id)
		if err != nil {
			return CheckResult{}, err
		}
		var eventIDs []string
		for rows.Next() {
			var eventID string
			if err := rows.Scan(&eventID); err != nil {
				rows.Close()
				return CheckResult{}, err
			}
			eventIDs = append(eventIDs, eventID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return CheckResult{}, err
		}
		rows.Close()
		for _, eventID := range eventIDs {
			payload, _ := json.Marshal(map[string]any{"sampleId": sampleID, "source": "go_postgres_collector"})
			if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
				VALUES(gen_random_uuid(),$1,'collection_recovered',$2,$3)`, eventID, now, payload); err != nil {
				return CheckResult{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE events SET status='resolved',ended_at=$1 WHERE id=$2`, now, eventID); err != nil {
				return CheckResult{}, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM open_event_groups WHERE event_id=$1`, eventID); err != nil {
				return CheckResult{}, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CheckResult{}, err
	}
	return CheckResult{SampleID: sampleID, Status: status, CollectedAt: now}, nil
}

func (s *Postgres) ensureCollectionPartitions(ctx context.Context, at time.Time) error {
	month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := month.AddDate(0, 1, 0)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1742026)"); err != nil {
		return err
	}
	for _, table := range []string{"samples", "events"} {
		name := fmt.Sprintf("%s_%04d%02d", table, month.Year(), int(month.Month()))
		query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
			name, table, month.Format(time.RFC3339), end.Format(time.RFC3339))
		if _, err := tx.Exec(ctx, query); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
