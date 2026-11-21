package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
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

func (s *Postgres) loadIncidentSamples(ctx context.Context, assetID string, ids []string) (map[string]sampleObservation, error) {
	result := make(map[string]sampleObservation)
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (s.id) s.id::text,s.collected_at,s.status,s.metrics_json
		FROM samples s WHERE s.asset_id=$1 AND s.id=ANY($2::uuid[]) ORDER BY s.id,s.collected_at DESC`, assetID, ids)
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

type replayPoint struct {
	EvidenceID string     `json:"evidenceId,omitempty"`
	SampleID   string     `json:"sampleId,omitempty"`
	SampleAt   *time.Time `json:"sampleAt,omitempty"`
	At         time.Time  `json:"at"`
	Layer      string     `json:"layer"`
	Stage      string     `json:"stage"`
	Kind       string     `json:"kind"`
	Signal     string     `json:"signal,omitempty"`
	Source     string     `json:"source,omitempty"`
	Strength   string     `json:"strength"`
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
		point.Layer = incidentLayer(item.Kind, point.Signal)
		if point.Layer == "" {
			continue
		}
		if sample, ok := samples[stringValue(payload["sampleId"])]; ok && sample.Metrics != nil {
			point.SampleID, point.SampleAt = sample.ID, &sample.At
			point.Strength = "direct"
			layers[point.Layer] = true
		}
		switch item.Kind {
		case "collection_error", "alert_firing":
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
		case "operator_response", "event_acknowledged", "rca_decision", "work_order_executed":
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
		case "lock_chain", "same_backend_pid_lock_chain", "wal_receiver_down":
			return "direct"
		}
		return "correlated"
	}
	if len(supports) == 0 {
		return "insufficient"
	}
	return "correlated"
}

func incidentLayer(kind, metric string) string {
	switch {
	case kind == "ai_advisory":
		return "ai"
	case kind == "operator_response" || kind == "event_acknowledged" || kind == "rca_decision" || kind == "work_order_executed":
		return "response"
	case strings.HasPrefix(kind, "k8s_") || strings.HasPrefix(metric, "k8s."):
		return "k8s"
	case strings.HasPrefix(kind, "linux_") || strings.HasPrefix(metric, "linux."):
		return "linux"
	case strings.HasPrefix(kind, "patroni_") || strings.HasPrefix(metric, "patroni.") || strings.Contains(metric, "replication_lag") ||
		strings.Contains(metric, "wal_receiver") || strings.Contains(metric, "archive_fail"):
		return "replication"
	case kind == "pgbouncer_state" || strings.HasPrefix(metric, "pgbouncer."):
		return "pooler"
	case strings.HasPrefix(kind, "sql_") || strings.HasPrefix(metric, "pg.sql_"):
		return "sql"
	case kind == "alert_firing" || kind == "alert_recovered" || kind == "collection_error" || kind == "collection_recovered" ||
		kind == "recovery_verified" || strings.HasPrefix(kind, "pg_"):
		return "postgres"
	}
	return ""
}
