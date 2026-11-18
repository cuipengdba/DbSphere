package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

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
	Kind           string          `json:"kind"`
	EventGroup     *string         `json:"eventGroup"`
	Title          string          `json:"title"`
	Status         string          `json:"status"`
	StartedAt      time.Time       `json:"startedAt"`
	EndedAt        *time.Time      `json:"endedAt"`
	AcknowledgedAt *time.Time      `json:"acknowledgedAt"`
	Evidence       []eventEvidence `json:"evidence"`
}

func (s *Postgres) loadEvent(ctx context.Context, id string) (*eventRecord, error) {
	var event eventRecord
	err := s.pool.QueryRow(ctx, `SELECT e.id::text,e.asset_id::text,a.name,e.kind,e.event_group,
		e.title,e.status,e.started_at,e.ended_at,e.acknowledged_at
		FROM events e JOIN assets a ON a.id=e.asset_id WHERE e.id=$1`, id).Scan(
		&event.ID, &event.AssetID, &event.AssetName, &event.Kind, &event.EventGroup,
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
	event, err := s.loadEvent(ctx, id)
	if err != nil || event == nil {
		return nil, err
	}
	eventNode := "event:" + event.ID
	assetNode := "asset:" + event.AssetID
	nodes := []map[string]any{
		{"id": eventNode, "type": "event", "label": event.Title, "at": event.StartedAt, "source": "/api/events/" + event.ID},
		{"id": assetNode, "type": "postgres", "label": event.AssetName, "source": "/api/assets/" + event.AssetID},
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
	missing := []string{}
	samples := map[string]sampleObservation{}
	for _, item := range event.Evidence {
		evidenceNode := "evidence:" + item.ID
		addNode(evidenceNode, "evidence", item.Kind, "/api/events/"+event.ID, item.ObservedAt)
		edges = append(edges, map[string]string{"from": eventNode, "to": evidenceNode, "relation": "contains"})
		var payload map[string]any
		if json.Unmarshal(item.Payload, &payload) != nil {
			continue
		}
		sampleID, _ := payload["sampleId"].(string)
		if sampleID == "" {
			continue
		}
		if !evidenceUUID.MatchString(sampleID) {
			missing = append(missing, "事件中的样本标识无效")
			continue
		}
		if _, exists := samples[sampleID]; exists {
			edges = append(edges, map[string]string{"from": evidenceNode,
				"to": "sample:" + sampleID, "relation": "observed_in"})
			continue
		}
		var sampleAt time.Time
		var sampleStatus string
		var metricsJSON []byte
		err := s.pool.QueryRow(ctx, `SELECT collected_at,status,metrics_json FROM samples
			WHERE asset_id=$1 AND id=$2 ORDER BY collected_at DESC LIMIT 1`, event.AssetID, sampleID).Scan(&sampleAt, &sampleStatus, &metricsJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = append(missing, "样本 "+sampleID+" 已被清理或未迁移")
			continue
		}
		if err != nil {
			return nil, err
		}
		sampleNode := "sample:" + sampleID
		var metrics map[string]any
		if err := json.Unmarshal(metricsJSON, &metrics); err != nil {
			missing = append(missing, "样本 "+sampleID+" 的指标格式无效")
		}
		samples[sampleID] = sampleObservation{ID: sampleID, At: sampleAt, Status: sampleStatus, Metrics: metrics}
		addNode(sampleNode, "sample", sampleStatus,
			"/api/assets/"+event.AssetID+"/samples/"+sampleID, sampleAt)
		edges = append(edges, map[string]string{"from": evidenceNode, "to": sampleNode, "relation": "observed_in"},
			map[string]string{"from": sampleNode, "to": assetNode, "relation": "collected_from"})
		addSampleLayers(event.AssetID, samples[sampleID], addNode, &edges)
	}
	if err := s.addSQLEvidenceGraph(ctx, event, samples, addNode, &edges); err != nil {
		return nil, err
	}
	candidates := analyzeCandidates(event, samples, addNode, &edges)
	planCandidate, err := s.sqlPlanCandidate(ctx, event, addNode, &edges)
	if err != nil {
		return nil, err
	}
	if planCandidate != nil {
		candidates = append(candidates, planCandidate)
	}
	traceCount := 0
	databaseSpans := 0
	services := []string{}
	traceDBSpans := []traceDBSpan{}
	traceRows, err := s.pool.Query(ctx, `SELECT trace_id,span_id,parent_span_id,service,kind,started_at,ended_at,status,backend_pid FROM trace_spans
		WHERE asset_id=$1 AND started_at >= $2::timestamptz-interval '10 minutes'
		AND started_at <= coalesce($3::timestamptz,now())+interval '5 minutes'
		ORDER BY started_at DESC LIMIT 100`, event.AssetID, event.StartedAt, event.EndedAt)
	if err != nil {
		return nil, err
	}
	serviceSet := map[string]bool{}
	spanSet := map[string]bool{}
	parentEdges := []map[string]string{}
	for traceRows.Next() {
		var traceID, spanID, service, kind, spanStatus string
		var parentID *string
		var backendPID *int32
		var at, endedAt time.Time
		if err := traceRows.Scan(&traceID, &spanID, &parentID, &service, &kind, &at, &endedAt, &spanStatus, &backendPID); err != nil {
			traceRows.Close()
			return nil, err
		}
		traceCount++
		if !serviceSet[service] {
			serviceSet[service] = true
			services = append(services, service)
		}
		spanNode := "span:" + traceID + ":" + spanID
		spanSet[spanNode] = true
		serviceNode := "service:" + service
		addNode(serviceNode, "service", service,
			"/api/assets/"+event.AssetID+"/traces/"+traceID, at)
		spanType := "service_span"
		if kind == "postgresql_client" {
			databaseSpans++
			spanType = "db_client_span"
			if parentID != nil {
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
		edges = append(edges, map[string]string{"from": serviceNode, "to": spanNode, "relation": "emitted"},
			map[string]string{"from": eventNode, "to": spanNode, "relation": "time_window"})
		if kind == "postgresql_client" {
			edges = append(edges, map[string]string{"from": spanNode, "to": assetNode, "relation": "calls"})
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
	for _, edge := range parentEdges {
		if spanSet[edge["from"]] {
			edges = append(edges, edge)
		}
	}
	if candidate := traceLockCandidate(event, samples, traceDBSpans, spanSet, addNode, &edges); candidate != nil {
		candidates = append(candidates, candidate)
	}
	if traceCount == 0 {
		missing = append(missing, "应用 Trace 未接入或当前时间窗无可关联 Span")
	}
	missing = append(missing, "Go 全量根因规则和自动跨层采集仍在迁移")
	status := "unlocated"
	if len(candidates) > 0 {
		status = "candidates"
	}
	analysis := map[string]any{
		"status": status, "graph": map[string]any{"nodes": nodes, "edges": edges},
		"timeline": event.Evidence, "candidates": candidates, "missingSignals": missing,
		"traceCoverage": map[string]any{"spans": traceCount, "databaseSpans": databaseSpans, "services": services},
	}
	return map[string]any{"event": event, "analysis": analysis}, nil
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
		payload, _ := json.Marshal(map[string]string{"actor": "admin"})
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
		"supports": supports, "actor": "admin"})
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
		'service',service,'kind',kind,'start',started_at,'end',ended_at,'status',status,'backendPid',backend_pid)
		FROM trace_spans WHERE asset_id=$1 AND trace_id=$2 ORDER BY started_at LIMIT 256`, assetID, traceID)
	if err != nil {
		return nil, err
	}
	if len(spans) == 0 {
		return nil, nil
	}
	return map[string]any{"spans": spans}, nil
}
