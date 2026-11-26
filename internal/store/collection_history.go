package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

const collectionHistoryDays = 7

var errCollectionStoreWrite = errors.New("collection storage write failed")

func migrateCollectionHistory(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=64)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE collection_attempts (
		id uuid PRIMARY KEY,
		asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		due_at timestamptz NOT NULL,
		claimed_at timestamptz NOT NULL,
		lease_until timestamptz NOT NULL,
		replica_id text,
		previous_replica_id text,
		takeover boolean NOT NULL,
		isolated boolean NOT NULL,
		finished_at timestamptz,
		outcome text NOT NULL DEFAULT 'running' CHECK (outcome IN
			('running','ok','target_error','execution_error','store_error','lease_expired')),
		query_ms bigint,
		store_ms bigint,
		permission_gaps integer,
		sample_id uuid,
		backoff_until timestamptz
	);
	CREATE INDEX collection_attempts_asset_due ON collection_attempts(asset_id,due_at DESC,claimed_at DESC,id DESC);
	CREATE INDEX collection_attempts_retention ON collection_attempts(claimed_at,id);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(64)`)
	return err
}

func (s *Postgres) collectionHistory(ctx context.Context, assetID string, params url.Values) (any, error) {
	const limit = 200
	from, to, err := timeRange(params, time.Hour, collectionHistoryDays*24*time.Hour)
	if err != nil {
		return nil, err
	}
	rows, err := s.queryJSON(ctx, `WITH recent AS (
		SELECT * FROM collection_attempts WHERE asset_id=$1 AND due_at>=$2 AND due_at<$3
		ORDER BY due_at DESC,claimed_at DESC,id DESC LIMIT $4::int+1
	)
	SELECT jsonb_build_object('dueAt',due_at,'claimedAt',claimed_at,'leaseUntil',lease_until,
		'finishedAt',finished_at,'replicaId',replica_id,'previousReplicaId',previous_replica_id,
		'takeover',takeover,'isolated',isolated,'outcome',CASE WHEN outcome='running' AND lease_until<=now()
			THEN 'lease_expired' ELSE outcome END,
		'queueMs',greatest(0,floor(extract(epoch FROM claimed_at-due_at)*1000)),'queryMs',query_ms,
		'storeMs',store_ms,'permissionGaps',permission_gaps,'sampleId',sample_id,'backoffUntil',backoff_until)
	FROM recent ORDER BY due_at DESC,claimed_at DESC,id DESC`, assetID, from, to, limit)
	if err != nil {
		return nil, err
	}
	var previous, following json.RawMessage
	err = s.pool.QueryRow(ctx, `SELECT
		(SELECT jsonb_build_object('id',id,'collectedAt',collected_at)
		 FROM samples WHERE asset_id=$1 AND collected_at<$2 AND status='ok'
		 ORDER BY collected_at DESC,id DESC LIMIT 1),
		(SELECT jsonb_build_object('id',id,'collectedAt',collected_at)
		 FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND status='ok'
		 ORDER BY collected_at,id LIMIT 1)`, assetID, to).Scan(&previous, &following)
	if err != nil {
		return nil, err
	}
	return map[string]any{"attempts": rows[:min(len(rows), limit)], "truncated": len(rows) > limit, "from": from, "to": to,
		"latestSuccessfulSample": previous, "nextSuccessfulSample": following, "retentionDays": collectionHistoryDays}, nil
}

func (s *Postgres) PruneCollectionHistory(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit < 1 || limit > 10000 {
		return 0, errors.New("invalid collection history retention batch")
	}
	result, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT id FROM collection_attempts WHERE claimed_at<$1 ORDER BY claimed_at,id LIMIT $2
		FOR UPDATE SKIP LOCKED
	) DELETE FROM collection_attempts a USING expired e WHERE a.id=e.id`, now.AddDate(0, 0, -collectionHistoryDays), limit)
	return result.RowsAffected(), err
}
