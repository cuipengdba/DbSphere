package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
)

const (
	etcdLeaderGroup = "etcd_leader_change"
	etcdHealthGroup = "etcd_health_degraded"
	etcdDiskGroup   = "etcd_disk_latency"
)

func (s *Postgres) checkEtcd(ctx context.Context, id string, target collector.Target, token string, lease *CollectionLease, interval time.Duration, infra nativeInfraTarget) (CheckResult, error) {
	snapshot, err := s.collectNativeObservation(ctx, id, target.Port, infra, lease, func() (collectoradapter.Snapshot, error) {
		return (collector.EtcdAdapter{}).Collect(ctx, target, token)
	})
	group, reason := "", ""
	metrics := snapshot.Metrics
	if err != nil {
		var cert *tls.CertificateVerificationError
		switch {
		case errors.Is(err, collector.ErrEtcdPermission), errors.Is(err, collector.ErrEtcdVersion), errors.Is(err, collector.ErrEtcdTLS):
			group, reason = "telemetry", err.Error()
		case errors.As(err, &cert):
			group, reason = "telemetry", "etcd TLS certificate or hostname verification failed"
		default:
			group, reason = "availability", "etcd 观测端点不可达"
		}
		metrics["collector"] = map[string]any{"etcdStatus": group}
	} else {
		metrics["adapter"] = snapshot.Report
		metrics["collector"] = map[string]any{"etcdStatus": "ok"}
	}
	return s.recordCheckWithLease(ctx, id, metrics, group, reason, time.Now().UTC(), lease, interval)
}

type etcdLatencyWindow struct {
	Operations float64  `json:"observations"`
	MeanMS     float64  `json:"meanMs"`
	P95UpperMS *float64 `json:"p95UpperMs"`
}

type etcdWindow struct {
	Previous string             `json:"previousSampleId"`
	WAL      *etcdLatencyWindow `json:"wal"`
	Backend  *etcdLatencyWindow `json:"backend"`
	Kube     *etcdLatencyWindow `json:"kubernetes"`
}

func etcdLatency(after, before collector.EtcdHistogram) *etcdLatencyWindow {
	if after.Count == nil || after.Sum == nil || before.Count == nil || before.Sum == nil ||
		*after.Count <= *before.Count || *after.Sum < *before.Sum {
		return nil
	}
	count := *after.Count - *before.Count
	value := &etcdLatencyWindow{Operations: count, MeanMS: 1000 * (*after.Sum - *before.Sum) / count}
	if len(after.Buckets) != len(before.Buckets) {
		return value
	}
	var prev float64
	var upper *float64
	for i, bucket := range after.Buckets {
		old := before.Buckets[i]
		delta := bucket.Count - old.Count
		if bucket.UpperSeconds != old.UpperSeconds || delta < prev || delta > count {
			return value
		}
		prev = delta
		if upper == nil && delta >= count*0.95 {
			threshold := bucket.UpperSeconds * 1000
			upper = &threshold
		}
	}
	value.P95UpperMS = upper
	return value
}

func etcdProcessContinues(current, previous *float64, priorAt time.Time) bool {
	if current == nil || previous == nil {
		return false
	}
	// A scrape can derive the process timestamp from an adjusted boot clock.
	// Accept minor drift only for a process known to predate the previous sample.
	const tolerance = 5
	before := float64(priorAt.Unix()) - tolerance
	return *current <= before && *previous <= before && math.Abs(*current-*previous) <= tolerance
}

func etcdInterval(current, previous collector.EtcdMetrics, priorAt, now time.Time, interval time.Duration, previousID string) *etcdWindow {
	limit := 15 * time.Minute
	if interval > 0 {
		limit = 2 * interval
	}
	elapsed := now.Sub(priorAt)
	if current.MemberID == "" || current.MemberID != previous.MemberID || !etcdProcessContinues(current.Started, previous.Started, priorAt) ||
		elapsed <= 0 || elapsed > limit || previousID == "" {
		return nil
	}
	window := &etcdWindow{Previous: previousID, WAL: etcdLatency(current.WAL, previous.WAL), Backend: etcdLatency(current.Backend, previous.Backend)}
	kube, old := current.Kubernetes, previous.Kubernetes
	if kube != nil && old != nil && kube.Context == old.Context && etcdProcessContinues(kube.Start, old.Start, priorAt) {
		window.Kube = etcdLatency(kube.Latency, old.Latency)
	}
	return window
}

func recordEtcdEvents(ctx context.Context, tx pgx.Tx, assetID, sampleID, previousID string, now time.Time,
	current, previous collector.EtcdMetrics, window *etcdWindow) error {
	hasLeader := current.HasLeader
	if hasLeader == nil && current.LeaderID != "" {
		yes := 1.0
		hasLeader = &yes
	}
	noLeader := hasLeader != nil && *hasLeader == 0
	if hasLeader != nil {
		changed := current.ClusterID != "" && current.ClusterID == previous.ClusterID &&
			current.LeaderID != "" && previous.LeaderID != "" && current.LeaderID != previous.LeaderID
		payload := map[string]any{"memberId": current.MemberID, "leaderId": current.LeaderID, "hasLeader": hasLeader,
			"clusterId": current.ClusterID, "raftTerm": current.RaftTerm}
		if previous.ClusterID != "" && current.ClusterID == previous.ClusterID {
			payload["previousLeader"], payload["previousSampleId"] = previous.LeaderID, previousID
		}
		if err := recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, etcdLeaderGroup, "etcd Leader 中断或切换",
			"/metrics /v3/maintenance/status", noLeader || changed, payload, payload); err != nil {
			return err
		}
	}
	if current.Health != nil && !noLeader {
		health := map[string]any{"health": *current.Health, "memberId": current.MemberID}
		if err := recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, etcdHealthGroup, "etcd 健康检查未通过",
			"/health", !*current.Health, health, health); err != nil {
			return err
		}
	}
	if window != nil {
		walObserved := window.WAL != nil && window.WAL.Operations >= 3
		commitObserved := window.Backend != nil && window.Backend.Operations >= 3
		slow := walObserved && window.WAL.MeanMS >= 25 || commitObserved && window.Backend.MeanMS >= 50
		if slow || walObserved && commitObserved {
			payload := map[string]any{"wal": window.WAL, "backend": window.Backend,
				"previousSampleId": window.Previous, "memberId": current.MemberID}
			if err := recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, etcdDiskGroup, "etcd WAL 或后端提交延迟升高",
				"etcd_disk_wal_fsync_duration_seconds / etcd_disk_backend_commit_duration_seconds", slow, payload, payload); err != nil {
				return err
			}
		}
	}
	return nil
}

func priorEtcdSample(raw []byte) collector.EtcdMetrics {
	var sample struct {
		Etcd collector.EtcdMetrics `json:"etcd"`
	}
	_ = json.Unmarshal(raw, &sample)
	return sample.Etcd
}
