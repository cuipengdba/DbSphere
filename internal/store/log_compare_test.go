package store

import "testing"

func TestLogComparisonRequiresObservedResolution(t *testing.T) {
	before, fast, regression := 1450., 210., 1990.
	prior := &logCompareMeasure{Count: 20, Timed: 20, Slow: 14, P95: &before}
	tests := []struct {
		label      string
		current    *logCompareMeasure
		comparable bool
		want       string
	}{
		{"filtered by slow logging", nil, true, "unobserved"},
		{"threshold changed", &logCompareMeasure{Count: 21, Timed: 21, P95: &fast}, false, "uncertain"},
		{"observed below threshold", &logCompareMeasure{Count: 21, Timed: 21, P95: &fast}, true, "resolved"},
		{"one timed query", &logCompareMeasure{Count: 21, Timed: 1, P95: &fast}, true, "uncertain"},
		{"regression", &logCompareMeasure{Count: 18, Timed: 18, Slow: 17, P95: &regression}, true, "regressed"},
	}
	for _, test := range tests {
		t.Run(test.label, func(t *testing.T) {
			if got := classifyLogChange(&logCompareRow{Kind: "statement", Baseline: prior, Current: test.current}, test.comparable); got != test.want {
				t.Fatalf("got %s, want %s", got, test.want)
			}
		})
	}
}
