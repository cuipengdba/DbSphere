package store

import (
	"math"
	"testing"
	"time"
)

func TestSQLWindowCountsIdleCoverageAndDoesNotCrossGaps(t *testing.T) {
	at := time.Now().UTC()
	sample := func(offset int, reset string, calls, elapsed float64) sqlSnapshot {
		statement := map[string]any{"queryId": "-234", "fingerprint": "1:2:-234:1"}
		for _, field := range sqlCounterFields {
			statement[field] = float64(0)
		}
		statement["calls"], statement["totalExecMs"] = calls, elapsed
		return sqlSnapshot{At: at.Add(time.Duration(offset) * time.Second), Data: map[string]any{
			"status": "ok", "resetAt": reset, "statements": []map[string]any{statement},
		}}
	}
	samples := []sqlSnapshot{
		sample(50, "reset-B", 150, 1000),
		sample(40, "reset-A", 55, 900),
		{At: at.Add(30 * time.Second)},
		sample(20, "reset-A", 4, 80),
		sample(10, "reset-A", 2, 40),
		sample(0, "reset-A", 2, 40),
	}
	ranked, pairs := rankSQLWindow(samples, "-234", "callsPerSecond")
	if pairs != 2 || len(ranked) != 1 || number(ranked[0]["calls"]) != 2 ||
		number(ranked[0]["totalExecMs"]) != 40 || number(ranked[0]["coveredSeconds"]) != 20 ||
		math.Abs(number(ranked[0]["callsPerSecond"])-0.1) > 1e-9 {
		t.Fatalf("mixed reset, failure or idle intervals into the ranking: %v (%d pairs)", ranked, pairs)
	}
}
