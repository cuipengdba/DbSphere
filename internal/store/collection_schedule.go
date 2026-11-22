package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const CollectionBudget = 45 * time.Second
const collectionLeaseSeconds = 60

var ErrCollectionLeaseLost = errors.New("collection lease expired or was replaced")

type CollectionLease struct {
	AssetID string
	Token   string
	DueAt   time.Time
}

func migrateCollectionSchedule(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=36)").Scan(&applied); err != nil {
		return fmt.Errorf("check collection schedule migration: %w", err)
	}
	if applied {
		return nil
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS collection_jobs (
		asset_id uuid PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
		due_at timestamptz NOT NULL,
		lease_token uuid,
		lease_until timestamptz,
		failures integer NOT NULL DEFAULT 0 CHECK (failures BETWEEN 0 AND 7),
		last_outcome text NOT NULL DEFAULT '' CHECK (last_outcome IN ('','ok','target_error','execution_error')),
		last_finished_at timestamptz,
		last_sample_id uuid,
		CHECK ((lease_token IS NULL) = (lease_until IS NULL))
	);
	CREATE INDEX IF NOT EXISTS collection_jobs_due ON collection_jobs(due_at,asset_id);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(36)`); err != nil {
		return fmt.Errorf("migrate collection schedule: %w", err)
	}
	return nil
}

// Seed only missing jobs. A restart preserves pending work and the retry clock.
func (s *Postgres) ClaimCollectionJobs(ctx context.Context, interval time.Duration, limit int) ([]CollectionLease, error) {
	if interval < 15*time.Second || interval > time.Hour || limit < 1 || limit > 100 {
		return nil, errors.New("invalid collection schedule")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO collection_jobs(asset_id,due_at)
		SELECT a.id,coalesce(a.last_sample_at+make_interval(secs=>$1),a.created_at)
		FROM assets a WHERE a.runtime IN ('linux','k8s','managed')
		AND NOT EXISTS (SELECT 1 FROM collection_jobs j WHERE j.asset_id=a.id)
		ORDER BY a.id LIMIT 100 FOR KEY SHARE OF a SKIP LOCKED
		ON CONFLICT(asset_id) DO NOTHING`, int(interval.Seconds())); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH ready AS (
		SELECT j.asset_id FROM collection_jobs j JOIN assets a ON a.id=j.asset_id
		WHERE j.due_at<=now() AND (j.lease_until IS NULL OR j.lease_until<=now())
		AND a.runtime IN ('linux','k8s','managed')
		ORDER BY j.due_at,j.asset_id LIMIT $1 FOR UPDATE OF j SKIP LOCKED
	) UPDATE collection_jobs j SET lease_token=gen_random_uuid(),
		lease_until=clock_timestamp()+make_interval(secs=>$2)
	FROM ready WHERE j.asset_id=ready.asset_id RETURNING j.asset_id::text,j.lease_token::text,j.due_at`,
		limit, collectionLeaseSeconds)
	if err != nil {
		return nil, err
	}
	leases, err := pgx.CollectRows(rows, pgx.RowToStructByPos[CollectionLease])
	if err != nil {
		return nil, err
	}
	return leases, tx.Commit(ctx)
}

func lockCollectionLease(ctx context.Context, tx pgx.Tx, lease CollectionLease) error {
	var token string
	err := tx.QueryRow(ctx, `SELECT lease_token::text FROM collection_jobs
		WHERE asset_id=$1 AND lease_token=$2 AND lease_until>clock_timestamp() FOR UPDATE`,
		lease.AssetID, lease.Token).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCollectionLeaseLost
	}
	return err
}

const finishCollectionSQL = `UPDATE collection_jobs SET
	lease_token=NULL,lease_until=NULL,
	failures=CASE WHEN $3='ok' THEN 0 ELSE least(failures+1,7) END,
	last_outcome=$3,last_finished_at=now(),last_sample_id=coalesce($4,last_sample_id),
	due_at=now()+make_interval(secs=>CASE WHEN $3='ok' THEN $5
		ELSE least(greatest($5,300),$5*(1 << least(failures,5))) END)
	WHERE asset_id=$1 AND lease_token=$2`

// Retry only a lease still owned by this worker. If commit already succeeded,
// the cleared token makes a retry harmless.
func (s *Postgres) RetryCollectionLease(ctx context.Context, lease CollectionLease) error {
	_, err := s.pool.Exec(ctx, finishCollectionSQL+" AND lease_until>clock_timestamp()",
		lease.AssetID, lease.Token, "execution_error", nil, 5)
	return err
}

func (s *Postgres) CheckCollectionLease(ctx context.Context, lease CollectionLease, interval time.Duration, key []byte) (CheckResult, error) {
	if now := time.Now().UTC(); now.Sub(lease.DueAt) >= interval {
		if err := s.checkStaleAsset(ctx, lease.AssetID, interval, now, &lease); err != nil {
			return CheckResult{}, err
		}
	}
	return s.checkAsset(ctx, lease.AssetID, key, &lease, interval)
}

const collectionQueueSQL = `WITH queue AS (
	SELECT a.id AS "assetId",a.name,a.last_sample_at AS "lastSampleAt",j.last_sample_id AS "lastSampleId",
		j.lease_until AS "leaseUntil",j.last_finished_at AS "lastFinishedAt",j.due_at AS "dueAt",
		coalesce(j.failures,0) AS failures,coalesce(j.last_outcome,'') AS outcome,
		CASE WHEN j.lease_until>now() THEN 'running'
		WHEN j.due_at<=now() AND (j.lease_until IS NOT NULL OR j.due_at<=now()-make_interval(secs=>$1)) THEN 'delayed'
		WHEN coalesce(j.failures,0)>0 AND j.due_at>now() THEN 'backoff'
		WHEN a.last_sample_at IS NULL THEN 'missing'
		WHEN coalesce(j.due_at,a.last_sample_at+make_interval(secs=>$1))<=now() THEN 'due'
		ELSE 'scheduled' END AS state
	FROM assets a LEFT JOIN collection_jobs j ON j.asset_id=a.id WHERE a.runtime IN ('linux','k8s','managed')
), ranked AS (
	SELECT *, array_position(ARRAY['delayed','backoff','missing','running','due','scheduled'],state) AS priority
	FROM queue ORDER BY priority,"dueAt" NULLS FIRST,"assetId" LIMIT 30
)
SELECT jsonb_build_object('counts',(SELECT jsonb_build_object(
	'total',count(*),'delayed',count(*) FILTER(WHERE state='delayed'),'backoff',count(*) FILTER(WHERE state='backoff'),
	'missing',count(*) FILTER(WHERE state='missing'),'due',count(*) FILTER(WHERE state='due'),
	'running',count(*) FILTER(WHERE state='running'),'scheduled',count(*) FILTER(WHERE state='scheduled')) FROM queue),
	'jobs',coalesce((SELECT jsonb_agg(to_jsonb(ranked)-'priority' ORDER BY priority,"dueAt" NULLS FIRST,"assetId") FROM ranked),'[]'::jsonb))`

func (s *Postgres) CollectionQueue(ctx context.Context, interval int) (any, error) {
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, collectionQueueSQL, interval).Scan(&result)
	return result, err
}
