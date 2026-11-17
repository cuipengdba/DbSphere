package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var evidenceUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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
	rows, err := s.pool.Query(ctx, `SELECT id::text,kind,observed_at,payload_json
		FROM evidence WHERE event_id=$1 ORDER BY observed_at,id LIMIT 500`, id)
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
		var sampleAt time.Time
		var sampleStatus string
		err := s.pool.QueryRow(ctx, `SELECT collected_at,status FROM samples
			WHERE asset_id=$1 AND id=$2 ORDER BY collected_at DESC LIMIT 1`, event.AssetID, sampleID).Scan(&sampleAt, &sampleStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = append(missing, "样本 "+sampleID+" 已被清理或未迁移")
			continue
		}
		if err != nil {
			return nil, err
		}
		sampleNode := "sample:" + sampleID
		addNode(sampleNode, "sample", sampleStatus,
			"/api/assets/"+event.AssetID+"/samples/"+sampleID, sampleAt)
		edges = append(edges, map[string]string{"from": evidenceNode, "to": sampleNode, "relation": "observed_in"},
			map[string]string{"from": sampleNode, "to": assetNode, "relation": "collected_from"})
	}
	traceCount := 0
	services := []string{}
	traceRows, err := s.pool.Query(ctx, `SELECT trace_id,span_id,service,started_at FROM trace_spans
		WHERE asset_id=$1 AND started_at >= $2::timestamptz-interval '10 minutes'
		AND started_at <= coalesce($3::timestamptz,now())+interval '5 minutes'
		ORDER BY started_at DESC LIMIT 100`, event.AssetID, event.StartedAt, event.EndedAt)
	if err != nil {
		return nil, err
	}
	serviceSet := map[string]bool{}
	for traceRows.Next() {
		var traceID, spanID, service string
		var at time.Time
		if err := traceRows.Scan(&traceID, &spanID, &service, &at); err != nil {
			traceRows.Close()
			return nil, err
		}
		traceCount++
		if !serviceSet[service] {
			serviceSet[service] = true
			services = append(services, service)
		}
		spanNode := "span:" + traceID + ":" + spanID
		addNode(spanNode, "db_client_span", service,
			"/api/assets/"+event.AssetID+"/traces/"+traceID, at)
		edges = append(edges, map[string]string{"from": eventNode, "to": spanNode, "relation": "time_window"},
			map[string]string{"from": spanNode, "to": assetNode, "relation": "calls"})
	}
	if err := traceRows.Err(); err != nil {
		traceRows.Close()
		return nil, err
	}
	traceRows.Close()
	if traceCount == 0 {
		missing = append(missing, "应用 Trace 未接入或当前时间窗无可关联 Span")
	}
	missing = append(missing, "Go 深度根因规则与跨层证据映射仍在迁移")
	analysis := map[string]any{
		"status": "pending", "graph": map[string]any{"nodes": nodes, "edges": edges},
		"timeline": event.Evidence, "candidates": []any{}, "missingSignals": missing,
		"traceCoverage": map[string]any{"spans": traceCount, "databaseSpans": traceCount, "services": services},
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
