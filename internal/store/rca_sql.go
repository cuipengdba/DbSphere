package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// sqlPlanCandidate requires a pre-incident baseline, a contemporaneous plan,
// and a slow log for the same exact Query ID. It remains a hypothesis.
func (s *Postgres) sqlPlanCandidate(ctx context.Context, event *eventRecord,
	add graphAdder, edges *[]map[string]string) (map[string]any, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.id::text,p.observed_at,p.query_id,p.shape_hash,p.summary_json,
		prior.id::text,prior.observed_at,prior.shape_hash,prior.summary_json,
		log.id::text,log.observed_at,log.duration_ms
		FROM sql_plan_snapshots p
		JOIN LATERAL (SELECT id,observed_at,shape_hash,summary_json FROM sql_plan_snapshots
			WHERE asset_id=p.asset_id AND query_id=p.query_id
			AND observed_at < $2::timestamptz-interval '5 minutes'
			AND observed_at >= $2::timestamptz-interval '30 days'
			ORDER BY observed_at DESC LIMIT 1) prior ON true
		JOIN LATERAL (SELECT id,observed_at,duration_ms FROM slow_log_entries
			WHERE asset_id=p.asset_id AND query_id=p.query_id AND duration_ms>=1000
			AND observed_at >= $2 AND observed_at <= coalesce($3::timestamptz,now())+interval '5 minutes'
			AND observed_at BETWEEN p.observed_at-interval '5 minutes' AND p.observed_at+interval '5 minutes'
			ORDER BY observed_at DESC LIMIT 1) log ON true
		WHERE p.asset_id=$1 AND p.observed_at >= $2
		AND p.observed_at <= coalesce($3::timestamptz,now())+interval '5 minutes'
		ORDER BY p.observed_at DESC LIMIT 20`, event.AssetID, event.StartedAt, event.EndedAt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var currentID, queryID, currentShape, priorID, priorShape, logID string
		var currentAt, priorAt, logAt time.Time
		var currentRaw, priorRaw []byte
		var slowDuration float64
		if err := rows.Scan(&currentID, &currentAt, &queryID, &currentShape, &currentRaw,
			&priorID, &priorAt, &priorShape, &priorRaw, &logID, &logAt, &slowDuration); err != nil {
			return nil, err
		}
		var current, prior map[string]any
		if json.Unmarshal(currentRaw, &current) != nil || json.Unmarshal(priorRaw, &prior) != nil {
			continue
		}
		currentMs, priorMs := number(current["executionMs"]), number(prior["executionMs"])
		ratio := 0.0
		if currentMs > 0 && priorMs > 0 {
			ratio = currentMs / priorMs
		}
		shapeChanged := currentShape != priorShape
		if !shapeChanged && ratio < 2 {
			continue
		}
		priorNode, currentNode, logNode := "plan:"+priorID, "plan:"+currentID, "slow_log:"+logID
		add(priorNode, "plan", "Query ID "+queryID+" · 事前基线",
			"/api/assets/"+event.AssetID+"/sql-plans/"+priorID, priorAt)
		add(currentNode, "plan", "Query ID "+queryID,
			"/api/assets/"+event.AssetID+"/sql-plans/"+currentID, currentAt)
		add(logNode, "slow_log", fmt.Sprintf("%.1f ms", slowDuration),
			"/api/assets/"+event.AssetID+"/slow-logs/"+logID, logAt)
		*edges = append(*edges, map[string]string{"from": currentNode, "to": priorNode, "relation": "compared_with"})
		title := "SQL 计划变化与慢日志同期出现"
		if ratio >= 2 {
			title = "执行计划耗时退化与慢日志同期出现"
		}
		code := "sql_plan_regression"
		candidateNode := "candidate:" + code
		add(candidateNode, "candidate", title, "/api/events/"+event.ID, currentAt)
		supports := []string{priorNode, currentNode, logNode}
		*edges = append(*edges, map[string]string{"from": "event:" + event.ID, "to": candidateNode, "relation": "hypothesizes"})
		for _, support := range supports {
			*edges = append(*edges, map[string]string{"from": candidateNode, "to": support, "relation": "supported_by"})
		}
		var review any
		for _, evidence := range event.Evidence {
			if evidence.Kind != "rca_decision" {
				continue
			}
			var payload map[string]any
			if json.Unmarshal(evidence.Payload, &payload) == nil && payload["code"] == code {
				review = map[string]any{"decision": payload["decision"], "actor": payload["actor"],
					"reason": payload["reason"], "at": evidence.ObservedAt, "evidenceId": evidence.ID}
			}
		}
		if reviewed := object(review); reviewed != nil {
			*edges = append(*edges, map[string]string{"from": candidateNode,
				"to": "evidence:" + stringValue(reviewed["evidenceId"]), "relation": "reviewed_by"})
		}
		return map[string]any{"code": code, "title": title, "confidence": "low", "supports": supports,
			"against": []string{}, "missing": []string{"应用请求与 Query ID 的直接链路需核对", "Query ID 复用或统计重置未排除"},
			"limitation": "同 Query ID 的计划变化、执行时间与慢日志同期出现，仅能作为排查线索，不能证明唯一根因。",
			"detail":     map[string]any{"queryId": queryID, "shapeChanged": shapeChanged, "executionRatio": ratio, "slowDurationMs": slowDuration},
			"state":      "hypothesis", "matchedConditions": []any{}, "review": review,
			"timeOrder": map[string]any{"firstObservedAt": currentAt, "eventStartedAt": event.StartedAt, "relation": "same_window"}}, nil
	}
	return nil, rows.Err()
}

func (s *Postgres) addSQLEvidenceGraph(ctx context.Context, event *eventRecord,
	samples map[string]sampleObservation, add graphAdder, edges *[]map[string]string) error {
	sqlNodes := map[string][]string{}
	for _, sample := range samples {
		performance := object(sample.Metrics["sqlPerformance"])
		if sample.Status != "ok" || stringValue(performance["status"]) != "ok" {
			continue
		}
		for _, item := range list(performance["statements"]) {
			statement := object(item)
			queryID := fmt.Sprint(statement["queryId"])
			fingerprint := stringValue(statement["fingerprint"])
			if queryID != "<nil>" && fingerprint != "" {
				sqlNodes[queryID] = append(sqlNodes[queryID], "sql:"+event.AssetID+":"+fingerprint)
			}
		}
	}
	logs, err := s.pool.Query(ctx, `SELECT id::text,observed_at,duration_ms,query_id
		FROM slow_log_entries WHERE asset_id=$1
		AND observed_at >= $2::timestamptz-interval '10 minutes'
		AND observed_at <= coalesce($3::timestamptz,now())+interval '5 minutes'
		ORDER BY observed_at DESC LIMIT 50`, event.AssetID, event.StartedAt, event.EndedAt)
	if err != nil {
		return err
	}
	for logs.Next() {
		var id string
		var at time.Time
		var duration float64
		var queryID *string
		if err := logs.Scan(&id, &at, &duration, &queryID); err != nil {
			logs.Close()
			return err
		}
		node := "slow_log:" + id
		add(node, "slow_log", fmt.Sprintf("%.1f ms", duration),
			"/api/assets/"+event.AssetID+"/slow-logs/"+id, at)
		*edges = append(*edges, map[string]string{"from": "event:" + event.ID,
			"to": node, "relation": "time_window"})
		if queryID != nil {
			for _, sqlNode := range sqlNodes[*queryID] {
				*edges = append(*edges, map[string]string{"from": node,
					"to": sqlNode, "relation": "query_id_matches"})
			}
		}
	}
	if err := logs.Err(); err != nil {
		logs.Close()
		return err
	}
	logs.Close()
	plans, err := s.pool.Query(ctx, `SELECT id::text,observed_at,query_id,summary_json
		FROM sql_plan_snapshots WHERE asset_id=$1
		AND observed_at >= $2::timestamptz-interval '10 minutes'
		AND observed_at <= coalesce($3::timestamptz,now())+interval '5 minutes'
		ORDER BY observed_at DESC LIMIT 50`, event.AssetID, event.StartedAt, event.EndedAt)
	if err != nil {
		return err
	}
	for plans.Next() {
		var id, queryID string
		var at time.Time
		var summary []byte
		if err := plans.Scan(&id, &at, &queryID, &summary); err != nil {
			plans.Close()
			return err
		}
		node := "plan:" + id
		add(node, "plan", "Query ID "+queryID,
			"/api/assets/"+event.AssetID+"/sql-plans/"+id, at)
		*edges = append(*edges, map[string]string{"from": "event:" + event.ID,
			"to": node, "relation": "time_window"})
		for _, sqlNode := range sqlNodes[queryID] {
			*edges = append(*edges, map[string]string{"from": node,
				"to": sqlNode, "relation": "query_id_matches"})
		}
	}
	if err := plans.Err(); err != nil {
		plans.Close()
		return err
	}
	plans.Close()
	return nil
}
