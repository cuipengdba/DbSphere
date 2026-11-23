package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/jackc/pgx/v5"
)

// ReadCollection exposes only fields already present in the historical browser API.
// Credentials and raw SQL are deliberately absent from these projections.
func (s *Postgres) ReadCollection(ctx context.Context, path string) (any, bool, error) {
	return s.readCollection(ctx, path, nil)
}

var scopedCollectionPredicates = map[string]string{
	"/api/assets":               "WHERE id = ANY($1::uuid[])",
	"/api/monitoring/instances": "AND a.id = ANY($1::uuid[])",
	"/api/events":               "WHERE e.asset_id = ANY($1::uuid[])",
	"/api/alerts":               "AND s.asset_id = ANY($1::uuid[])",
	"/api/alert-workbench":      "AND e.asset_id = ANY($1::uuid[])",
	"/api/silences":             "WHERE asset_id = ANY($1::uuid[])",
	"/api/notifications":        "WHERE v.asset_id = ANY($1::uuid[])",
	"/api/inspection-schedules": "WHERE asset_id = ANY($1::uuid[])",
	"/api/inspection-runs":      "WHERE asset_id = ANY($1::uuid[])",
}

// ReadScopedCollection applies the asset filter before list limits.
func (s *Postgres) ReadScopedCollection(ctx context.Context, path string, assetIDs []string) (any, bool, error) {
	if assetIDs == nil || scopedCollectionPredicates[path] == "" {
		return nil, true, errors.New("collection has no scoped asset query")
	}
	return s.readCollection(ctx, path, assetIDs)
}

func (s *Postgres) readCollection(ctx context.Context, path string, assetIDs []string) (any, bool, error) {
	queries := map[string]struct{ key, sql string }{
		"/api/assets":               {"assets", "SELECT " + assetJSON + " FROM assets /*asset_scope*/ ORDER BY created_at DESC"},
		"/api/monitoring/instances": {"instances", monitoringInstancesSQL},
		"/api/events": {"events", `SELECT jsonb_build_object(
			'id',e.id,'assetId',e.asset_id,'assetName',a.name,'kind',e.kind,
			'eventGroup',e.event_group,'title',e.title,'status',e.status,
			'startedAt',e.started_at,'endedAt',e.ended_at,'acknowledgedAt',e.acknowledged_at)
			FROM events e JOIN assets a ON a.id=e.asset_id /*asset_scope*/ ORDER BY e.started_at DESC LIMIT 200`},
		"/api/alerts": {"alerts", `SELECT jsonb_build_object(
			'assetId',s.asset_id,'assetName',a.name,'ruleId',s.rule_id,
			'rule',r.definition_json,'state',s.state_json)
			FROM alert_states s JOIN assets a ON a.id=s.asset_id
			JOIN alert_rules r ON r.id=s.rule_id WHERE s.state_json->>'status'='firing' /*asset_scope*/`},
		"/api/alert-workbench": {"incidents", alertWorkbenchSQL},
		"/api/alert-rules": {"rules", `SELECT definition_json || jsonb_build_object('editable',
			definition_json->>'metric' IN ('pg.unavailable','collector.pg_failed',
                        'collector.missed_intervals','collector.k8s_failed','collector.linux_failed',
                        'collector.patroni_failed','patroni.leader_count_invalid',
			'pg.connection_utilization','pg.database_growth_bytes_per_day','pg.blocked_sessions',
			'pg.long_transactions','pg.replication_lag_bytes','pg.logical_slot_retained_bytes','pg.wal_receiver_down',
			'pg.archive_fail_delta',
			'pg.autovacuum_disabled','pg.vacuum_backlog_percent',
			'linux.cpu_used_percent','linux.memory_used_percent','linux.disk_used_percent',
			'k8s.pod_not_ready','k8s.pod_unscheduled','k8s.scheduling_failure_recent',
			'k8s.restart_delta',
			'k8s.oom_recent','k8s.node_pressure','k8s.pvc_used_percent',
			'pgbouncer.waiting_clients'))
			FROM alert_rules ORDER BY id`},
		"/api/silences": {"silences", `SELECT jsonb_build_object(
			'id',id,'kind',kind,'assetId',asset_id,'ruleId',rule_id,
			'startsAt',starts_at,'endsAt',ends_at,'reason',reason,
			'createdAt',created_at,'revokedAt',revoked_at)
			FROM silences /*asset_scope*/ ORDER BY created_at DESC LIMIT 200`},
		"/api/notifications": {"notifications", `SELECT jsonb_build_object(
			'id',n.id,'channel',n.channel,'status',n.status,'attempts',n.attempts,
			'nextAttemptAt',n.next_attempt_at,'deliveredAt',n.delivered_at,
			'lastError',n.last_error,'kind',e.kind,'observedAt',e.observed_at,
			'eventId',v.id,'assetId',v.asset_id,'assetName',a.name)
			FROM notification_outbox n JOIN evidence e ON e.id=n.evidence_id
			JOIN events v ON v.id=e.event_id JOIN assets a ON a.id=v.asset_id
			/*asset_scope*/ ORDER BY e.observed_at DESC,n.id DESC LIMIT 100`},
		"/api/inspection-templates": {"templates", `SELECT definition_json FROM inspection_templates ORDER BY id`},
		"/api/inspection-schedules": {"schedules", `SELECT jsonb_build_object(
			'assetId',asset_id,'templateId',template_id,'intervalMinutes',interval_minutes,
			'nextDueAt',next_due_at,'enabled',enabled)
			FROM inspection_schedules /*asset_scope*/ ORDER BY next_due_at`},
		"/api/inspection-runs": {"runs", `SELECT jsonb_build_object(
			'id',id,'assetId',asset_id,'templateId',template_id,
			'templateVersion',template_version,'sampleId',sample_id,'retryOf',retry_of,
			'startedAt',started_at,'completedAt',completed_at,'status',status,
			'error',error,'eventIds',event_ids_json,
			'summary',summary_json,'findings',findings_json)
			FROM inspection_runs /*asset_scope*/ ORDER BY completed_at DESC LIMIT 50`},
	}
	if path == "/api/storage" {
		value, err := s.storageSnapshot(ctx)
		return map[string]any{"storage": value}, true, err
	}
	query, ok := queries[path]
	if !ok {
		return nil, false, nil
	}
	if assetIDs == nil {
		query.sql = strings.ReplaceAll(query.sql, "/*asset_scope*/", "")
	} else {
		if !strings.Contains(query.sql, "/*asset_scope*/") {
			return nil, true, errors.New("collection query has no asset scope marker")
		}
		query.sql = strings.ReplaceAll(query.sql, "/*asset_scope*/", scopedCollectionPredicates[path])
	}
	var rows pgx.Rows
	var err error
	if assetIDs == nil {
		rows, err = s.pool.Query(ctx, query.sql)
	} else {
		rows, err = s.pool.Query(ctx, query.sql, assetIDs)
	}
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", path, err)
	}
	defer rows.Close()
	values, err := jsonRows(rows)
	if err != nil {
		return nil, true, fmt.Errorf("decode %s: %w", path, err)
	}
	return map[string]any{query.key: values}, true, nil
}

func jsonRows(rows pgx.Rows) ([]json.RawMessage, error) {
	values := []json.RawMessage{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		values = append(values, json.RawMessage(raw))
	}
	return values, rows.Err()
}

func (s *Postgres) storageSnapshot(ctx context.Context) (json.RawMessage, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `WITH sample_stats AS (
		SELECT count(*) AS sample_count,min(collected_at) AS oldest_sample_at,
			max(collected_at) AS newest_sample_at,
			count(*) FILTER (WHERE collected_at>=now()-interval '7 days') AS recent_samples
		FROM samples), event_stats AS (
		SELECT count(*) AS event_count,count(*) FILTER (WHERE status='open') AS open_event_count
		FROM events), sizes AS (
		SELECT pg_database_size(current_database()) AS database_bytes,
			(SELECT coalesce(sum(pg_total_relation_size(inhrelid)),0) FROM pg_inherits
			WHERE inhparent='samples'::regclass) AS sample_partition_bytes,
			(SELECT coalesce(sum(pg_total_relation_size(inhrelid)),0) FROM pg_inherits
			WHERE inhparent='events'::regclass) AS event_partition_bytes)
		SELECT jsonb_build_object('mode','postgresql18',
			'databaseBytes',sizes.database_bytes,
			'samplePartitionBytes',sizes.sample_partition_bytes,
			'eventPartitionBytes',sizes.event_partition_bytes,
			'sampleCount',sample_stats.sample_count,
			'eventCount',event_stats.event_count,
			'openEventCount',event_stats.open_event_count,
			'oldestSampleAt',sample_stats.oldest_sample_at,
			'newestSampleAt',sample_stats.newest_sample_at,
			'estimatedSamplesPerDay',round(sample_stats.recent_samples::numeric/7),
			'estimatedSampleBytesPerDay',CASE WHEN sample_stats.sample_count=0 THEN 0 ELSE
				round(sizes.sample_partition_bytes::numeric/sample_stats.sample_count *
				round(sample_stats.recent_samples::numeric/7)) END,
			'retentionDays',nullif($1::int,0),'eventRetentionDays',nullif($2::int,0),
			'backup',$3::jsonb)
		FROM sample_stats,event_stats,sizes`, s.sampleRetentionDays, s.eventRetentionDays, collector.StoreBackup(time.Now().UTC())).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("read PostgreSQL 18 storage status: %w", err)
	}
	return json.RawMessage(raw), nil
}
