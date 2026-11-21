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

func sqlResetAt(value any) string {
	switch reset := value.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, reset); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
		return reset
	case time.Time:
		if !reset.IsZero() {
			return reset.UTC().Format(time.RFC3339Nano)
		}
	case *time.Time:
		if reset != nil && !reset.IsZero() {
			return reset.UTC().Format(time.RFC3339Nano)
		}
	}
	return ""
}

func sqlStatementRows(value any) []map[string]any {
	if rows, ok := value.([]map[string]any); ok {
		return rows
	}
	result := []map[string]any{}
	for _, raw := range list(value) {
		if row := object(raw); row != nil {
			result = append(result, row)
		}
	}
	return result
}

func sqlDelta(newer, older map[string]any) map[string]map[string]any {
	return sqlIntervalDelta(newer, older, false)
}

func sqlIntervalDelta(newer, older map[string]any, includeIdle bool) map[string]map[string]any {
	if sqlResetAt(newer["resetAt"]) == "" ||
		sqlResetAt(newer["resetAt"]) != sqlResetAt(older["resetAt"]) {
		return nil
	}
	prior := map[string]map[string]any{}
	for _, row := range sqlStatementRows(older["statements"]) {
		prior[stringValue(row["fingerprint"])] = row
	}
	result := map[string]map[string]any{}
	for _, row := range sqlStatementRows(newer["statements"]) {
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
		calls := number(change["calls"])
		if valid && (includeIdle || calls > 0) {
			if calls > 0 {
				change["averageExecMs"] = number(change["totalExecMs"]) / calls
			}
			result[fingerprint] = change
		}
	}
	return result
}

// sqlRegressionSignal compares two complete intervals of the same fingerprint.
// A missing interval is not a healthy signal: it must not close a firing alert.
func sqlRegressionSignal(current, previous, older map[string]any) map[string]any {
	for _, snapshot := range []map[string]any{current, previous, older} {
		if stringValue(snapshot["status"]) != "ok" {
			return nil
		}
	}
	interval := sqlDelta(current, previous)
	baseline := sqlDelta(previous, older)
	if interval == nil || baseline == nil {
		return nil
	}
	eligible := 0
	bestRatio := 0.0
	var best map[string]any
	for _, row := range sqlStatementRows(current["statements"]) {
		fingerprint := stringValue(row["fingerprint"])
		latest, prior := interval[fingerprint], baseline[fingerprint]
		if latest == nil || prior == nil || number(latest["calls"]) < 5 || number(prior["calls"]) < 5 {
			continue
		}
		previousMs, currentMs := number(prior["averageExecMs"]), number(latest["averageExecMs"])
		if previousMs <= 0 {
			continue
		}
		eligible++
		ratio := currentMs / previousMs
		if ratio >= 2 && currentMs-previousMs >= 50 && ratio > bestRatio {
			bestRatio = ratio
			best = map[string]any{"queryId": row["queryId"], "fingerprint": fingerprint,
				"previousAverageExecMs": previousMs, "currentAverageExecMs": currentMs,
				"previousCalls": prior["calls"], "currentCalls": latest["calls"]}
		}
	}
	if eligible == 0 {
		return nil
	}
	return map[string]any{"status": "ok", "source": "pg_stat_statements",
		"eligibleStatements": eligible, "maxRatio": bestRatio, "worst": best}
}

func (s *Postgres) SQLSummary(ctx context.Context, assetID string) (any, error) {
	return s.sqlSummary(ctx, assetID, nil, nil, "")
}

func (s *Postgres) sqlSummary(ctx context.Context, assetID string, from, to *time.Time, queryID string) (any, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text,collected_at,status,metrics_json
		FROM samples WHERE asset_id=$1 AND ($2::timestamptz IS NULL OR collected_at>=$2)
		AND ($3::timestamptz IS NULL OR collected_at<$3) ORDER BY collected_at DESC,id DESC LIMIT 12`, assetID, from, to)
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
		pgAvailable := status == "ok" || (from != nil && stringValue(object(metrics["collector"])["pgStatus"]) == "ok")
		if pgAvailable && stringValue(performance["status"]) == "ok" {
			snapshots = append(snapshots, sqlSnapshot{ID: id, At: at, Data: performance, Metrics: metrics})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	eventIDs, err := s.queryJSON(ctx, `SELECT to_jsonb(id::text) FROM events WHERE asset_id=$1 AND
		(($2::timestamptz IS NULL AND status='open') OR ($2 IS NOT NULL AND started_at<$3::timestamptz
		AND (ended_at IS NULL OR ended_at>=$2))) ORDER BY started_at DESC LIMIT 100`, assetID, from, to)
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
		if queryID != "" && stringValue(original["queryId"]) != queryID {
			continue
		}
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
