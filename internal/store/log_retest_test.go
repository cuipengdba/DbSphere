package store

import "testing"

func TestLogRetestDecision(t *testing.T) {
	measure := func(p95 float64, timed int64) logCompareMeasure {
		return logCompareMeasure{P95: &p95, Timed: timed}
	}
	for _, test := range []struct {
		name     string
		coverage string
		current  logCompareMeasure
		target   float64
		want     string
	}{
		{"target met", "", measure(400, 12), 450, "met"},
		{"target missed", "", measure(700, 12), 550, "missed"},
		{"regression", "", measure(1200, 12), 1500, "regressed"},
		{"small sample", "", measure(400, 2), 450, "insufficient"},
		{"changed threshold", "采集方式需要对齐", measure(400, 12), 450, "insufficient"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, reason := logRetestDecision(test.coverage, measure(800, 12), test.current, test.target)
			if status != test.want || reason == "" {
				t.Fatalf("decision: %s, %s", status, reason)
			}
		})
	}
}
