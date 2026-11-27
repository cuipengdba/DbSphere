package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/remoteobs"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
)

var ErrRemoteScope = errors.New("remote collector is not assigned to the asset")
var ErrRemoteConflict = errors.New("observation ID already used for different data")

func migrateRemoteCollection(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=71)`).Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE assets ADD COLUMN remote_site text NOT NULL DEFAULT '',
		ADD COLUMN remote_assigned_at timestamptz, ADD COLUMN remote_received_at timestamptz,
		ADD COLUMN remote_backlog integer, ADD COLUMN remote_interval_seconds integer;
	CREATE INDEX assets_remote_site ON assets (remote_site,id) WHERE remote_site<>'';
	CREATE TABLE remote_collection_receipts (
		site text NOT NULL, id uuid NOT NULL, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		observed_at timestamptz NOT NULL, digest bytea NOT NULL, sample_id uuid,
		PRIMARY KEY(site,id)
	);
	CREATE INDEX remote_collection_receipts_age ON remote_collection_receipts(observed_at);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(71)`)
	return err
}

// AssignRemoteCollector is a deployment administrator operation. The target
// credentials used by a site collector are configured on that collector.
func (s *Postgres) AssignRemoteCollector(ctx context.Context, id, site string) error {
	if !remoteobs.ValidID(id) || (site != "" && !remoteobs.ValidSite(site)) {
		return remoteobs.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current string
	if err := tx.QueryRow(ctx, `SELECT remote_site FROM assets WHERE id=$1 AND
		($2='' OR runtime IN ('linux','managed','mysql','redis')) FOR UPDATE`, id, site).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRemoteScope
		}
		return err
	}
	if current != site {
		if _, err := tx.Exec(ctx, `UPDATE assets SET remote_site=$2,remote_assigned_at=clock_timestamp(),
			remote_received_at=NULL,remote_backlog=NULL,remote_interval_seconds=NULL WHERE id=$1`, id, site); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM collection_jobs WHERE asset_id=$1`, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type remoteWrite struct {
	site       string
	item       remoteobs.Observation
	digest     [32]byte
	receivedAt time.Time
	backlog    int
	interval   time.Duration
	duplicate  bool
}

func (r *remoteWrite) prepare(ctx context.Context, tx pgx.Tx, runtime, assignedSite string, metrics map[string]any) (bool, error) {
	engine := runtime
	if runtime == "linux" || runtime == "managed" {
		engine = "postgresql"
	}
	if assignedSite != r.site || engine != r.item.Engine {
		return false, ErrRemoteScope
	}
	// Delayed samples predating the assignment cannot populate a reassigned asset.
	var assigned time.Time
	if err := tx.QueryRow(ctx, `SELECT remote_assigned_at FROM assets WHERE id=$1`, r.item.AssetID).Scan(&assigned); err != nil {
		return false, err
	}
	if r.item.ObservedAt.Before(assigned.Add(-15 * time.Second)) {
		return false, ErrRemoteScope
	}
	tag, err := tx.Exec(ctx, `INSERT INTO remote_collection_receipts(site,id,asset_id,observed_at,digest)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, r.site, r.item.ID, r.item.AssetID, r.item.ObservedAt, r.digest[:])
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		var same bool
		if err := tx.QueryRow(ctx, `SELECT digest=$3 FROM remote_collection_receipts WHERE site=$1 AND id=$2`, r.site, r.item.ID, r.digest[:]).Scan(&same); err != nil {
			return false, err
		}
		if !same {
			return false, ErrRemoteConflict
		}
		r.duplicate = true
		return true, nil
	}
	metrics["remoteCollector"] = map[string]any{"site": r.site, "observationId": r.item.ID,
		"receivedAt": r.receivedAt, "backlog": r.backlog}
	return false, nil
}

func (r *remoteWrite) finish(ctx context.Context, tx pgx.Tx, sampleID string) error {
	if _, err := tx.Exec(ctx, `UPDATE remote_collection_receipts SET sample_id=$3 WHERE site=$1 AND id=$2`, r.site, r.item.ID, sampleID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE assets SET remote_received_at=greatest(remote_received_at,$2),remote_interval_seconds=$4,
		remote_backlog=CASE WHEN remote_received_at IS NULL OR remote_received_at <= $2 THEN $3 ELSE remote_backlog END WHERE id=$1`,
		r.item.AssetID, r.receivedAt, r.backlog, int(r.interval.Seconds()))
	return err
}

func remoteMetrics(item remoteobs.Observation) (map[string]any, error) {
	metrics := map[string]any{}
	if item.Failure != "" {
		key := item.Engine + "Status"
		if item.Engine == "postgresql" {
			key = "pgStatus"
		}
		status := "availability"
		if item.Failure == "credential_unavailable" {
			status = "telemetry"
		}
		metrics["collector"] = map[string]any{key: status}
		return metrics, nil
	}
	// Decode the engine's structured fields back into the types expected by
	// the native interval and alert evaluators.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(item.Metrics, &raw); err != nil {
		return nil, remoteobs.ErrInvalid
	}
	var adapter collectoradapter.Report
	if json.Unmarshal(raw["adapter"], &adapter) != nil || adapter.Engine != item.Engine ||
		adapter.Contract != collectoradapter.ContractVersion || adapter.Version <= 0 {
		return nil, remoteobs.ErrInvalid
	}
	metrics["adapter"], metrics["version_num"] = adapter, adapter.Version
	if adapter.Role != "" {
		metrics["role"] = adapter.Role
	}
	statusKey := item.Engine + "Status"
	allowed := []string{"connections", "max_connections"}
	switch item.Engine {
	case "postgresql":
		statusKey = "pgStatus"
		allowed = append(allowed, "pg14_eol", "databaseName", "postgresEndpoint", "postgresStartedAt", "monitor_role", "stats_visible",
			"privileged_role", "xact_commit", "xact_rollback", "database_bytes", "replicas", "wal_receiver", "replication_lag_bytes",
			"replication_observed", "diagnostics", "pgStorageIO", "sqlPerformance", "pgCompatibility", "highgoWaits")
	case "mysql":
		statusKey = "mysqlStatus"
		var value collector.MySQLMetrics
		if json.Unmarshal(raw["mysql"], &value) != nil {
			return nil, remoteobs.ErrInvalid
		}
		metrics["mysql"] = value
	case "redis":
		statusKey = "redisStatus"
		var value collector.RedisMetrics
		if json.Unmarshal(raw["redis"], &value) != nil {
			return nil, remoteobs.ErrInvalid
		}
		metrics["redis"] = value
	}
	for _, key := range allowed {
		if value, ok := raw[key]; ok {
			var decoded any
			if json.Unmarshal(value, &decoded) != nil {
				return nil, remoteobs.ErrInvalid
			}
			metrics[key] = decoded
		}
	}
	metrics["collector"] = map[string]any{statusKey: "ok"}
	return metrics, nil
}

func (s *Postgres) StoreRemoteObservation(ctx context.Context, site string, item remoteobs.Observation, interval time.Duration, backlog int) (remoteobs.Receipt, error) {
	now := time.Now().UTC()
	if !remoteobs.ValidSite(site) || interval < remoteobs.MinInterval || interval > remoteobs.MaxInterval || item.Validate(now) != nil || backlog < 0 || backlog > 10000 {
		return remoteobs.Receipt{}, remoteobs.ErrInvalid
	}
	metrics, err := remoteMetrics(item)
	if err != nil {
		return remoteobs.Receipt{}, err
	}
	group, reason := "", ""
	switch item.Failure {
	case "target_unavailable":
		group, reason = "availability", "站点数据库不可达或采集失败"
	case "credential_unavailable":
		group, reason = "telemetry", "站点采集凭据不可用"
	}
	wire, err := json.Marshal(item)
	if err != nil {
		return remoteobs.Receipt{}, err
	}
	remote := &remoteWrite{site: site, item: item, digest: sha256.Sum256(wire), receivedAt: now, backlog: backlog, interval: interval}
	_, err = s.recordObservation(ctx, item.AssetID, metrics, group, reason, item.ObservedAt.UTC(), nil, interval, remote)
	if errors.Is(err, ErrAssetNotFound) {
		err = ErrRemoteScope
	}
	if err != nil {
		return remoteobs.Receipt{}, err
	}
	status := "stored"
	if remote.duplicate {
		status = "duplicate"
	}
	return remoteobs.Receipt{ID: item.ID, Status: status}, nil
}

func (s *Postgres) RefreshRemoteCollection(ctx context.Context, interval time.Duration, now time.Time) error {
	if interval < remoteobs.MinInterval || interval > remoteobs.MaxInterval {
		return remoteobs.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM assets WHERE remote_site<>''
		AND greatest(remote_assigned_at,last_sample_at) <= $1`, now.Add(-2*interval))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.checkStaleCollection(ctx, id, interval, now, nil, true); err != nil && !errors.Is(err, ErrAssetNotFound) {
			return err
		}
	}
	_, err = s.pool.Exec(ctx, `DELETE FROM remote_collection_receipts WHERE (site,id) IN
		(SELECT site,id FROM remote_collection_receipts WHERE observed_at<$1 ORDER BY observed_at LIMIT 500)`,
		now.Add(-remoteobs.MaxReplayAge-time.Hour))
	return err
}
