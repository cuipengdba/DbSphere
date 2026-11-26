package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRecoveryInventoryInput = errors.New("invalid recovery inventory input")

// RecoveryInventory reads the connected monitoring database in one snapshot.
// It can be run on the source and on an isolated recovered database.
func (s *Postgres) RecoveryInventory(ctx context.Context, eventID string) (any, error) {
	if OrganizationFromContext(ctx) != "" || (eventID != "" && !evidenceUUID.MatchString(eventID)) {
		return nil, ErrRecoveryInventoryInput
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var inventory json.RawMessage
	err = tx.QueryRow(ctx, `SELECT jsonb_build_object(
		'observedAt',statement_timestamp(),'serverVersion',current_setting('server_version'),
		'schemaVersion',(SELECT max(version) FROM dbsphere_schema_migrations),
		'counts',jsonb_build_object('assets',(SELECT count(*) FROM assets),'rules',(SELECT count(*) FROM alert_rules),
			'ruleRevisions',(SELECT count(*) FROM alert_rule_revisions),'collectionJobs',(SELECT count(*) FROM collection_jobs),
			'inspectionSchedules',(SELECT count(*) FROM inspection_schedules),'events',(SELECT count(*) FROM event_keys),
			'samples',samples.total,'hourlyBuckets',(SELECT count(*) FROM history_tier_hours),
			'logFiles',(SELECT count(*) FROM log_files),'logRecords',(SELECT count(*) FROM log_records),
			'logArchives',(SELECT count(*) FROM log_files WHERE stored_size>0 AND archive_expired_at IS NULL)),
		'sampleFrom',samples.first,'sampleThrough',samples.last)
		FROM (SELECT count(*) AS total,min(collected_at) AS first,max(collected_at) AS last FROM samples) samples`).Scan(&inventory)
	if err != nil {
		return nil, err
	}
	var event json.RawMessage
	if eventID != "" {
		err = tx.QueryRow(ctx, `SELECT jsonb_build_object('id',k.id,'assetId',k.asset_id,'status',e.status,
			'startedAt',e.started_at,'endedAt',e.ended_at,
			'samples',coalesce((SELECT jsonb_agg(item ORDER BY item->>'id') FROM (
				SELECT jsonb_build_object('id',r.sample_id,'available',EXISTS(SELECT 1 FROM samples s
					WHERE s.asset_id=k.asset_id AND s.id=r.sample_id::uuid)) AS item
				FROM (SELECT DISTINCT refs.sample_id FROM evidence x JOIN evidence_sample_refs refs ON refs.evidence_id=x.id
					WHERE x.event_id=k.id AND refs.sample_id ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'
					ORDER BY refs.sample_id LIMIT 101) r
			) checked),'[]'::jsonb),
			'logs',coalesce((SELECT jsonb_agg(item ORDER BY item->>'id',item->>'batchId') FROM (
				SELECT jsonb_build_object('id',ref.record_id,'batchId',ref.batch_id,
					'indexed',r.id IS NOT NULL,'available',coalesce(f.stored_size>0 AND f.archive_expired_at IS NULL
						AND r.raw_offset>=0 AND r.raw_length>0 AND r.raw_offset+r.raw_length<=f.raw_size
						AND (SELECT count(*) FROM log_archive_blocks b WHERE b.file_id=f.id
							AND b.part BETWEEN span.first_part AND span.last_part)=span.last_part-span.first_part+1,false)) AS item
				FROM (SELECT DISTINCT x.payload_json->>'recordId' AS record_id,x.payload_json->>'batchId' AS batch_id
					FROM evidence x WHERE x.event_id=k.id AND x.kind='log_record'
					ORDER BY 1,2 LIMIT 101) ref
				LEFT JOIN log_records r ON r.id=CASE WHEN ref.record_id ~ '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$'
					THEN ref.record_id::uuid END AND r.batch_id::text=ref.batch_id
				LEFT JOIN log_files f ON f.id=r.file_id
				CROSS JOIN LATERAL (SELECT greatest(0,r.raw_offset-2048)/$2::bigint AS first_part,
					(least(f.raw_size,r.raw_offset+least(r.raw_length,$2::bigint)+2048)-1)/$2::bigint AS last_part) span
			) checked),'[]'::jsonb))
			FROM event_keys k JOIN events e ON e.id=k.id AND e.started_at=k.started_at WHERE k.id=$1`, eventID, LogArchiveBlockBytes).Scan(&event)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	return map[string]any{"inventory": inventory, "eventId": eventID, "event": event}, tx.Commit(ctx)
}
