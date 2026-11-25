package store

import (
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
)

func TestEtcdWindowUsesSameProcessAndReportsFiniteHistogramBound(t *testing.T) {
	n := func(value float64) *float64 { return &value }
	old := collector.EtcdMetrics{MemberID: "a", Started: n(1), WAL: collector.EtcdHistogram{
		Count: n(4), Sum: n(0.100), Buckets: []collector.EtcdBucket{{UpperSeconds: .01, Count: 2}, {UpperSeconds: .05, Count: 4}},
	}}
	current := collector.EtcdMetrics{MemberID: "a", Started: n(1), WAL: collector.EtcdHistogram{
		Count: n(8), Sum: n(.200), Buckets: []collector.EtcdBucket{{UpperSeconds: .01, Count: 3}, {UpperSeconds: .05, Count: 8}},
	}}
	prior := time.Unix(600, 0)
	now := prior.Add(time.Minute)
	window := etcdInterval(current, old, prior, now, time.Minute, "old")
	if window == nil || window.WAL == nil || window.WAL.Operations != 4 || window.WAL.MeanMS != 25 ||
		window.WAL.P95UpperMS == nil || *window.WAL.P95UpperMS != 50 {
		t.Fatalf("unexpected etcd latency window: %#v", window)
	}
	current.Started = n(620)
	if again := etcdInterval(current, old, prior, now, time.Minute, "old"); again != nil {
		t.Fatalf("expected a sampling gap after restart: %#v", again)
	}
	current.Started = n(3)
	current.WAL.Buckets[1].Count = 6
	window = etcdInterval(current, old, prior, now, time.Minute, "old")
	if window == nil || window.WAL == nil || window.WAL.P95UpperMS != nil {
		t.Fatalf("P95 beyond the last finite bucket must remain unknown: %#v", window.WAL)
	}
}
