package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/trace"
	"github.com/jackc/pgx/v5"
)

var evidenceUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var rcaCode = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
var ErrInvalidRCADecision = errors.New("invalid RCA decision or unsupported candidate")
var ErrEventNotFound = errors.New("event not found")

type eventEvidence struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	ObservedAt time.Time       `json:"observedAt"`
	Payload    json.RawMessage `json:"payload"`
}

type eventRecord struct {
	ID             string          `json:"id"`
	AssetID        string          `json:"assetId"`
	AssetName      string          `json:"assetName"`
	ClusterName    string          `json:"clusterName,omitempty"`
	Kind           string          `json:"kind"`
	EventGroup     *string         `json:"eventGroup"`
	Title          string          `json:"title"`
	Status         string          `json:"status"`
	StartedAt      time.Time       `json:"startedAt"`
	EndedAt        *time.Time      `json:"endedAt"`
	AcknowledgedAt *time.Time      `json:"acknowledgedAt"`
	Evidence       []eventEvidence `json:"evidence"`
}

type relatedEventRecord struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	EventGroup     string `json:"eventGroup"`
	Status         string `json:"status"`
	SharedSampleID string `json:"sharedSampleId"`
}

// A shared original sample is an exact link between incidents, not a claim that
// either incident caused the other. Keep the query within the same asset.
func (s *Postgres) relatedEvents(ctx context.Context, event *eventRecord) ([]relatedEventRecord, error) {
	ids := make([]string, 0, len(event.Evidence))
	seen := make(map[string]bool)
	for _, item := range event.Evidence {
		var payload struct {
			SampleID string `json:"sampleId"`
		}
		if json.Unmarshal(item.Payload, &payload) == nil && evidenceUUID.MatchString(payload.SampleID) && !seen[payload.SampleID] {
			seen[payload.SampleID] = true
			ids = append(ids, payload.SampleID)
		}
	}
	result := []relatedEventRecord{}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id::text,e.title,e.event_group,e.status,
		MIN(ev.payload_json->>'sampleId')
		FROM events e JOIN evidence ev ON ev.event_id=e.id
		WHERE e.asset_id=$1 AND e.id<>$2
		  AND e.event_group IN ('availability','telemetry')
		  AND ev.payload_json->>'sampleId'=ANY($3::text[])
		GROUP BY e.id,e.title,e.event_group,e.status,e.started_at
		ORDER BY e.started_at DESC LIMIT 10`, event.AssetID, event.ID, ids)
	if err != nil {
		return nil, fmt.Errorf("read related events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row relatedEventRecord
		if err := rows.Scan(&row.ID, &row.Title, &row.EventGroup, &row.Status, &row.SharedSampleID); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

type serviceTraceImpact struct {
	Service             string    `json:"service"`
	Spans               int       `json:"spans"`
	DatabaseSpans       int       `json:"databaseSpans"`
	ErrorSpans          int       `json:"errorSpans"`
	ObservedTraces      int       `json:"observedTraces"`
	CorrelatedTraces    int       `json:"correlatedTraces"`
	ErrorTraces         int       `json:"errorTraces"`
	LockWaitTraces      int       `json:"lockWaitTraces"`
	SQLRegressionTraces int       `json:"sqlRegressionTraces"`
	TraceIDs            []string  `json:"traceIds"`
	FirstAt             time.Time `json:"firstAt"`
	LastAt              time.Time `json:"lastAt"`
}

type traceSamplingSummary struct {
	DeclaredSpans  int      `json:"declaredSpans"`
	MinProbability *float64 `json:"minProbability"`
	MaxProbability *float64 `json:"maxProbability"`
}

type traceClockSummary struct {
	TimedSpans           int   `json:"timedSpans"`
	FutureTimestampSpans int   `json:"futureTimestampSpans"`
	MaxApparentLeadMs    int64 `json:"maxApparentLeadMs"`
}

func (s *Postgres) loadEvent(ctx context.Context, id string) (*eventRecord, error) {
	var event eventRecord
	err := s.pool.QueryRow(ctx, `SELECT e.id::text,e.asset_id::text,a.name,coalesce(a.cluster_name,''),e.kind,e.event_group,
		e.title,e.status,e.started_at,e.ended_at,e.acknowledged_at
		FROM events e JOIN assets a ON a.id=e.asset_id WHERE e.id=$1`, id).Scan(
		&event.ID, &event.AssetID, &event.AssetName, &event.ClusterName, &event.Kind, &event.EventGroup,
		&event.Title, &event.Status, &event.StartedAt, &event.EndedAt, &event.AcknowledgedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read event: %w", err)
	}
	event.Evidence = []eventEvidence{}
	rows, err := s.pool.Query(ctx, `SELECT id::text,kind,observed_at,payload_json FROM (
		SELECT id,kind,observed_at,payload_json FROM evidence WHERE event_id=$1
		ORDER BY observed_at DESC,id DESC LIMIT 500
		) latest ORDER BY observed_at,id`, id)
	if err != nil {
		return nil, fmt.Errorf("read event evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item eventEvidence
		var payload []byte
		if err := rows.Scan(&item.ID, &item.Kind, &item.ObservedAt, &payload); err != nil {
			return nil, err
		}
		item.Payload = json.RawMessage(payload)
		event.Evidence = append(event.Evidence, item)
	}
	return &event, rows.Err()
}

func (s *Postgres) EventByID(ctx context.Context, id string) (any, error) {
	return s.eventWithRCAPolicy(ctx, id, "")
}

func (s *Postgres) eventWithRCAPolicy(ctx context.Context, id, replayCode string) (any, error) {
	event, err := s.loadEvent(ctx, id)
	if err != nil || event == nil {
		return nil, err
	}
	related, err := s.relatedEvents(ctx, event)
	if err != nil {
		return nil, err
	}
	waitCaptures, err := s.EventWaitCaptures(ctx, id)
	if err != nil {
		return nil, err
	}
	eventNode := "event:" + event.ID
	assetNode := "asset:" + event.AssetID
	assetLayer := "postgres"
	if engine, _ := nativeHAEvent(event.Kind); engine != "" {
		assetLayer = engine
	} else if event.Kind == mysqlLockGroup {
		assetLayer = "mysql"
	} else if event.Kind == dm8LockGroup {
		assetLayer = "dm8"
	} else if event.Kind == kingbaseLockGroup {
		assetLayer = "kingbase"
	} else if event.Kind == redisEvictionGroup || event.Kind == redisSentinelGroup || event.Kind == redisSlotsGroup {
		assetLayer = "redis"
	} else if event.Kind == etcdLeaderGroup || event.Kind == etcdHealthGroup || event.Kind == etcdDiskGroup {
		assetLayer = "etcd"
	}
	nodes := []map[string]any{
		{"id": eventNode, "type": "event", "label": event.Title, "at": event.StartedAt, "source": "/api/events/" + event.ID},
		{"id": assetNode, "type": assetLayer, "label": event.AssetName, "source": "/api/assets/" + event.AssetID},
	}
	edges := []map[string]string{{"from": eventNode, "to": assetNode, "relation": "affects"}}
	seen := map[string]bool{eventNode: true, assetNode: true}
	addNode := func(id, kind, label, source string, at any) {
		if seen[id] {
			return
		}
		seen[id] = true
		nodes = append(nodes, map[string]any{"id": id, "type": kind, "label": label, "source": source, "at": at})
	}
	for _, raw := range waitCaptures {
		var capture struct {
			ID        string    `json:"id"`
			StartedAt time.Time `json:"startedAt"`
		}
		if json.Unmarshal(raw, &capture) != nil {
			continue
		}
		captureNode := "wait-capture:" + capture.ID
		addNode(captureNode, "evidence", "短时等待采样", "/api/assets/"+event.AssetID+"/wait-captures?id="+capture.ID, capture.StartedAt)
		edges = append(edges, map[string]string{"from": eventNode, "to": captureNode, "relation": "contains"})
	}
	missing := []string{}
	references := make(map[string][]sampleReference, len(event.Evidence))
	seenSamples := map[string]bool{}
	observationAt := map[string]time.Time{}
	var sampleIDs []string
	for _, item := range event.Evidence {
		references[item.ID] = evidenceSampleReferences(item)
		for _, reference := range references[item.ID] {
			if evidenceUUID.MatchString(reference.id) && !seenSamples[reference.id] {
				seenSamples[reference.id] = true
				sampleIDs = append(sampleIDs, reference.id)
			}
			if reference.relation == "observed_in" && !item.ObservedAt.IsZero() {
				observationAt[reference.id] = item.ObservedAt
			}
		}
	}
	samples, err := s.loadIncidentSamples(ctx, event.AssetID, sampleIDs, observationAt)
	if err != nil {
		return nil, err
	}
	samplesAt := make(map[int64][]sampleObservation, len(samples))
	for _, sample := range samples {
		at := sample.At.UnixNano()
		samplesAt[at] = append(samplesAt[at], sample)
	}
	for i := range event.Evidence {
		item := &event.Evidence[i]
		if id := linkLegacyLayerEvidence(item, samplesAt); id != "" {
			references[item.ID] = append(references[item.ID], sampleReference{id, "observed_in"})
		}
	}
	for _, item := range event.Evidence {
		evidenceNode := "evidence:" + item.ID
		addNode(evidenceNode, "evidence", item.Kind, "/api/events/"+event.ID, item.ObservedAt)
		edges = append(edges, map[string]string{"from": eventNode, "to": evidenceNode, "relation": "contains"})
		for _, reference := range references[item.ID] {
			sampleID := reference.id
			if !evidenceUUID.MatchString(sampleID) {
				missing = append(missing, "事件中的样本标识无效")
				continue
			}
			sampleNode := "sample:" + sampleID
			if seen[sampleNode] {
				edges = append(edges, map[string]string{"from": evidenceNode,
					"to": "sample:" + sampleID, "relation": reference.relation})
				continue
			}
			sample, exists := samples[sampleID]
			if !exists {
				missing = append(missing, "样本 "+sampleID+" 已被清理或未迁移")
				continue
			}
			if sample.Metrics == nil {
				missing = append(missing, "样本 "+sampleID+" 的指标格式无效")
			}
			addNode(sampleNode, "sample", sample.Status,
				"/api/assets/"+event.AssetID+"/samples/"+sampleID, sample.At)
			edges = append(edges, map[string]string{"from": evidenceNode, "to": sampleNode, "relation": reference.relation},
				map[string]string{"from": sampleNode, "to": assetNode, "relation": "collected_from"})
			addSampleLayers(event.AssetID, sample, addNode, &edges)
		}
	}
	sqlDatabase := observedSQLDatabase(event.StartedAt, samples)
	if err := s.addSQLEvidenceGraph(ctx, event, sqlDatabase, samples, addNode, &edges); err != nil {
		return nil, err
	}
	candidates := analyzeCandidates(event, samples, addNode, &edges)
	planCandidate, err := s.sqlPlanCandidate(ctx, event, sqlDatabase, addNode, &edges)
	if err != nil {
		return nil, err
	}
	if planCandidate != nil {
		candidates = append(candidates, planCandidate)
	}
	traceCount := 0
	databaseSpans := 0
	services := []string{}
	serviceImpacts := map[string]*serviceTraceImpact{}
	traceDBSpans := []traceDBSpan{}
	observedSpans := []observedTraceSpan{}
	requestSpans := []requestSpanObservation{}
	traceReplay := []replayPoint{}
	appPodReported, appPodVerified := 0, 0
	appPodEdges := map[string]bool{}
	appPVCStatusNodes := map[string]bool{}
	appPVCStatusOmitted := false
	sampling := traceSamplingSummary{}
	clock := traceClockSummary{}
	traceRows, err := s.pool.Query(ctx, `SELECT trace_id,span_id,parent_span_id,service,kind,started_at,ended_at,status,backend_pid,query_id,route,
		app_pod_reported,app_pod_verified,received_at,sampling_probability,coalesce(endpoint_name,'') FROM trace_spans
		WHERE asset_id=$1 AND started_at >= $2::timestamptz-interval '10 minutes'
		AND started_at <= coalesce($3::timestamptz,now())+interval '5 minutes'
		ORDER BY started_at DESC LIMIT 100`, event.AssetID, event.StartedAt, event.EndedAt)
	if err != nil {
		return nil, err
	}
	serviceSet := map[string]bool{}
	spanSet := map[string]bool{}
	spanServices := map[string]string{}
	parentEdges := []map[string]string{}
	for traceRows.Next() {
		var traceID, spanID, service, kind, spanStatus, route, endpoint string
		var parentID *string
		var backendPID *int32
		var queryID *string
		var reportedPodJSON, verifiedPodJSON []byte
		var at, endedAt time.Time
		var receivedAt *time.Time
		var declaredProbability *float64
		if err := traceRows.Scan(&traceID, &spanID, &parentID, &service, &kind, &at, &endedAt,
			&spanStatus, &backendPID, &queryID, &route, &reportedPodJSON, &verifiedPodJSON,
			&receivedAt, &declaredProbability, &endpoint); err != nil {
			traceRows.Close()
			return nil, err
		}
		traceCount++
		if declaredProbability != nil {
			sampling.DeclaredSpans++
			if sampling.MinProbability == nil || *declaredProbability < *sampling.MinProbability {
				value := *declaredProbability
				sampling.MinProbability = &value
			}
			if sampling.MaxProbability == nil || *declaredProbability > *sampling.MaxProbability {
				value := *declaredProbability
				sampling.MaxProbability = &value
			}
		}
		timingAnomaly := false
		if receivedAt != nil {
			clock.TimedSpans++
			if lead := endedAt.Sub(*receivedAt); lead > time.Second {
				timingAnomaly = true
				clock.FutureTimestampSpans++
				if lead.Milliseconds() > clock.MaxApparentLeadMs {
					clock.MaxApparentLeadMs = lead.Milliseconds()
				}
			}
		}
		observed := observedTraceSpan{TraceID: traceID, SpanID: spanID, Service: service,
			Kind: kind, Status: spanStatus, Start: at, End: endedAt}
		if parentID != nil {
			observed.ParentSpanID = *parentID
		}
		if backendPID != nil {
			observed.BackendPID = int(*backendPID)
		}
		if queryID != nil {
			observed.QueryID = *queryID
		}
		requestSpans = append(requestSpans, requestSpanObservation{observedTraceSpan: observed, Route: route, Endpoint: endpoint,
			Sampling: declaredProbability, ReceivedAt: receivedAt, TimingAnomaly: timingAnomaly})
		if !timingAnomaly {
			observedSpans = append(observedSpans, observed)
		}
		if !serviceSet[service] {
			serviceSet[service] = true
			services = append(services, service)
		}
		impact := serviceImpacts[service]
		if impact == nil {
			impact = &serviceTraceImpact{Service: service, FirstAt: at, LastAt: endedAt}
			serviceImpacts[service] = impact
		}
		impact.Spans++
		if trace.IsDatabaseTargetSpan(kind) {
			impact.DatabaseSpans++
		}
		if spanStatus == "error" {
			impact.ErrorSpans++
		}
		if at.Before(impact.FirstAt) {
			impact.FirstAt = at
		}
		if endedAt.After(impact.LastAt) {
			impact.LastAt = endedAt
		}
		spanNode := "span:" + traceID + ":" + spanID
		strength, limitation := "correlated", "Trace 与事件属于同一资产及查询窗，跨层关系需再核对"
		if timingAnomaly {
			strength, limitation = "insufficient", "Span 结束时间晚于接收时间，跨层时序待核对"
		}
		traceReplay = append(traceReplay, replayPoint{TraceID: traceID, SpanID: spanID, At: at, EndAt: &endedAt,
			Layer: "app", Stage: "observation", Kind: "trace_span", Signal: kind, Source: service,
			Strength: strength, Limitation: limitation, BackendPID: observed.BackendPID})
		spanSet[spanNode] = !timingAnomaly
		spanServices[spanNode] = service
		serviceNode := "service:" + service
		addNode(serviceNode, "service", service,
			"/api/assets/"+event.AssetID+"/traces/"+traceID, at)
		spanType := "service_span"
		if trace.IsDatabaseTargetSpan(kind) {
			databaseSpans++
			spanType = "db_client_span"
		}
		if kind == "postgresql_client" {
			if parentID != nil && !timingAnomaly {
				item := traceDBSpan{
					Node: spanNode, Parent: "span:" + traceID + ":" + *parentID,
					Service: service, Start: at, End: endedAt}
				if backendPID != nil {
					item.BackendPID = int(*backendPID)
				}
				traceDBSpans = append(traceDBSpans, item)
			}
		}
		addNode(spanNode, spanType, fmt.Sprintf("%s · %d ms · %s", service,
			endedAt.Sub(at).Milliseconds(), spanStatus),
			"/api/assets/"+event.AssetID+"/traces/"+traceID, at)
		timeRelation := "time_window"
		if timingAnomaly {
			timeRelation = "time_unreliable"
		}
		edges = append(edges, map[string]string{"from": serviceNode, "to": spanNode, "relation": "emitted"},
			map[string]string{"from": eventNode, "to": spanNode, "relation": timeRelation})
		if trace.IsDatabaseTargetSpan(kind) && kind != "postgresql_client" {
			edges = append(edges, map[string]string{"from": spanNode, "to": assetNode, "relation": "observed_endpoint"})
		}
		if kind == "postgresql_client" {
			if route == "pgbouncer" {
				poolerNode := "pooler:" + event.AssetID
				addNode(poolerNode, "pgbouncer", "PgBouncer", "/api/assets/"+event.AssetID, at)
				edges = append(edges, map[string]string{"from": spanNode, "to": poolerNode, "relation": "via_pooler"},
					map[string]string{"from": poolerNode, "to": assetNode, "relation": "forwards_to"})
			} else {
				edges = append(edges, map[string]string{"from": spanNode, "to": assetNode, "relation": "calls"})
			}
			if queryID != nil && !timingAnomaly {
				if fingerprint := traceSQLFingerprint(samples, *queryID, at, endedAt); fingerprint != "" {
					edges = append(edges, map[string]string{"from": spanNode,
						"to":       "sql:" + event.AssetID + ":" + fingerprint,
						"relation": "query_id_matches"})
					if attachTraceToSQLPlanCandidate(planCandidate, *queryID, spanNode) {
						edges = append(edges, map[string]string{"from": "candidate:sql_plan_regression",
							"to": spanNode, "relation": "supported_by"})
					}
				}
			}
			if len(reportedPodJSON) != 0 {
				appPodReported++
			}
			if len(verifiedPodJSON) != 0 {
				var pod struct {
					Namespace, Name, UID, Node string
					PVCs                       []string
					PVCStates                  []struct {
						Name, Status, Phase, VolumeName string
					}
					ObservedAt time.Time
				}
				if json.Unmarshal(verifiedPodJSON, &pod) == nil && pod.Namespace != "" &&
					pod.Name != "" && pod.UID != "" {
					appPodVerified++
					podNode := "app-pod:" + pod.UID
					source := "/api/assets/" + event.AssetID + "/traces/" + traceID
					addNode(podNode, "app_pod", pod.Namespace+"/"+pod.Name, source, pod.ObservedAt)
					link := spanNode + ":" + podNode
					if !appPodEdges[link] {
						appPodEdges[link] = true
						edges = append(edges, map[string]string{"from": spanNode, "to": podNode,
							"relation": "originated_from"})
					}
					if pod.Node != "" {
						nodeID := "node:" + pod.Node
						addNode(nodeID, "k8s_node", pod.Node, source, pod.ObservedAt)
						link := podNode + ":" + nodeID
						if !appPodEdges[link] {
							appPodEdges[link] = true
							edges = append(edges, map[string]string{"from": podNode, "to": nodeID,
								"relation": "scheduled_on"})
						}
					}
					states := map[string]struct{ Status, Phase string }{}
					for _, state := range pod.PVCStates {
						states[state.Name] = struct{ Status, Phase string }{state.Status, state.Phase}
					}
					for _, name := range pod.PVCs {
						pvcID := "pvc:" + pod.Namespace + ":" + name
						addNode(pvcID, "pvc", name, source, pod.ObservedAt)
						link := podNode + ":" + pvcID
						if !appPodEdges[link] {
							appPodEdges[link] = true
							edges = append(edges, map[string]string{"from": podNode, "to": pvcID,
								"relation": "references_pvc"})
						}
						if state, ok := states[name]; ok {
							statusNode := fmt.Sprintf("app-pvc-state:%s:%s:%d", pod.UID, name, pod.ObservedAt.UnixNano())
							if !appPVCStatusNodes[statusNode] && len(appPVCStatusNodes) < 20 {
								appPVCStatusNodes[statusNode] = true
								label := name + " · 状态未核对"
								if state.Status == "ok" {
									label = name + " · " + state.Phase
								}
								label += " · 观测于 " + pod.ObservedAt.UTC().Format(time.RFC3339)
								addNode(statusNode, "pvc_status", label, source, pod.ObservedAt)
								edges = append(edges, map[string]string{"from": statusNode, "to": pvcID,
									"relation": "observed_pvc_status"})
							} else if !appPVCStatusNodes[statusNode] {
								appPVCStatusOmitted = true
							}
						}
					}
				}
			}
		}
		if parentID != nil {
			parentEdges = append(parentEdges, map[string]string{"from": "span:" + traceID + ":" + *parentID,
				"to": spanNode, "relation": "parent_of"})
		}
	}
	if err := traceRows.Err(); err != nil {
		traceRows.Close()
		return nil, err
	}
	traceRows.Close()
	requestPaths, requestPathsAtLimit := traceRequestPaths(requestSpans, samples)
	classifyTraceImpact(observedSpans, samples, serviceImpacts, sqlRegressionForEvent(event))
	unresolvedParentSpans := addTraceParentAndServiceEdges(parentEdges, spanServices, &edges)
	if candidate := traceLockCandidate(event, samples, traceDBSpans, spanSet, addNode, &edges); candidate != nil {
		candidates = append(candidates, candidate)
	}
	if traceCount == 0 {
		missing = append(missing, "应用 Trace 未接入或当前时间窗无可关联 Span")
	}
	if unresolvedParentSpans > 0 {
		missing = append(missing, fmt.Sprintf("%d 条 Span 的父 Span 未在当前事件窗口中找到", unresolvedParentSpans))
	}
	if appPodReported > appPodVerified {
		missing = append(missing, fmt.Sprintf("%d 条应用 Pod 身份仅由 OTel 上报，未通过 Kubernetes API 核对",
			appPodReported-appPodVerified))
	}
	if appPVCStatusOmitted {
		missing = append(missing, "应用 PVC 状态快照超过事件图 20 个节点上限；原始 Trace 仍可查看完整快照")
	}
	if clock.FutureTimestampSpans > 0 {
		missing = append(missing, fmt.Sprintf("%d 条 Span 的结束时间晚于 DbSphere 接收时间超过 1 秒；其跨层时间关联未用于根因或影响面判断",
			clock.FutureTimestampSpans))
	}
	sort.Strings(services)
	impactList := make([]serviceTraceImpact, 0, len(services))
	for _, service := range services {
		impactList = append(impactList, *serviceImpacts[service])
	}
	historical, err := s.rcaPoliciesAt(ctx, &event.StartedAt)
	if err != nil {
		return nil, err
	}
	policies := policiesByCode(historical)
	if replayCode != "" {
		current, err := s.rcaPoliciesAt(ctx, nil)
		if err != nil {
			return nil, err
		}
		var matched map[string]any
		for _, candidate := range candidates {
			if candidate["code"] == replayCode {
				matched = candidate
				break
			}
		}
		return map[string]any{"eventId": event.ID, "assetId": event.AssetID, "eventStatus": event.Status, "eventStartedAt": event.StartedAt,
			"ruleCode": replayCode, "title": rcaRuleTitles[replayCode],
			"historical": evaluateRCACandidate(matched, policies[replayCode], event.StartedAt),
			"current":    evaluateRCACandidate(matched, policiesByCode(current)[replayCode], event.StartedAt)}, nil
	}
	candidates, hidden := selectRCACandidates(candidates, policies, event.StartedAt)
	if len(hidden) != 0 {
		nodes = slices.DeleteFunc(nodes, func(node map[string]any) bool { return hidden[stringValue(node["id"])] })
		edges = slices.DeleteFunc(edges, func(edge map[string]string) bool { return hidden[edge["from"]] || hidden[edge["to"]] })
	}
	status := "unlocated"
	if len(candidates) > 0 {
		status = "candidates"
	}
	dependencies, err := s.incidentDependencies(ctx, event, observedSpans, samples)
	if err != nil {
		return nil, err
	}
	analysis := map[string]any{
		"status": status, "graph": map[string]any{"nodes": nodes, "edges": edges},
		"timeline": event.Evidence, "candidates": candidates, "missingSignals": missing,
		"ruleSuppressedCount": len(hidden), "dependencies": dependencies,
		"traceCoverage": map[string]any{"spans": traceCount, "databaseSpans": databaseSpans,
			"unresolvedParentSpans": unresolvedParentSpans,
			"services":              services, "serviceImpact": impactList, "atLimit": traceCount == 100,
			"appPodReported": appPodReported, "appPodVerified": appPodVerified,
			"sampling": sampling, "clock": clock, "requestPaths": requestPaths, "requestPathsAtLimit": requestPathsAtLimit},
	}
	replay := incidentReplay(event, samples, candidates)
	if err := s.enrichIncidentReplay(ctx, event, replay, samples, traceReplay, edges); err != nil {
		return nil, err
	}
	investigation := map[string]any{"database": sqlDatabase,
		"locks":            investigationLockEvidence(replay["points"].([]replayPoint), samples),
		"connectionStages": connectionStages(requestSpans, samples, traceCount == 100),
		"collectionGaps":   incidentCollectionGaps(samples)}
	return map[string]any{"event": event, "analysis": analysis, "relatedEvents": related, "replay": replay,
		"waitCaptures": waitCaptures, "investigation": investigation}, nil
}

func (s *Postgres) AcknowledgeEvent(ctx context.Context, id string) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var status string
	var acknowledged *time.Time
	err = tx.QueryRow(ctx, "SELECT status,acknowledged_at FROM events WHERE id=$1 FOR UPDATE", id).Scan(&status, &acknowledged)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if status == "open" && acknowledged == nil {
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, "UPDATE events SET acknowledged_at=$1 WHERE id=$2", now, id); err != nil {
			return nil, err
		}
		payload, _ := json.Marshal(map[string]string{"actor": accessActor(ctx, "admin")})
		if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'event_acknowledged',$2,$3)`, id, now, payload); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	event, err := s.loadEvent(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"event": event}, nil
}

func (s *Postgres) RecordRCADecision(ctx context.Context, id, code, decision, reason string) error {
	reason = strings.TrimSpace(reason)
	if !rcaCode.MatchString(code) || (decision != "confirmed" && decision != "rejected") ||
		len([]rune(reason)) < 5 || len([]rune(reason)) > 500 {
		return ErrInvalidRCADecision
	}
	detail, err := s.EventByID(ctx, id)
	if err != nil {
		return err
	}
	if detail == nil {
		return ErrEventNotFound
	}
	analysis := object(detail.(map[string]any)["analysis"])
	var supports []string
	for _, candidate := range analysis["candidates"].([]map[string]any) {
		if candidate["code"] == code {
			supports = candidate["supports"].([]string)
			break
		}
	}
	if supports == nil {
		return ErrInvalidRCADecision
	}
	payload, err := json.Marshal(map[string]any{
		"code": code, "decision": decision, "reason": reason,
		"supports": supports, "actor": accessActor(ctx, "admin")})
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'rca_decision',now(),$2)`, id, payload)
	return err
}

func (s *Postgres) RawSample(ctx context.Context, assetID, sampleID string) (any, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object('id',id,'assetId',asset_id,
		'collectedAt',collected_at,'status',status,'metrics',metrics_json,'error',error)
		FROM samples WHERE asset_id=$1 AND id=$2 ORDER BY collected_at DESC LIMIT 1`,
		assetID, sampleID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"sample": json.RawMessage(raw)}, nil
}

func (s *Postgres) RawTrace(ctx context.Context, assetID, traceID string) (any, error) {
	spans, err := s.queryJSON(ctx, `SELECT jsonb_build_object(
		'assetId',asset_id,'traceId',trace_id,'spanId',span_id,'parentSpanId',parent_span_id,
		'service',service,'kind',kind,'start',started_at,'end',ended_at,'status',status,'backendPid',backend_pid,'queryId',query_id,'route',route,
		'appPodReported',app_pod_reported,'appPodVerified',app_pod_verified,'endpoint',endpoint_name,
		'receivedAt',received_at,'declaredSamplingProbability',sampling_probability)
		FROM trace_spans WHERE asset_id=$1 AND trace_id=$2 ORDER BY started_at LIMIT 256`, assetID, traceID)
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, nil
	}
	return map[string]any{"spans": spans}, nil
}
