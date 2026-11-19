package store

import "time"

// A PostgreSQL Query ID identifies a statement shape, but may appear in
// multiple pg_stat_statements rows. Link a request only when the event samples
// resolve it to exactly one fingerprint in the same five-minute window.
func traceSQLFingerprint(samples map[string]sampleObservation, queryID string, start, end time.Time) string {
	if queryID == "" {
		return ""
	}
	fingerprints := map[string]bool{}
	for _, sample := range samples {
		if sample.Status != "ok" || sample.At.Before(start.Add(-5*time.Minute)) ||
			sample.At.After(end.Add(5*time.Minute)) {
			continue
		}
		performance := object(sample.Metrics["sqlPerformance"])
		if stringValue(performance["status"]) != "ok" {
			continue
		}
		for _, item := range list(performance["statements"]) {
			statement := object(item)
			if stringValue(statement["queryId"]) == queryID {
				if fingerprint := stringValue(statement["fingerprint"]); fingerprint != "" {
					fingerprints[fingerprint] = true
				}
			}
		}
	}
	if len(fingerprints) != 1 {
		return ""
	}
	for fingerprint := range fingerprints {
		return fingerprint
	}
	return ""
}

// A matching application span closes the candidate's explicit link gap. The
// custom Query ID still needs human verification against the database session.
func attachTraceToSQLPlanCandidate(candidate map[string]any, queryID, spanNode string) bool {
	if candidate == nil || queryID == "" ||
		stringValue(object(candidate["detail"])["queryId"]) != queryID {
		return false
	}
	supports, ok := candidate["supports"].([]string)
	if !ok {
		return false
	}
	for _, support := range supports {
		if support == spanNode {
			return false
		}
	}
	if len(supports) >= 4 {
		return false
	}
	candidate["supports"] = append(supports, spanNode)
	candidate["missing"] = []string{
		"应用自定义 Query ID 与数据库会话身份需复核", "Query ID 复用或统计重置未排除"}
	candidate["matchedConditions"] = append(candidate["matchedConditions"].([]any),
		"应用数据库 Span 的 Query ID 与同一事件的 SQL 指纹一致")
	return true
}
