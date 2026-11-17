package store

import (
	"context"
	"encoding/json"
	"sort"
	"time"
)

var sqlCounterFields = []string{"calls", "totalExecMs", "rows", "sharedBlocksHit", "sharedBlocksRead",
	"sharedBlocksWritten", "tempBlocksRead", "tempBlocksWritten", "walBytes"}

type sqlSnapshot struct {
	ID      string
	At      time.Time
	Data    map[string]any
	Metrics map[string]any
}

func sqlDelta(newer, older map[string]any) map[string]map[string]any {
	if stringValue(newer["resetAt"]) == "" ||
		stringValue(newer["resetAt"]) != stringValue(older["resetAt"]) {
		return nil
	}
	prior := map[string]map[string]any{}
	for _, raw := range list(older["statements"]) {
		row := object(raw)
		prior[stringValue(row["fingerprint"])] = row
	}
	result := map[string]map[string]any{}
	for _, raw := range list(newer["statements"]) {
		row := object(raw)
		fingerprint := stringValue(row["fingerprint"])
		before := prior[fingerprint]
		if before == nil {
			continue
		}
		change := map[string]any{"fingerprint": fingerprint}
		valid := true
		for _, field := range sqlCounterFields {
			afterNumber, afterOK := row[field].(float64)
			beforeNumber, beforeOK := before[field].(float64)
			if !afterOK || !beforeOK || afterNumber < beforeNumber {
				valid = false
				break
			}
			change[field] = afterNumber - beforeNumber
		}
		if valid && number(change["calls"]) > 0 {
			change["averageExecMs"] = number(change["totalExecMs"]) / number(change["calls"])
			result[fingerprint] = change
		}
	}
	return result
}

func (s *Postgres) SQLSummary(ctx context.Context, assetID string) (any, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text,collected_at,status,metrics_json
		FROM samples WHERE asset_id=$1 ORDER BY collected_at DESC LIMIT 12`, assetID)
	if err != nil {
		return nil, err
	}
	snapshots := []sqlSnapshot{}
	var latest map[string]any
	first := true
	for rows.Next() {
		var id, status string
		var at time.Time
		var raw []byte
		if err := rows.Scan(&id, &at, &status, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var metrics map[string]any
		if json.Unmarshal(raw, &metrics) != nil {
			continue
		}
		performance := object(metrics["sqlPerformance"])
		if first {
			latest = performance
			first = false
		}
		if status == "ok" && stringValue(performance["status"]) == "ok" {
			snapshots = append(snapshots, sqlSnapshot{ID: id, At: at, Data: performance, Metrics: metrics})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	eventIDs, err := s.queryJSON(ctx, `SELECT to_jsonb(id::text) FROM events
		WHERE asset_id=$1 AND status='open' ORDER BY started_at DESC LIMIT 100`, assetID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, raw := range eventIDs {
		var id string
		if json.Unmarshal(raw, &id) == nil {
			ids = append(ids, id)
		}
	}
	if stringValue(latest["status"]) != "ok" || len(snapshots) == 0 {
		status := stringValue(latest["status"])
		if status == "" {
			status = "unknown"
		}
		reason := stringValue(latest["reason"])
		if reason == "" {
			reason = "No SQL performance sample"
		}
		return map[string]any{"performance": map[string]any{
			"status": status, "reason": reason, "statements": []any{}, "eventIds": ids}}, nil
	}
	current := snapshots[0]
	var interval, baseline map[string]map[string]any
	if len(snapshots) >= 2 {
		interval = sqlDelta(current.Data, snapshots[1].Data)
	}
	if len(snapshots) >= 3 {
		baseline = sqlDelta(snapshots[1].Data, snapshots[2].Data)
	}
	statements := []map[string]any{}
	for _, raw := range list(current.Data["statements"]) {
		original := object(raw)
		row := make(map[string]any, len(original)+3)
		for key, value := range original {
			row[key] = value
		}
		fingerprint := stringValue(row["fingerprint"])
		row["delta"] = interval[fingerprint]
		row["trend"] = []any{}
		row["regression"] = nil
		if delta, previous := interval[fingerprint], baseline[fingerprint]; delta != nil && previous != nil &&
			number(delta["calls"]) >= 5 && number(previous["calls"]) >= 5 &&
			number(delta["averageExecMs"]) >= 2*number(previous["averageExecMs"]) &&
			number(delta["averageExecMs"])-number(previous["averageExecMs"]) >= 50 &&
			number(previous["averageExecMs"]) > 0 {
			row["regression"] = map[string]any{
				"previousAverageExecMs": previous["averageExecMs"],
				"currentAverageExecMs":  delta["averageExecMs"],
				"ratio":                 number(delta["averageExecMs"]) / number(previous["averageExecMs"])}
		}
		statements = append(statements, row)
	}
	sort.Slice(statements, func(i, j int) bool {
		score := func(row map[string]any) float64 {
			if delta := object(row["delta"]); delta != nil {
				return number(delta["totalExecMs"])
			}
			return number(row["totalExecMs"])
		}
		return score(statements[i]) > score(statements[j])
	})
	if len(statements) > 20 {
		statements = statements[:20]
	}
	comparison := "cumulative"
	if interval != nil {
		comparison = "interval"
	}
	return map[string]any{"performance": map[string]any{
		"status": "ok", "source": current.Data["source"], "collectedAt": current.At,
		"sampleId": current.ID, "resetAt": current.Data["resetAt"],
		"comparison": comparison, "truncated": current.Data["truncated"],
		"hiddenQueryCount": current.Data["hiddenQueryCount"], "statements": statements,
		"eventIds": ids}}, nil
}
