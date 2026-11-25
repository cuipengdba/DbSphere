package store

import (
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
)

func TestRedisWindowDoesNotTreatMissingCountersAsZero(t *testing.T) {
	n := func(value int64) *int64 { return &value }
	before := collector.RedisMetrics{Mode: "standalone", RunID: "run", Hits: n(10), Misses: n(5), Evicted: n(2), Rejected: n(10)}
	after := collector.RedisMetrics{Mode: "standalone", RunID: "run", Hits: n(14), Misses: n(6), Evicted: n(5), Rejected: n(1)}
	window := redisInterval(after, before, time.Minute, time.Minute, "older")
	if window == nil || window.HitPercent == nil || *window.HitPercent != 80 ||
		window.Evicted == nil || *window.Evicted != 3 || window.Rejected != nil || window.PreviousSampleID != "older" {
		t.Fatalf("unexpected Redis interval: %#v", window)
	}
	after.RunID = "new"
	if window := redisInterval(after, before, time.Minute, time.Minute, "older"); window != nil {
		t.Fatalf("process change produced a Redis interval: %#v", window)
	}
	after.RunID = before.RunID
	if window := redisInterval(after, before, 3*time.Minute, time.Minute, "older"); window != nil {
		t.Fatalf("gap produced a Redis interval: %#v", window)
	}
	if window := redisInterval(before, before, time.Minute, time.Minute, "older"); window == nil ||
		window.HitPercent != nil || window.Evicted == nil || *window.Evicted != 0 {
		t.Fatalf("no traffic and measured zero eviction must remain distinct: %#v", window)
	}
}
