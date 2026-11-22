package store

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrHistoryTierInput = errors.New("invalid history retention or trend input")

type HistoryTierPolicy struct {
	OrganizationID string    `json:"organizationId"`
	Metric         string    `json:"metric"`
	RawDays        int       `json:"rawDays"`
	HourlyDays     int       `json:"hourlyDays"`
	ArchiveThrough time.Time `json:"archiveThrough"`
}

type HistoryTierPass struct {
	Batched      int64
	RemovedHours int64
	Advanced     bool
}

func historyTierMetric(metric string) bool {
	switch metric {
	case "connections", "replicationLagBytes", "databaseBytes", "linuxCpuPercent":
		return true
	default:
		return false
	}
}

func migrateHistoryTiers(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=49)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `
		CREATE FUNCTION dbsphere_tier_path(metric text) RETURNS text[] LANGUAGE sql IMMUTABLE STRICT AS $$
		  SELECT CASE metric
		    WHEN 'connections' THEN ARRAY['connections']
		    WHEN 'replicationLagBytes' THEN ARRAY['replication_lag_bytes']
		    WHEN 'databaseBytes' THEN ARRAY['database_bytes']
		    WHEN 'linuxCpuPercent' THEN ARRAY['linux','cpu','usedPercent']
		  END
		$$;
		CREATE TABLE history_tier_policy (
			organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
			metric text NOT NULL CHECK (dbsphere_tier_path(metric) IS NOT NULL),
			raw_days integer NOT NULL CHECK(raw_days BETWEEN 1 AND 90),
			hourly_days integer NOT NULL CHECK(hourly_days BETWEEN raw_days AND 365),
			archive_through timestamptz NOT NULL,
			worked_at timestamptz NOT NULL DEFAULT '-infinity',
			PRIMARY KEY(organization_id,metric)
		);
		CREATE TABLE history_tier_hours (
			asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			metric text NOT NULL CHECK (dbsphere_tier_path(metric) IS NOT NULL),
			bucket timestamptz NOT NULL, samples integer NOT NULL CHECK(samples>0),
			total numeric NOT NULL, low numeric NOT NULL, peak numeric NOT NULL,
			PRIMARY KEY(asset_id,metric,bucket)
		);
		CREATE INDEX history_tier_hours_expiry ON history_tier_hours(metric,bucket,asset_id);
		INSERT INTO dbsphere_schema_migrations(version) VALUES(49)`)
	return err
}

func (s *Postgres) HistoryTierPolicies(ctx context.Context) ([]HistoryTierPolicy, error) {
	rows, err := s.pool.Query(ctx, `SELECT organization_id::text,metric,raw_days,hourly_days,archive_through
		FROM history_tier_policy WHERE ($1::uuid IS NULL OR organization_id=$1) ORDER BY organization_id,metric`,
		nullable(OrganizationFromContext(ctx)))
	if err != nil {
		return nil, err
	}
	value, err := pgx.CollectRows(rows, pgx.RowToStructByPos[HistoryTierPolicy])
	return append([]HistoryTierPolicy{}, value...), err
}

func (s *Postgres) SetHistoryTierPolicy(ctx context.Context, policy HistoryTierPolicy) (HistoryTierPolicy, error) {
	org := OrganizationFromContext(ctx)
	if org != "" {
		if policy.OrganizationID != "" && policy.OrganizationID != org {
			return HistoryTierPolicy{}, ErrHistoryTierInput
		}
		policy.OrganizationID = org
	}
	if !evidenceUUID.MatchString(policy.OrganizationID) || !historyTierMetric(policy.Metric) ||
		policy.RawDays < 1 || policy.RawDays > 90 || policy.HourlyDays < policy.RawDays || policy.HourlyDays > 365 {
		return HistoryTierPolicy{}, ErrHistoryTierInput
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO history_tier_policy(organization_id,metric,raw_days,hourly_days,archive_through)
		SELECT id,$2,$3,$4,date_trunc('hour',coalesce((
			SELECT min(oldest.sample_at) FROM assets a CROSS JOIN LATERAL (
				SELECT collected_at as sample_at FROM samples WHERE asset_id=a.id ORDER BY collected_at LIMIT 1
			) oldest WHERE a.organization_id=$1
			),now()),'UTC') FROM organizations WHERE id=$1
		ON CONFLICT(organization_id,metric) DO UPDATE SET raw_days=excluded.raw_days,hourly_days=excluded.hourly_days
		RETURNING archive_through`, policy.OrganizationID, policy.Metric, policy.RawDays, policy.HourlyDays).Scan(&policy.ArchiveThrough)
	if errors.Is(err, pgx.ErrNoRows) {
		return HistoryTierPolicy{}, ErrHistoryTierInput
	}
	return policy, err
}

const historyTierArchiveSQL = `WITH batch AS MATERIALIZED (
	SELECT s.id,s.asset_id,s.collected_at,(s.metrics_json#>dbsphere_tier_path($2))::numeric AS value
	FROM assets a JOIN samples s ON s.asset_id=a.id
	WHERE a.organization_id=$1 AND s.collected_at >= $3 AND s.collected_at < $4
	  AND jsonb_typeof(s.metrics_json#>dbsphere_tier_path($2))='number'
	  AND NOT EXISTS (SELECT 1 FROM evidence_sample_refs refs WHERE refs.sample_id=s.id::text)
	  AND NOT EXISTS (SELECT 1 FROM inspection_runs run WHERE run.sample_id=s.id)
	ORDER BY s.collected_at,s.id LIMIT $5 FOR UPDATE OF s
), compacted AS (
	UPDATE samples s SET metrics_json=s.metrics_json#-dbsphere_tier_path($2) FROM batch b
	WHERE s.id=b.id AND s.collected_at=b.collected_at
	RETURNING b.asset_id,b.collected_at,b.value
), rolled AS (
	INSERT INTO history_tier_hours(asset_id,metric,bucket,samples,total,low,peak)
	SELECT asset_id,$2,date_trunc('hour',collected_at,'UTC'),count(*)::int,sum(value),min(value),max(value)
	FROM compacted WHERE collected_at >= $6 GROUP BY asset_id,date_trunc('hour',collected_at,'UTC')
	ON CONFLICT(asset_id,metric,bucket) DO UPDATE SET samples=history_tier_hours.samples+excluded.samples,
		total=history_tier_hours.total+excluded.total,low=least(history_tier_hours.low,excluded.low),
		peak=greatest(history_tier_hours.peak,excluded.peak)
) SELECT count(*) FROM compacted`

// AdvanceHistoryTier spends at most one transaction on the selected policy. Each
// increment of a stored bucket commits with the removal of the corresponding raw field.
func (s *Postgres) AdvanceHistoryTier(ctx context.Context, now time.Time, limit int) (HistoryTierPass, error) {
	var result HistoryTierPass
	if now.IsZero() || limit < 1 || limit > 10000 {
		return result, ErrHistoryTierInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var policy HistoryTierPolicy
	hour := now.UTC().Truncate(time.Hour)
	err = tx.QueryRow(ctx, `SELECT organization_id::text,metric,raw_days,hourly_days,archive_through
		FROM history_tier_policy ORDER BY (archive_through<$1::timestamptz-make_interval(days=>raw_days)) DESC,
		worked_at,organization_id,metric LIMIT 1 FOR UPDATE SKIP LOCKED`, hour).
		Scan(&policy.OrganizationID, &policy.Metric, &policy.RawDays, &policy.HourlyDays, &policy.ArchiveThrough)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	rawCutoff := hour.AddDate(0, 0, -policy.RawDays)
	hourlyCutoff := hour.AddDate(0, 0, -policy.HourlyDays)
	through := policy.ArchiveThrough
	if through.Before(rawCutoff) {
		// PostgreSQL scans a bounded time slice even when there are years of retained history.
		end := earlierTime(through.Add(6*time.Hour), rawCutoff)
		if err := tx.QueryRow(ctx, historyTierArchiveSQL, policy.OrganizationID, policy.Metric, through, end, limit, hourlyCutoff).Scan(&result.Batched); err != nil {
			return result, err
		}
		if result.Batched < int64(limit) {
			through = end
		}
		result.Advanced = true
	}
	expired, err := tx.Exec(ctx, `WITH expired AS (
		SELECT t.asset_id,t.metric,t.bucket FROM history_tier_hours t JOIN assets a ON a.id=t.asset_id
		WHERE a.organization_id=$1 AND t.metric=$2 AND t.bucket<$3 ORDER BY t.bucket LIMIT $4
	) DELETE FROM history_tier_hours t USING expired e
	  WHERE t.asset_id=e.asset_id AND t.metric=e.metric AND t.bucket=e.bucket`,
		policy.OrganizationID, policy.Metric, hourlyCutoff, limit)
	if err != nil {
		return result, err
	}
	result.RemovedHours = expired.RowsAffected()
	_, err = tx.Exec(ctx, `UPDATE history_tier_policy SET archive_through=$3,worked_at=clock_timestamp()
		WHERE organization_id=$1 AND metric=$2`, policy.OrganizationID, policy.Metric, through)
	if err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

func (s *Postgres) ReadHistoryTier(ctx context.Context, asset, metric string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 90*24*time.Hour)
	if err != nil || !evidenceUUID.MatchString(asset) || !historyTierMetric(metric) ||
		params.Get("to") == "" || !from.Equal(from.Truncate(time.Hour)) || !to.Equal(to.Truncate(time.Hour)) {
		return nil, ErrHistoryTierInput
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	values, err := s.queryJSON(ctx, `WITH bounds AS (
		SELECT greatest($3::timestamptz,coalesce(date_trunc('hour',now(),'UTC')-make_interval(days=>p.hourly_days),$3)) AS start
		FROM assets a LEFT JOIN history_tier_policy p ON p.organization_id=a.organization_id AND p.metric=$2 WHERE a.id=$1
	), raw AS (
		SELECT date_trunc('hour',s.collected_at,'UTC') AS bucket,(s.metrics_json#>dbsphere_tier_path($2))::numeric AS value
		FROM samples s WHERE s.asset_id=$1 AND s.collected_at>=(SELECT start FROM bounds) AND s.collected_at<$4
		AND jsonb_typeof(s.metrics_json#>dbsphere_tier_path($2))='number'
	), partial AS (
		SELECT bucket,count(*)::integer AS samples,sum(value) AS total,min(value) AS low,max(value) AS peak,
		  true AS has_raw,false AS has_archive FROM raw GROUP BY bucket
		UNION ALL
		SELECT bucket,samples,total,low,peak,false,true FROM history_tier_hours
		WHERE asset_id=$1 AND metric=$2 AND bucket>=(SELECT start FROM bounds) AND bucket<$4
	)
	SELECT jsonb_build_object('bucket',bucket,'samples',sum(samples),'average',round(sum(total)/sum(samples),3),
		'min',min(low),'max',max(peak),'raw',bool_or(has_raw),'hourly',bool_or(has_archive))
	FROM partial GROUP BY bucket ORDER BY bucket`, asset, metric, from, to)
	return map[string]any{"assetId": asset, "metric": metric, "buckets": values}, err
}
