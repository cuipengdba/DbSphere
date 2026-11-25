package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type sampleReference struct{ id, relation string }

func evidenceSampleReferences(item eventEvidence) []sampleReference {
	var payload map[string]any
	if json.Unmarshal(item.Payload, &payload) != nil {
		return nil
	}
	var references []sampleReference
	if id := stringValue(payload["sampleId"]); id != "" {
		references = append(references, sampleReference{id, "observed_in"})
	}
	detail := object(payload["detail"])
	switch stringValue(payload["metric"]) {
	case "pg.sql_regression_ratio", "pg.database_growth_bytes_per_day":
		if id := stringValue(detail["baselineSampleId"]); id != "" {
			references = append(references, sampleReference{id, "compared_with"})
		}
		if stringValue(payload["metric"]) == "pg.sql_regression_ratio" {
			if id := stringValue(detail["previousSampleId"]); id != "" {
				references = append(references, sampleReference{id, "compared_with"})
			}
		}
	}
	return references
}

func (s *Postgres) loadIncidentSamples(ctx context.Context, assetID string, ids []string, observedAt map[string]time.Time) (map[string]sampleObservation, error) {
	result := make(map[string]sampleObservation)
	if len(ids) == 0 {
		return result, nil
	}
	hintedIDs := []string{}
	hintedTimes := []time.Time{}
	for _, id := range ids {
		if at, ok := observedAt[id]; ok {
			hintedIDs, hintedTimes = append(hintedIDs, id), append(hintedTimes, at)
		}
	}
	// Incident evidence commonly uses the exact sample timestamp. Resolve it
	// within a single partition when possible and search historical partitions
	// only for unmatched IDs, such as SQL baselines and older evidence formats.
	rows, err := s.pool.Query(ctx, `WITH matched AS MATERIALIZED (
		SELECT found.* FROM unnest($3::uuid[],$4::timestamptz[]) AS hint(id,at)
		CROSS JOIN LATERAL (SELECT id,collected_at,status,metrics_json FROM samples
			WHERE asset_id=$1 AND id=hint.id AND collected_at=hint.at LIMIT 1) found
	), remaining AS (
		SELECT id FROM unnest($2::uuid[]) requested(id) WHERE NOT EXISTS (SELECT 1 FROM matched m WHERE m.id=requested.id)
	)
	SELECT id::text,collected_at,status,metrics_json FROM matched
	UNION ALL
	SELECT found.id::text,found.collected_at,found.status,found.metrics_json FROM remaining r
	CROSS JOIN LATERAL (SELECT id,collected_at,status,metrics_json FROM samples
		WHERE asset_id=$1 AND id=r.id ORDER BY collected_at DESC LIMIT 1) found`, assetID, ids, hintedIDs, hintedTimes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sample sampleObservation
		var metrics []byte
		if err := rows.Scan(&sample.ID, &sample.At, &sample.Status, &metrics); err != nil {
			return nil, err
		}
		if json.Unmarshal(metrics, &sample.Metrics) != nil {
			sample.Metrics = nil
		}
		result[sample.ID] = sample
	}
	return result, rows.Err()
}

// Legacy state rows omitted the sample ID. Restore links only when the original
// sample referenced by this event has the same timestamp and identical state.
func linkLegacyLayerEvidence(item *eventEvidence, samplesAt map[int64][]sampleObservation) string {
	section := map[string]string{"k8s_state": "k8s", "linux_state": "linux", "pgbouncer_state": "pgbouncer", "patroni_state": "patroni"}[item.Kind]
	if section == "" {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(item.Payload, &payload) != nil || len(payload) == 0 || payload["sampleId"] != nil {
		return ""
	}
	matched := ""
	for _, sample := range samplesAt[item.ObservedAt.UnixNano()] {
		if !reflect.DeepEqual(payload, sample.Metrics[section]) {
			continue
		}
		if matched != "" {
			return ""
		}
		matched = sample.ID
	}
	if matched != "" {
		payload["sampleId"] = matched
		item.Payload, _ = json.Marshal(payload)
	}
	return matched
}

type replayPoint struct {
	EvidenceID string     `json:"evidenceId,omitempty"`
	SampleID   string     `json:"sampleId,omitempty"`
	SampleAt   *time.Time `json:"sampleAt,omitempty"`
	At         time.Time  `json:"at"`
	EndAt      *time.Time `json:"endAt,omitempty"`
	Layer      string     `json:"layer"`
	Stage      string     `json:"stage"`
	Kind       string     `json:"kind"`
	Signal     string     `json:"signal,omitempty"`
	Value      *float64   `json:"value,omitempty"`
	Source     string     `json:"source,omitempty"`
	Strength   string     `json:"strength"`
	Phase      string     `json:"phase,omitempty"`
	TraceID    string     `json:"traceId,omitempty"`
	SpanID     string     `json:"spanId,omitempty"`
	BackendPID int        `json:"backendPid,omitempty"`
	Limitation string     `json:"limitation,omitempty"`
}

func incidentReplay(event *eventRecord, samples map[string]sampleObservation, candidates []map[string]any) map[string]any {
	points := []replayPoint{}
	var detection, recovery *replayPoint
	seen := map[string]bool{}
	layers := map[string]bool{}
	for _, item := range event.Evidence {
		var payload map[string]any
		if json.Unmarshal(item.Payload, &payload) != nil {
			continue
		}
		point := replayPoint{EvidenceID: item.ID, At: item.ObservedAt, Kind: item.Kind, Stage: "observation", Strength: "insufficient"}
		point.Signal, point.Source = stringValue(payload["metric"]), stringValue(payload["source"])
		if value, ok := numeric(payload["value"]); ok {
			point.Value = &value
		}
		point.Layer = incidentLayer(item.Kind, point.Signal)
		if item.Kind == mysqlLockGroup || point.Source == "go_mysql_collector" ||
			(event.Kind == mysqlLockGroup && item.Kind == "collection_recovered") {
			point.Layer = "mysql"
		} else if item.Kind == dm8LockGroup || point.Source == "go_dm8_collector" ||
			(event.Kind == dm8LockGroup && item.Kind == "collection_recovered") {
			point.Layer = "dm8"
		} else if item.Kind == kingbaseLockGroup || point.Source == "go_kingbase_collector" ||
			(event.Kind == kingbaseLockGroup && item.Kind == "collection_recovered") {
			point.Layer = "kingbase"
		} else if item.Kind == redisEvictionGroup || item.Kind == redisSentinelGroup || item.Kind == redisSlotsGroup ||
			point.Source == "go_redis_collector" || item.Kind == "collection_recovered" &&
			(event.Kind == redisEvictionGroup || event.Kind == redisSentinelGroup || event.Kind == redisSlotsGroup) {
			point.Layer = "redis"
		} else if item.Kind == etcdLeaderGroup || item.Kind == etcdHealthGroup || item.Kind == etcdDiskGroup ||
			point.Source == "go_etcd_collector" || item.Kind == "collection_recovered" &&
			(event.Kind == etcdLeaderGroup || event.Kind == etcdHealthGroup || event.Kind == etcdDiskGroup) {
			point.Layer = "etcd"
		}
		if point.Layer == "" {
			continue
		}
		if sample, ok := samples[stringValue(payload["sampleId"])]; ok && sample.Metrics != nil {
			point.SampleID, point.SampleAt = sample.ID, &sample.At
			point.Strength = "direct"
			if point.At.Sub(sample.At) > 10*time.Minute || sample.At.Sub(point.At) > time.Minute {
				point.Strength, point.Limitation = "correlated", "证据引用的采样与记录时刻相隔较远"
			}
			layers[point.Layer] = true
		}
		switch item.Kind {
		case "collection_error", "alert_firing", "alert_unsuppressed", mysqlLockGroup, dm8LockGroup, kingbaseLockGroup,
			redisEvictionGroup, redisSentinelGroup, redisSlotsGroup, etcdLeaderGroup, etcdHealthGroup, etcdDiskGroup:
			point.Stage = "detection"
			if detection == nil || (detection.SampleID == "" && point.SampleID != "") {
				value := point
				detection = &value
			}
		case "collection_recovered", "alert_recovered", "recovery_verified":
			point.Stage = "recovery"
			if point.SampleID != "" || recovery == nil {
				value := point
				recovery = &value
			}
		case "operator_response", "event_acknowledged", "event_handoff", "rca_decision", "work_order_executed",
			"notification_delivery", "notification_retry":
			point.Stage, point.Strength = "response", "record"
		case "ai_advisory":
			point.Strength = "correlated"
		}
		key := point.SampleID + "|" + point.Kind + "|" + point.Signal
		if point.SampleID == "" || !seen[key] {
			points = append(points, point)
		}
		seen[key] = true
	}
	if detection == nil {
		detection = &replayPoint{At: event.StartedAt, Kind: "event_started", Layer: "postgres", Stage: "detection", Strength: "record"}
	}
	if event.EndedAt != nil && recovery == nil {
		recovery = &replayPoint{At: *event.EndedAt, Kind: "event_ended", Layer: "postgres", Stage: "recovery", Strength: "record"}
	}
	for _, candidate := range candidates {
		candidate["evidenceStrength"] = candidateEvidenceStrength(candidate, samples)
	}
	return map[string]any{"eventId": event.ID, "assetId": event.AssetID, "startedAt": event.StartedAt, "endedAt": event.EndedAt,
		"detection": detection, "recovery": recovery, "points": points, "observedLayers": layers}
}

func candidateEvidenceStrength(candidate map[string]any, samples map[string]sampleObservation) string {
	if stringValue(object(candidate["timeOrder"])["relation"]) == "after_event" {
		return "correlated"
	}
	if against, ok := candidate["against"].([]string); ok && len(against) != 0 {
		return "correlated"
	}
	supports, _ := candidate["supports"].([]string)
	hasRawSample := false
	for _, support := range supports {
		if sample, ok := samples[strings.TrimPrefix(support, "sample:")]; ok && sample.Metrics != nil {
			hasRawSample = true
			break
		}
	}
	if hasRawSample {
		switch stringValue(candidate["code"]) {
		case "lock_chain", "trace_lock_pid_matched", "wal_receiver_down", "mysql_innodb_lock_wait", "dm8_transaction_lock", "kingbase_session_lock",
			"redis_eviction_confirmed", "redis_sentinel_master_observed", "redis_slots_observed",
			"etcd_leader_observed", "etcd_disk_observed", "etcd_health_observed":
			return "direct"
		}
		return "correlated"
	}
	if len(supports) == 0 {
		return "insufficient"
	}
	return "correlated"
}

type replayComparison struct {
	Phase    string             `json:"phase"`
	SampleID string             `json:"sampleId,omitempty"`
	At       *time.Time         `json:"at,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
}

func (s *Postgres) enrichIncidentReplay(ctx context.Context, event *eventRecord, replay map[string]any,
	samples map[string]sampleObservation, traces []replayPoint, edges []map[string]string) error {
	points := replay["points"].([]replayPoint)
	layers := replay["observedLayers"].(map[string]bool)
	exact := map[string]bool{}
	for _, edge := range edges {
		if edge["relation"] == "same_backend_pid" {
			exact[edge["from"]] = true
		}
	}
	for _, trace := range traces {
		if trace.Strength != "insufficient" && exact["span:"+trace.TraceID+":"+trace.SpanID] {
			trace.Strength, trace.Limitation = "direct", "可核对这条数据库 Span 对应的等待 PID 和阻塞链"
		} else if trace.Strength == "correlated" && tracePIDConflict(trace, samples) {
			trace.Strength, trace.Limitation = "conflict", "同窗锁样本的等待 PID 与应用上报不符，仍需核对连接和采样范围"
		}
		points = append(points, trace)
		layers["app"] = true
	}
	for i := range points {
		point := &points[i]
		point.Phase = "during"
		switch {
		case point.Stage == "recovery" || event.EndedAt != nil && !point.At.Before(*event.EndedAt):
			point.Phase = "after"
		case point.At.Before(event.StartedAt) && (point.EndAt == nil || point.EndAt.Before(event.StartedAt)):
			point.Phase = "before"
		}
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].At.Before(points[j].At) })
	replay["points"] = points
	baseline := sampleObservation{}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT id::text,collected_at,status,metrics_json FROM samples WHERE asset_id=$1
		AND collected_at >= $2::timestamptz-interval '30 minutes' AND collected_at < $2::timestamptz
		AND (status='ok' OR metrics_json#>>'{collector,pgStatus}'='ok')
		ORDER BY collected_at DESC,id DESC LIMIT 1`, event.AssetID, event.StartedAt).Scan(&baseline.ID, &baseline.At, &baseline.Status, &raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if raw != nil {
		_ = json.Unmarshal(raw, &baseline.Metrics)
	}
	reference := func(point *replayPoint) sampleObservation {
		if point != nil {
			return samples[point.SampleID]
		}
		return sampleObservation{}
	}
	replay["comparison"] = []replayComparison{compareReplaySample("before", baseline),
		compareReplaySample("during", reference(replay["detection"].(*replayPoint))),
		compareReplaySample("after", reference(replay["recovery"].(*replayPoint)))}
	for _, point := range points {
		if point.Stage != "detection" || point.SampleID == "" {
			continue
		}
		peak, err := s.incidentPeak(ctx, event, &point, samples[point.SampleID])
		if err != nil {
			return err
		}
		if peak != nil {
			replay["peak"] = peak
			break
		}
	}
	return nil
}

func tracePIDConflict(point replayPoint, samples map[string]sampleObservation) bool {
	if point.Signal != "postgresql_client" || point.BackendPID == 0 || point.EndAt == nil {
		return false
	}
	seenWait := false
	for _, sample := range samples {
		if sample.At.Before(point.At) || sample.At.After(*point.EndAt) {
			continue
		}
		chain := object(object(sample.Metrics["diagnostics"])["lockChain"])
		if stringValue(chain["status"]) == "ok" && len(list(chain["data"])) > 0 {
			if matchedLockWaitBlocker(sample, point.At, *point.EndAt, point.BackendPID) > 0 {
				return false
			}
			seenWait = true
		}
	}
	return seenWait
}

func compareReplaySample(phase string, sample sampleObservation) replayComparison {
	result := replayComparison{Phase: phase}
	if sample.ID == "" || sample.Metrics == nil {
		return result
	}
	result.SampleID, result.At = sample.ID, &sample.At
	result.Metrics = map[string]float64{}
	if value, ok := numeric(sample.Metrics["connections"]); ok {
		result.Metrics["connections"] = value
	}
	for name, metric := range map[string]string{"blockedSessions": "pg.blocked_sessions", "poolWaiting": "pgbouncer.waiting_clients",
		"replicationLagBytes": "pg.replication_lag_bytes", "podUnready": "k8s.pod_not_ready"} {
		if signal := readGoSignal(metric, sample.Metrics, sample.At); signal.Available {
			result.Metrics[name] = signal.Value
		}
	}
	return result
}

func incidentLayer(kind, metric string) string {
	switch {
	case kind == "ai_advisory":
		return "ai"
	case kind == "notification_delivery" || kind == "notification_retry":
		return "notification"
	case kind == "operator_response" || kind == "event_acknowledged" || kind == "rca_decision" || kind == "event_handoff" || strings.HasPrefix(kind, "work_order_"):
		return "response"
	case strings.HasPrefix(metric, "container.") || metric == "k8s.pvc_used_percent" || metric == "linux.disk_used_percent":
		return "storage"
	case kind == "routing_observed" || strings.HasPrefix(metric, "routing."):
		return "entry"
	case strings.HasPrefix(kind, "k8s_") || strings.HasPrefix(metric, "k8s."):
		return "k8s"
	case strings.HasPrefix(kind, "linux_") || strings.HasPrefix(metric, "linux."):
		return "linux"
	case strings.HasPrefix(kind, "patroni_") || strings.HasPrefix(metric, "patroni.") || strings.Contains(metric, "replication_lag") ||
		strings.Contains(metric, "wal_receiver") || strings.Contains(metric, "archive_fail") || metric == "pg.logical_slot_retained_bytes":
		return "replication"
	case kind == "pgbouncer_state" || strings.HasPrefix(metric, "pgbouncer."):
		return "pooler"
	case strings.HasPrefix(kind, "sql_") || strings.HasPrefix(metric, "pg.sql_"):
		return "sql"
	case strings.HasPrefix(kind, "alert_") || kind == "collection_error" || kind == "collection_recovered" ||
		kind == "recovery_verified" || strings.HasPrefix(kind, "pg_"):
		return "postgres"
	}
	return ""
}
