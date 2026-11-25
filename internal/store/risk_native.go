package store

import (
	"context"
	"encoding/json"
	"math"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
)

type RiskProfile struct {
	TimeZone string `json:"timeZone"`
	Start    int    `json:"businessStartHour"`
	End      int    `json:"businessEndHour"`
}

func (p RiskProfile) period(at time.Time) string {
	if riskWeekend(at) {
		return "weekend"
	}
	if p.Start == p.End || p.Start < p.End && at.Hour() >= p.Start && at.Hour() < p.End ||
		p.Start > p.End && (at.Hour() >= p.Start || at.Hour() < p.End) {
		return "business"
	}
	return "off_hours"
}

func migrateNativeRisk(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=61)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE asset_risk_mode ADD COLUMN profile jsonb NOT NULL DEFAULT '{}'::jsonb,
		ADD COLUMN profile_changed_at timestamptz;
		INSERT INTO dbsphere_schema_migrations(version) VALUES(61)`)
	return err
}

func (s *Postgres) SetRiskProfile(ctx context.Context, asset string, profile RiskProfile) error {
	if _, err := time.LoadLocation(profile.TimeZone); err != nil || len(profile.TimeZone) == 0 || len(profile.TimeZone) > 80 ||
		profile.TimeZone == "Local" || profile.Start < 0 || profile.Start > 23 || profile.End < 0 || profile.End > 23 {
		return ErrRiskInput
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO asset_risk_mode(asset_id,profile,profile_changed_at)
		SELECT id,$2,now() FROM (SELECT id FROM assets WHERE id=$1 AND ($3='' OR organization_id::text=$3) FOR UPDATE) a
		ON CONFLICT(asset_id) DO UPDATE SET profile=excluded.profile,enabled=false,profile_changed_at=excluded.profile_changed_at
		WHERE asset_risk_mode.profile<>excluded.profile`, asset, raw, OrganizationFromContext(ctx))
	return err
}

func nativeRiskSpecs(engine string) []riskSpec {
	switch engine {
	case "dm8":
		return []riskSpec{
			nativeRiskSpec("dm8_utilization", "达梦表空间使用接近阈值", "storage", 24, 85, 1, 5),
			nativeRiskSpec("dm8_replay", "达梦复制回放差距持续偏高", "dm8_ha_replication", 4, 5000, 1.75, 1000),
		}
	case "kingbase":
		return []riskSpec{
			nativeRiskSpec("kingbase_capacity", "金仓数据库容量增长偏高", "storage", 48, 0, 1.15, 128*1024*1024),
			nativeRiskSpec("kingbase_replay", "金仓复制回放差距持续偏高", "kingbase_ha_replication", 4, 1024*1024, 1.75, 256*1024),
		}
	case "redis":
		return []riskSpec{
			nativeRiskSpec("redis_eviction_rate", "Redis 淘汰速率持续偏高", redisEvictionGroup, 4, .1, 1.75, .02),
			nativeRiskSpec("redis_hit_loss", "Redis 缓存未命中率持续偏高", "redis_cache_efficiency", 4, 15, 1.25, 8),
		}
	case "etcd":
		return []riskSpec{
			nativeRiskSpec("etcd_quota", "etcd 使用量接近空间配额", "storage", 24, 80, 1, 5),
			nativeRiskSpec("etcd_commit", "etcd 后端提交耗时持续偏高", etcdDiskGroup, 4, 25, 1.75, 8),
		}
	case "mysql":
		return nil
	}
	return forecastSpecs
}

func isNativeRiskEngine(engine string) bool {
	switch engine {
	case "dm8", "kingbase", "redis", "etcd":
		return true
	}
	return false
}

func nativeRiskSpec(metric, title, group string, horizon int, minimum, factor, change float64) riskSpec {
	return riskSpec{metric, title, group, time.Duration(horizon) * time.Hour,
		func(center float64) float64 { return max(minimum, center*factor, center+change) },
		func(hour riskHour) *float64 { return hour.Native[metric] }}
}

const nativeRiskHoursSQL = `SELECT bucket,coalesce(s.id::text,''),coalesce(s.metrics_json->>'role',''),
	CASE WHEN s.status='ok' THEN s.metrics_json END,
	EXISTS(SELECT 1 FROM silences w WHERE w.kind='maintenance' AND w.revoked_at IS NULL
		AND (w.asset_id=$1 OR w.asset_id IS NULL) AND w.starts_at<bucket+interval '1 hour' AND w.ends_at>bucket)
	OR EXISTS(SELECT 1 FROM asset_changes c WHERE c.asset_id=$1 AND c.source='operator'
		AND c.kind IN ('maintenance','deployment','configuration')
		AND c.occurred_at>=bucket-interval '2 hours' AND c.occurred_at<bucket+interval '1 hour')
	FROM generate_series($2::timestamptz-interval '21 days',$2::timestamptz-interval '1 hour',interval '1 hour') AS bucket
	LEFT JOIN LATERAL (SELECT id,status,metrics_json FROM samples WHERE asset_id=$1
		AND collected_at>=bucket AND collected_at<bucket+interval '1 hour' ORDER BY collected_at DESC,id DESC LIMIT 1) s ON true
	WHERE s.id IS NOT NULL ORDER BY bucket`

func readNativeRiskHours(ctx context.Context, tx pgx.Tx, asset, engine string, hour time.Time) ([]riskHour, error) {
	rows, err := tx.Query(ctx, nativeRiskHoursSQL, asset, hour)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []riskHour
	for rows.Next() {
		var b riskHour
		var raw []byte
		if err := rows.Scan(&b.At, &b.SampleID, &b.Role, &raw, &b.Planned); err != nil {
			return nil, err
		}
		var sample map[string]any
		if b.Role != "" && json.Unmarshal(raw, &sample) == nil {
			b.Native = nativeRiskReadings(engine, sample)
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

func nativeRiskReadings(engine string, sample map[string]any) map[string]*float64 {
	result := map[string]*float64{}
	data := object(sample[engine])
	switch engine {
	case "dm8":
		filesTruncated, _ := data["filesTruncated"].(bool)
		var total, free float64
		for _, item := range list(data["files"]) {
			file := object(item)
			allocated, allocatedOK := numeric(file["allocatedBytes"])
			available, availableOK := numeric(file["freeBytes"])
			if !allocatedOK || !availableOK || allocated < 0 || available < 0 || available > allocated {
				filesTruncated = true
				break
			}
			total += allocated
			free += available
		}
		if !filesTruncated && total > 0 {
			used := 100 * (total - free) / total
			result["dm8_utilization"] = &used
		}
		result["dm8_replay"] = riskReplayGap(data)
	case "kingbase":
		result["kingbase_capacity"] = dependencyNumber(data["databaseBytes"])
		result["kingbase_replay"] = riskReplayGap(data)
	case "redis":
		window := object(sample["redisInterval"])
		if evicted, ok := numeric(window["evicted"]); ok && evicted >= 0 && number(window["seconds"]) > 0 {
			rate := evicted / number(window["seconds"])
			result["redis_eviction_rate"] = &rate
		}
		if hits, ok := numeric(window["hitPercent"]); ok && hits >= 0 && hits <= 100 {
			loss := 100 - hits
			result["redis_hit_loss"] = &loss
		}
	case "etcd":
		if bytes, ok := numeric(data["sizeBytes"]); ok && bytes >= 0 && number(data["quotaBytes"]) > 0 {
			used := bytes / number(data["quotaBytes"]) * 100
			result["etcd_quota"] = &used
		}
		result["etcd_commit"] = dependencyNumber(object(object(sample["etcdInterval"])["backend"])["meanMs"])
	}
	return result
}

func riskReplayGap(data map[string]any) *float64 {
	ha := object(data["ha"])
	if gap, ok := numeric(ha["replayGap"]); ok && gap >= 0 {
		return &gap
	}
	if truncated, _ := ha["peersTruncated"].(bool); truncated {
		return nil
	}
	largest := -1.0
	for _, item := range list(ha["peers"]) {
		peer := object(item)
		if peer["kind"] != "downstream" {
			continue
		}
		gap, ok := numeric(peer["replayGap"])
		if !ok || gap < 0 {
			return nil
		}
		largest = math.Max(largest, gap)
	}
	if largest < 0 {
		return nil
	}
	return &largest
}
