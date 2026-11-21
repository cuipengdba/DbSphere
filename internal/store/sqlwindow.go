package store

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"time"
)

const maxSQLWindowSamples = 720

var sqlWindowOrder = map[string]string{
	"calls": "callsPerSecond", "time": "totalExecMs", "latency": "averageExecMs",
	"reads": "sharedBlocksRead", "temp": "tempBlocksWritten",
}

func (s *Postgres) sqlWindow(ctx context.Context, assetID string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	if err != nil || params.Get("to") == "" {
		return nil, ErrInvalidRange
	}
	queryID := params.Get("queryId")
	if queryID != "" {
		if _, err := strconv.ParseInt(queryID, 10, 64); err != nil {
			return nil, ErrInvalidRange
		}
	}
	metric := sqlWindowOrder[params.Get("order")]
	if params.Get("order") == "" {
		metric = sqlWindowOrder["time"]
	}
	if metric == "" {
		return nil, ErrInvalidRange
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,collected_at,
		(status='ok' OR coalesce(metrics_json#>>'{collector,pgStatus}','')='ok'),
		metrics_json->'sqlPerformance'
		FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
		ORDER BY collected_at DESC,id DESC LIMIT $4`, assetID, from, to, maxSQLWindowSamples+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	snapshots := make([]sqlSnapshot, 0, 64)
	var latest map[string]any
	truncated := false
	for rows.Next() {
		if len(snapshots) == maxSQLWindowSamples {
			truncated = true
			break
		}
		var item sqlSnapshot
		var raw []byte
		var available bool
		if err := rows.Scan(&item.ID, &item.At, &available, &raw); err != nil {
			return nil, err
		}
		if len(raw) != 0 {
			if err := json.Unmarshal(raw, &item.Data); err != nil {
				return nil, err
			}
		}
		if len(snapshots) == 0 {
			latest = item.Data
		}
		if !available {
			item.Data = nil
		}
		snapshots = append(snapshots, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ranked, pairs := rankSQLWindow(snapshots, queryID, metric)
	count := len(ranked)
	if count > 50 {
		ranked = ranked[:50]
	}
	result := map[string]any{"source": "pg_stat_statements", "sampleCount": len(snapshots),
		"comparablePairs": pairs, "truncated": truncated, "collectionLimited": latest["truncated"] == true,
		"statements": ranked, "total": count, "latestStatus": latest["status"], "reason": latest["reason"]}
	if len(snapshots) != 0 {
		result["firstSampleAt"] = snapshots[len(snapshots)-1].At
		result["lastSampleAt"] = snapshots[0].At
	}
	return map[string]any{"performance": result}, nil
}

// Snapshots are newest first. Adjacent samples across a reset, failure or
// unknown interval cannot contribute to an observed query rate.
func rankSQLWindow(snapshots []sqlSnapshot, queryID, metric string) ([]map[string]any, int) {
	totals := map[string]map[string]any{}
	pairs := 0
	for i := 0; i+1 < len(snapshots); i++ {
		after, before := snapshots[i], snapshots[i+1]
		seconds := after.At.Sub(before.At).Seconds()
		if seconds <= 0 || stringValue(after.Data["status"]) != "ok" || stringValue(before.Data["status"]) != "ok" {
			continue
		}
		deltas := sqlIntervalDelta(after.Data, before.Data, true)
		if deltas == nil {
			continue
		}
		pairs++
		for _, statement := range sqlStatementRows(after.Data["statements"]) {
			id := stringValue(statement["queryId"])
			key := stringValue(statement["fingerprint"])
			delta := deltas[key]
			if id == "" || key == "" || (queryID != "" && queryID != id) || delta == nil {
				continue
			}
			row := totals[key]
			if row == nil {
				row = map[string]any{"queryId": id, "fingerprint": key,
					"databaseOid": statement["databaseOid"], "userOid": statement["userOid"], "topLevel": statement["topLevel"]}
				totals[key] = row
			}
			for _, field := range sqlCounterFields {
				row[field] = number(row[field]) + number(delta[field])
			}
			row["coveredSeconds"] = number(row["coveredSeconds"]) + seconds
		}
	}
	ranked := make([]map[string]any, 0, len(totals))
	for _, row := range totals {
		if number(row["calls"]) == 0 {
			continue
		}
		row["averageExecMs"] = number(row["totalExecMs"]) / number(row["calls"])
		row["callsPerSecond"] = number(row["calls"]) / number(row["coveredSeconds"])
		ranked = append(ranked, row)
	}
	sort.Slice(ranked, func(i, j int) bool {
		left, right := number(ranked[i][metric]), number(ranked[j][metric])
		if left == right {
			return stringValue(ranked[i]["fingerprint"]) < stringValue(ranked[j]["fingerprint"])
		}
		return left > right
	})
	return ranked, pairs
}
