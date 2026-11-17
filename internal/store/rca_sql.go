package store

import (
	"context"
	"fmt"
	"time"
)

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
