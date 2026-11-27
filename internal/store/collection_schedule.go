package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collectorstate"
	"github.com/jackc/pgx/v5"
)

const CollectionBudget = 45 * time.Second
const collectionLeaseSeconds = 60

var ErrCollectionLeaseLost = errors.New("collection lease expired or was replaced")

type CollectionLease struct {
	AssetID   string
	Token     string
	DueAt     time.Time
	ClaimedAt time.Time
	Isolated  bool

	startedAt time.Time
	queryMs   *int64
}

type CollectionLane int

const (
	CollectionAny CollectionLane = iota
	CollectionRegular
	CollectionIsolated
)

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
func (s *Postgres) ClaimCollectionJobs(ctx context.Context, interval time.Duration, limit int, lane CollectionLane) ([]CollectionLease, error) {
	if interval < collectorstate.MinimumIntervalSeconds*time.Second || interval > time.Hour || limit < 1 || limit > 100 ||
		lane < CollectionAny || lane > CollectionIsolated {
		return nil, errors.New("invalid collection schedule")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO collection_jobs(asset_id,due_at)
		SELECT a.id,coalesce(a.last_sample_at+make_interval(secs=>$1),a.created_at)
		FROM assets a WHERE a.runtime IN ('linux','k8s','managed','mysql','dm8','kingbase','opengauss','redis','etcd')
		AND a.remote_site=''
		AND NOT EXISTS (SELECT 1 FROM collection_jobs j WHERE j.asset_id=a.id)
		ORDER BY a.id LIMIT 100 FOR KEY SHARE OF a SKIP LOCKED
		ON CONFLICT(asset_id) DO NOTHING`, int(interval.Seconds())); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH eligible AS (
		SELECT j.asset_id,j.due_at,row_number() OVER (PARTITION BY CASE WHEN a.runtime IN ('linux','k8s','managed')
			THEN 'postgres' ELSE a.runtime END ORDER BY j.due_at,j.asset_id) AS source_slot
		FROM collection_jobs j JOIN assets a ON a.id=j.asset_id
		WHERE j.due_at<=now() AND (j.lease_until IS NULL OR j.lease_until<=now())
		AND a.runtime IN ('linux','k8s','managed','mysql','dm8','kingbase','opengauss','redis','etcd')
		AND a.remote_site=''
		AND ($4::int=0 OR j.isolated=($4=2))
	), ready AS (
		SELECT j.asset_id,j.lease_token,j.lease_replica_id,j.lease_until FROM eligible e JOIN collection_jobs j ON j.asset_id=e.asset_id
		WHERE j.due_at<=now() AND (j.lease_until IS NULL OR j.lease_until<=now())
		AND ($4::int=0 OR j.isolated=($4=2))
		ORDER BY e.source_slot,e.due_at,e.asset_id LIMIT $1 FOR UPDATE OF j SKIP LOCKED
	), claimed AS (
		UPDATE collection_jobs j SET lease_token=gen_random_uuid(),
		lease_until=statement_timestamp()+make_interval(secs=>$2),lease_replica_id=nullif($3,'')
		FROM ready WHERE j.asset_id=ready.asset_id
		RETURNING j.asset_id,j.lease_token,j.due_at,j.lease_until,j.isolated,j.lease_replica_id,
			ready.lease_token AS previous_token,ready.lease_replica_id AS previous_replica,
			ready.lease_until AS previous_expiry
	), expired AS (
		UPDATE collection_attempts a SET outcome='lease_expired',finished_at=c.previous_expiry
		FROM claimed c WHERE a.id=c.previous_token AND a.outcome='running'
	), logged AS (
		INSERT INTO collection_attempts(id,asset_id,due_at,claimed_at,lease_until,replica_id,previous_replica_id,takeover,isolated)
		SELECT lease_token,asset_id,due_at,statement_timestamp(),lease_until,lease_replica_id,previous_replica,
			previous_token IS NOT NULL,isolated FROM claimed
		RETURNING asset_id,id,due_at,claimed_at,isolated
	) SELECT asset_id::text,id::text,due_at,claimed_at,isolated FROM logged`,
		limit, collectionLeaseSeconds, s.platformReplicaID, lane)
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

const finishCollectionSQL = `WITH advanced AS (UPDATE collection_jobs SET
	lease_token=NULL,lease_until=NULL,lease_replica_id=NULL,
	failures=CASE WHEN $3='ok' THEN 0 ELSE least(failures+1,7) END,
	last_outcome=$3,last_finished_at=statement_timestamp(),last_sample_id=coalesce($4,last_sample_id),
	last_success_at=CASE WHEN $3='ok' THEN $10 ELSE last_success_at END,
	last_queue_ms=$6,last_query_ms=$7,last_store_ms=$8,last_replica_id=nullif($9,''),
	last_permission_gaps=$12,last_engine=coalesce(nullif($13,''),last_engine),
	isolated=CASE WHEN $3='ok' THEN coalesce($11::bigint>=5000 AND (isolated OR last_query_ms>=5000),false)
		ELSE isolated OR failures>=1 OR coalesce($11::bigint>=5000,false) END,
	due_at=CASE WHEN $3='ok'
		THEN date_bin(make_interval(secs=>$5::int),statement_timestamp(),due_at)+make_interval(secs=>$5::int)
		ELSE statement_timestamp()+make_interval(secs=>least(greatest($5::int,300),$5::int*(1 << least(failures,5)))) END
	WHERE asset_id=$1 AND lease_token=$2 AND ($14::text='' OR lease_until>clock_timestamp())
	RETURNING due_at,last_finished_at,last_outcome)
	UPDATE collection_attempts a SET outcome=coalesce(nullif($14,''),advanced.last_outcome),
		finished_at=advanced.last_finished_at,query_ms=$7,store_ms=$8,permission_gaps=$12,sample_id=$4,
		backoff_until=CASE WHEN advanced.last_outcome<>'ok' THEN advanced.due_at END
	FROM advanced WHERE a.id=$2`

// Retry only a lease still owned by this worker. If commit already succeeded,
// the cleared token makes a retry harmless.
func (s *Postgres) RetryCollectionLease(ctx context.Context, lease CollectionLease) error {
	return s.RetryFailedCollectionLease(ctx, lease, nil)
}

func (s *Postgres) RetryFailedCollectionLease(ctx context.Context, lease CollectionLease, failure error) error {
	outcome := "execution_error"
	if errors.Is(failure, errCollectionStoreWrite) {
		outcome = "store_error"
	}
	_, err := s.pool.Exec(ctx, finishCollectionSQL,
		lease.AssetID, lease.Token, "execution_error", nil, 5, collectionQueueDelay(lease), nil, nil,
		s.platformReplicaID, nil, nil, nil, "", outcome)
	return err
}

func (lease *CollectionLease) recordQueryDuration(start time.Time) {
	if lease != nil {
		duration := time.Since(start).Milliseconds()
		lease.queryMs = &duration
	}
}

func (s *Postgres) CheckCollectionLease(ctx context.Context, lease CollectionLease, interval time.Duration, key []byte) (CheckResult, error) {
	if now := time.Now().UTC(); now.Sub(lease.DueAt) >= interval {
		if err := s.checkStaleAsset(ctx, lease.AssetID, interval, now, &lease); err != nil {
			return CheckResult{}, err
		}
	}
	return s.checkAsset(ctx, lease.AssetID, key, &lease, interval)
}

func collectionQueueDelay(lease CollectionLease) *int64 {
	if lease.ClaimedAt.IsZero() || lease.DueAt.IsZero() {
		return nil
	}
	delay := max(int64(0), lease.ClaimedAt.Sub(lease.DueAt).Milliseconds())
	return &delay
}

const collectionQueueSQL = `WITH queue AS (
	SELECT a.id AS "assetId",a.name,a.last_sample_at AS "lastSampleAt",j.last_sample_id AS "lastSampleId",
		a.runtime,coalesce(j.last_engine,CASE WHEN a.runtime IN ('linux','k8s','managed') THEN 'postgres' ELSE a.runtime END) AS engine,
		j.last_permission_gaps AS "permissionGaps",
		j.lease_until AS "leaseUntil",j.lease_replica_id AS "leaseReplicaId",j.last_finished_at AS "lastFinishedAt",j.due_at AS "dueAt",
		coalesce(j.isolated,false) AS isolated,j.last_queue_ms AS "queueMs",j.last_query_ms AS "queryMs",j.last_store_ms AS "storeMs",
		coalesce(j.failures,0) AS failures,coalesce(j.last_outcome,'') AS outcome,
		CASE WHEN j.lease_until>now() THEN 'running'
		WHEN j.due_at<=now() AND (j.lease_until IS NOT NULL OR j.due_at<=now()-make_interval(secs=>$1)) THEN 'delayed'
		WHEN coalesce(j.failures,0)>0 AND j.due_at>now() THEN 'backoff'
		WHEN a.last_sample_at IS NULL THEN 'missing'
		WHEN coalesce(j.due_at,a.last_sample_at+make_interval(secs=>$1))<=now() THEN 'due'
		ELSE 'scheduled' END AS state
	FROM assets a LEFT JOIN collection_jobs j ON j.asset_id=a.id WHERE a.runtime IN ('linux','k8s','managed','mysql','dm8','kingbase','opengauss','redis','etcd')
		AND ($2::uuid IS NULL OR a.organization_id=$2)
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
	err := s.pool.QueryRow(ctx, collectionQueueSQL, interval, nullable(OrganizationFromContext(ctx))).Scan(&result)
	return result, err
}
