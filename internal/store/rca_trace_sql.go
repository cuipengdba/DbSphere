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
