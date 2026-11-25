package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

const (
	redisEvictionGroup = "redis_eviction"
	redisSentinelGroup = "redis_sentinel_failover"
	redisSlotsGroup    = "redis_slots_unhealthy"
)

func (s *Postgres) checkRedis(ctx context.Context, id string, target collector.Target, password string, lease *CollectionLease, interval time.Duration, infra nativeInfraTarget) (CheckResult, error) {
	snapshot, err := s.collectNativeObservation(ctx, id, target.Port, infra, lease, func() (collectoradapter.Snapshot, error) {
		return (collector.RedisAdapter{}).Collect(ctx, target, password)
	})
	group, reason := "", ""
	metrics := snapshot.Metrics
	if err != nil {
		var certificate *tls.CertificateVerificationError
		switch {
		case errors.Is(err, collector.ErrRedisVersion), errors.Is(err, collector.ErrRedisConfig):
			group, reason = "telemetry", err.Error()
		case errors.As(err, &certificate):
			group, reason = "telemetry", "Redis TLS certificate or hostname verification failed"
		case redis.HasErrorPrefix(err, "WRONGPASS"), redis.HasErrorPrefix(err, "NOAUTH"):
			group, reason = "telemetry", "Redis authentication failed"
		case redis.HasErrorPrefix(err, "NOPERM"):
			group, reason = "telemetry", "Redis monitoring account cannot read INFO server"
		default:
			group, reason = "availability", "Redis connection or collection failed"
		}
		metrics["collector"] = map[string]any{"redisStatus": group}
	} else {
		metrics["adapter"] = snapshot.Report
		metrics["collector"] = map[string]any{"redisStatus": "ok"}
	}
	return s.recordCheckWithLease(ctx, id, metrics, group, reason, time.Now().UTC(), lease, interval)
}

type redisIntervalMetrics struct {
	HitPercent       *float64 `json:"hitPercent"`
	Evicted          *int64   `json:"evicted"`
	Rejected         *int64   `json:"rejected"`
	Seconds          float64  `json:"seconds"`
	PreviousSampleID string   `json:"previousSampleId"`
}

func redisInterval(current, previous collector.RedisMetrics, elapsed, configuredInterval time.Duration, sampleID string) *redisIntervalMetrics {
	limit := 15 * time.Minute
	if configuredInterval > 0 {
		limit = 2 * configuredInterval
	}
	if current.Mode == "sentinel" || current.RunID == "" || current.RunID != previous.RunID ||
		current.Mode != previous.Mode || elapsed <= 0 || elapsed > limit || sampleID == "" {
		return nil
	}
	diff := func(after, before *int64) *int64 {
		if after == nil || before == nil || *after < *before {
			return nil
		}
		delta := *after - *before
		return &delta
	}
	hits, misses := diff(current.Hits, previous.Hits), diff(current.Misses, previous.Misses)
	window := &redisIntervalMetrics{Evicted: diff(current.Evicted, previous.Evicted), Rejected: diff(current.Rejected, previous.Rejected),
		Seconds: elapsed.Seconds(), PreviousSampleID: sampleID}
	if hits != nil && misses != nil {
		total := float64(*hits) + float64(*misses)
		if total > 0 {
			percent := 100 * float64(*hits) / total
			window.HitPercent = &percent
		}
	}
	return window
}

func recordRedisEvents(ctx context.Context, tx pgx.Tx, assetID, sampleID, previousID string, now time.Time,
	current, before collector.RedisMetrics, window *redisIntervalMetrics) error {
	if window != nil && window.Evicted != nil {
		source := "INFO stats.evicted_keys"
		payload := map[string]any{"value": *window.Evicted, "memoryBytes": current.Memory, "maxMemoryBytes": current.MaxMemory,
			"intervalSeconds": window.Seconds, "previousSampleId": previousID}
		if err := recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, redisEvictionGroup, "Redis 缓存发生淘汰",
			source, *window.Evicted > 0, payload, map[string]any{"value": 0, "previousSampleId": previousID}); err != nil {
			return err
		}
	}
	if current.Sentinel != nil && current.Sentinel.State != "unknown" {
		master := current.Sentinel
		payload := map[string]any{"group": master.Group, "master": master.Master, "masterState": master.State, "replicas": master.Replicas}
		changed := false
		if before.Sentinel != nil && before.Sentinel.Group == master.Group {
			old := before.Sentinel
			inconsistentRunID := old.RunID != "" && master.RunID != "" && old.RunID != master.RunID
			changed = old.Master != master.Master || inconsistentRunID
			payload["previousSampleId"], payload["previousMaster"] = previousID, old.Master
		}
		if err := recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, redisSentinelGroup, "Redis Sentinel 主节点异常或切换",
			"SENTINEL MASTER", master.State != "ok" || changed, payload, payload); err != nil {
			return err
		}
	}
	if current.ClusterState == "ok" || current.ClusterState == "fail" {
		payload := map[string]any{"clusterState": current.ClusterState, "slotsOk": current.SlotsOK, "slotsFail": current.SlotsFail,
			"slotsPfail": current.SlotsPFail}
		if err := recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, redisSlotsGroup, "Redis Cluster 槽位异常",
			"CLUSTER INFO", current.ClusterState == "fail", payload, payload); err != nil {
			return err
		}
	}
	return nil
}

func priorRedisSample(raw []byte) collector.RedisMetrics {
	var sample struct {
		Redis collector.RedisMetrics `json:"redis"`
	}
	_ = json.Unmarshal(raw, &sample)
	return sample.Redis
}
