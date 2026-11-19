package store

import (
	"testing"
	"time"
)

func TestTraceSQLFingerprintRequiresUniqueContemporaneousQueryID(t *testing.T) {
	now := time.Now().UTC()
	statement := func(queryID, fingerprint string) map[string]any {
		return map[string]any{"queryId": queryID, "fingerprint": fingerprint}
	}
	sample := func(at time.Time, rows ...map[string]any) sampleObservation {
		items := make([]any, len(rows))
		for i, row := range rows {
			items[i] = row
		}
		return sampleObservation{Status: "ok", At: at, Metrics: map[string]any{
			"sqlPerformance": map[string]any{"status": "ok", "statements": items}}}
	}
	start, end := now.Add(-time.Second), now
	samples := map[string]sampleObservation{
		"current": sample(now, statement("42", "1:2:42:1"), statement("99", "1:2:99:1")),
		"repeat":  sample(now.Add(-time.Minute), statement("42", "1:2:42:1")),
	}
	if got := traceSQLFingerprint(samples, "42", start, end); got != "1:2:42:1" {
		t.Fatalf("exact Query ID did not resolve: %q", got)
	}
	if got := traceSQLFingerprint(samples, "43", start, end); got != "" {
		t.Fatalf("unobserved Query ID linked to SQL: %q", got)
	}
	samples["other_role"] = sample(now, statement("42", "1:3:42:1"))
	if got := traceSQLFingerprint(samples, "42", start, end); got != "" {
		t.Fatalf("ambiguous Query ID linked to multiple SQL fingerprints: %q", got)
	}
	delete(samples, "other_role")
	samples["current"] = sample(now.Add(10*time.Minute), statement("42", "1:2:42:1"))
	samples["repeat"] = sample(now.Add(-10*time.Minute), statement("42", "1:2:42:1"))
	if got := traceSQLFingerprint(samples, "42", start, end); got != "" {
		t.Fatalf("out-of-window sample linked to request: %q", got)
	}
}
