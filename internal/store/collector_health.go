package store

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

func migrateCollectorHealth(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=52)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE collection_jobs ADD COLUMN IF NOT EXISTS isolated boolean NOT NULL DEFAULT false,
		ADD COLUMN IF NOT EXISTS last_queue_ms bigint, ADD COLUMN IF NOT EXISTS last_query_ms bigint,
		ADD COLUMN IF NOT EXISTS last_store_ms bigint, ADD COLUMN IF NOT EXISTS last_success_at timestamptz,
		ADD COLUMN IF NOT EXISTS last_replica_id text;
		UPDATE collection_jobs SET isolated=failures>=2,
			last_success_at=CASE WHEN last_outcome='ok' THEN last_finished_at END
			WHERE failures>=2 OR last_outcome='ok';
		CREATE INDEX IF NOT EXISTS collection_jobs_lane_due ON collection_jobs(isolated,due_at,asset_id);
		INSERT INTO dbsphere_schema_migrations(version) VALUES(52)`)
	return err
}

type incidentCollectionGap struct {
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Seconds  int64     `json:"seconds"`
	SampleID string    `json:"sampleId"`
}

func incidentCollectionGaps(samples map[string]sampleObservation) []incidentCollectionGap {
	var gaps []incidentCollectionGap
	for _, sample := range samples {
		observed := object(sample.Metrics["collector"])
		since, err := time.Parse(time.RFC3339Nano, stringValue(observed["gapSince"]))
		if err != nil || !since.Before(sample.At) {
			continue
		}
		gaps = append(gaps, incidentCollectionGap{Since: since, Until: sample.At,
			Seconds: int64(sample.At.Sub(since).Seconds()), SampleID: sample.ID})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Until.Before(gaps[j].Until) })
	return gaps
}

// CollectionHealth summarizes the latest scheduled attempt for each target.
// Old versions leave timing fields null. Last successful sampling age remains
// independent of failures, so a recent connection error cannot look healthy.
func (s *Postgres) CollectionHealth(ctx context.Context, interval int) (any, error) {
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object(
		'organizationId',a.organization_id,'organization',o.name,'source',a.runtime,
		'targets',count(*),'isolated',count(*) FILTER (WHERE j.isolated),
		'timedTargets',count(j.last_queue_ms),'queryTargets',count(j.last_query_ms),'storeTargets',count(j.last_store_ms),
		'fresh',count(*) FILTER (WHERE j.last_success_at>=now()-make_interval(secs=>2*$1)),
		'latestSampleAt',max(a.last_sample_at),'oldestSuccessfulAt',min(j.last_success_at),
		'queueP95Ms',percentile_disc(0.95) WITHIN GROUP (ORDER BY j.last_queue_ms),
		'queryP95Ms',percentile_disc(0.95) WITHIN GROUP (ORDER BY j.last_query_ms),
		'storeP95Ms',percentile_disc(0.95) WITHIN GROUP (ORDER BY j.last_store_ms))
		FROM assets a JOIN organizations o ON o.id=a.organization_id
		LEFT JOIN collection_jobs j ON j.asset_id=a.id
		WHERE a.runtime IN ('linux','k8s','managed','mysql') AND ($2::uuid IS NULL OR a.organization_id=$2)
		GROUP BY a.organization_id,o.name,a.runtime ORDER BY o.name,a.organization_id,a.runtime`,
		interval, nullable(OrganizationFromContext(ctx)))
	return map[string]any{"groups": rows, "freshnessSeconds": 2 * interval}, err
}

func ensureCollectorLayer(metrics map[string]any) map[string]any {
	layer, ok := metrics["collector"].(map[string]any)
	if !ok || layer == nil {
		layer = make(map[string]any)
		metrics["collector"] = layer
	}
	return layer
}
