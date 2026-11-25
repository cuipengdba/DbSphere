package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cuipengdba/dbsphere/internal/trace"
)

type incidentAssetScopeKey struct{}

func WithIncidentAssetScope(ctx context.Context, ids []string) context.Context {
	return context.WithValue(ctx, incidentAssetScopeKey{}, ids)
}

type dependencyCall struct {
	TraceID    string            `json:"traceId"`
	SpanID     string            `json:"spanId"`
	ParentID   string            `json:"parentId"`
	Service    string            `json:"service"`
	AssetID    string            `json:"assetId"`
	Asset      string            `json:"asset"`
	Engine     string            `json:"engine"`
	Status     string            `json:"status"`
	DurationMS float64           `json:"durationMs"`
	Sample     *dependencySample `json:"sample,omitempty"`
}

type dependencySample struct {
	ID          string    `json:"id"`
	At          time.Time `json:"at"`
	Linux       string    `json:"linux,omitempty"`
	Devices     int       `json:"devices,omitempty"`
	PodUID      string    `json:"podUid,omitempty"`
	PVCs        int       `json:"pvcs,omitempty"`
	Containers  int       `json:"containers,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Evictions   *float64  `json:"evictions,omitempty"`
	HitPercent  *float64  `json:"hitPercent,omitempty"`
}

type dependencyControlPlane struct {
	SampleID string   `json:"sampleId"`
	Context  string   `json:"context,omitempty"`
	Member   string   `json:"member,omitempty"`
	APIMean  *float64 `json:"apiMeanMs,omitempty"`
	WALMean  *float64 `json:"walMeanMs,omitempty"`
	Commit   *float64 `json:"commitMeanMs,omitempty"`
	Linux    string   `json:"linux,omitempty"`
	Devices  int      `json:"devices,omitempty"`
}

type incidentDependencyPaths struct {
	Calls        []dependencyCall        `json:"calls"`
	ControlPlane *dependencyControlPlane `json:"controlPlane,omitempty"`
	Limited      bool                    `json:"limited"`
}

// A mapped application span establishes an endpoint association. Infrastructure
// samples are only time-nearby evidence, even when the trace itself is observed.
func (s *Postgres) incidentDependencies(ctx context.Context, event *eventRecord, observed []observedTraceSpan,
	incidentSamples map[string]sampleObservation) (incidentDependencyPaths, error) {
	paths := incidentDependencyPaths{Calls: []dependencyCall{}, ControlPlane: controlPlanePath(event.StartedAt, incidentSamples)}
	traces := []string{}
	seen := map[string]bool{}
	for _, span := range observed {
		if !trace.IsDatabaseTargetSpan(span.Kind) || span.ParentSpanID == "" || seen[span.TraceID] ||
			span.Start.Before(event.StartedAt.Add(-2*time.Minute)) || span.Start.After(event.StartedAt.Add(5*time.Minute)) {
			continue
		}
		if len(traces) == 12 {
			paths.Limited = true
			break
		}
		seen[span.TraceID] = true
		traces = append(traces, span.TraceID)
	}
	if len(traces) == 0 {
		return paths, nil
	}
	ids, _ := ctx.Value(incidentAssetScopeKey{}).([]string)
	rows, err := s.pool.Query(ctx, `SELECT t.trace_id,t.span_id,t.parent_span_id,parent.service,a.id::text,a.name,
		CASE WHEN a.runtime IN ('linux','k8s','managed') THEN 'postgres' ELSE a.runtime END,
		t.status,extract(epoch FROM t.ended_at-t.started_at)::float8*1000,t.query_id,
		m.id::text,m.collected_at,m.metrics_json
		FROM assets origin JOIN assets a ON a.organization_id=origin.organization_id
		JOIN trace_spans t ON t.asset_id=a.id AND t.trace_id=ANY($2::text[]) AND t.parent_span_id IS NOT NULL
		JOIN trace_spans parent ON parent.asset_id=t.asset_id AND parent.trace_id=t.trace_id
		AND parent.span_id=t.parent_span_id AND parent.kind='service'
		AND parent.started_at<=t.ended_at AND parent.ended_at>=t.started_at
		LEFT JOIN LATERAL (SELECT id,collected_at,metrics_json FROM samples
			WHERE asset_id=a.id AND collected_at BETWEEN t.started_at-interval '90 seconds' AND t.ended_at+interval '90 seconds'
			AND status='ok' ORDER BY abs(extract(epoch FROM collected_at-t.ended_at)),id LIMIT 1) m ON true
		WHERE origin.id=$1 AND ($3::text[] IS NULL OR a.id::text=ANY($3::text[]))
		AND t.kind IN ('postgresql_client','mysql_client','redis_client','etcd_client','dm8_client','kingbase_client')
		AND t.started_at BETWEEN $4::timestamptz-interval '2 minutes' AND $4::timestamptz+interval '5 minutes'
		AND (t.received_at IS NULL OR t.ended_at<=t.received_at+interval '1 second')
		AND (parent.received_at IS NULL OR parent.ended_at<=parent.received_at+interval '1 second')
		ORDER BY t.started_at,t.trace_id,t.span_id LIMIT 65`, event.AssetID, traces, ids, event.StartedAt)
	if err != nil {
		return paths, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(paths.Calls) == 64 {
			paths.Limited = true
			break
		}
		var call dependencyCall
		var queryID, sampleID *string
		var sampledAt *time.Time
		var sampleJSON []byte
		if err := rows.Scan(&call.TraceID, &call.SpanID, &call.ParentID, &call.Service, &call.AssetID, &call.Asset,
			&call.Engine, &call.Status, &call.DurationMS, &queryID, &sampleID, &sampledAt, &sampleJSON); err != nil {
			return paths, err
		}
		if sampleID != nil && sampledAt != nil {
			var metrics map[string]any
			if json.Unmarshal(sampleJSON, &metrics) == nil {
				call.Sample = dependencySampleReading(*sampleID, *sampledAt, metrics, queryID)
			}
		}
		paths.Calls = append(paths.Calls, call)
	}
	return paths, rows.Err()
}

func dependencySampleReading(id string, at time.Time, metrics map[string]any, queryID *string) *dependencySample {
	result := &dependencySample{ID: id, At: at}
	linux := object(metrics["linux"])
	if linux["status"] == "ok" {
		result.Linux = stringValue(linux["endpoint"])
		result.Devices = len(object(object(linux["devices"])["disk"]))
		if result.Linux == "" {
			result.Linux = "Linux"
		}
	}
	kube := object(metrics["k8s"])
	if kube["status"] == "ok" {
		result.PodUID = stringValue(object(kube["pod"])["uid"])
		result.PVCs = len(list(kube["pvcs"]))
	}
	for _, item := range object(object(metrics["containerStorage"])["containers"]) {
		if object(item)["status"] == "ok" {
			result.Containers++
		}
	}
	if queryID != nil {
		result.Fingerprint = traceSQLFingerprint(map[string]sampleObservation{id: {ID: id, At: at, Status: "ok", Metrics: metrics}}, *queryID, at, at)
	}
	redis := object(metrics["redisInterval"])
	result.Evictions, result.HitPercent = dependencyNumber(redis["evicted"]), dependencyNumber(redis["hitPercent"])
	return result
}

func dependencyNumber(value any) *float64 {
	if n, ok := numeric(value); ok {
		return &n
	}
	return nil
}

func controlPlanePath(anchor time.Time, samples map[string]sampleObservation) *dependencyControlPlane {
	var closest sampleObservation
	for _, sample := range samples {
		if sample.Status == "ok" && object(sample.Metrics["etcd"]) != nil && (closest.ID == "" ||
			sample.At.Sub(anchor).Abs() < closest.At.Sub(anchor).Abs() ||
			(sample.At.Sub(anchor).Abs() == closest.At.Sub(anchor).Abs() && sample.ID < closest.ID)) {
			closest = sample
		}
	}
	if closest.ID == "" {
		return nil
	}
	etcd := object(closest.Metrics["etcd"])
	window := object(closest.Metrics["etcdInterval"])
	infra := dependencySampleReading(closest.ID, closest.At, closest.Metrics, nil)
	return &dependencyControlPlane{SampleID: closest.ID, Context: stringValue(object(etcd["kubernetes"])["context"]),
		Member: stringValue(etcd["memberId"]), APIMean: dependencyNumber(object(window["kubernetes"])["meanMs"]),
		WALMean: dependencyNumber(object(window["wal"])["meanMs"]), Commit: dependencyNumber(object(window["backend"])["meanMs"]),
		Linux: infra.Linux, Devices: infra.Devices}
}
