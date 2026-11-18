package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

type capacityPoint struct {
	Day           time.Time `json:"day"`
	DatabaseBytes float64   `json:"databaseBytes"`
	SampleID      string    `json:"sampleId"`
}

func (s *Postgres) CapacityForecast(ctx context.Context, assetID string) (any, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (day) day,database_bytes,id::text FROM (
		SELECT date_trunc('day',collected_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS day,
			metrics_json->>'database_bytes' AS database_bytes,id,collected_at
		FROM samples WHERE asset_id=$1 AND collected_at>=now()-interval '30 days'
			AND collected_at<=now() AND status='ok'
			AND metrics_json->>'database_bytes' ~ '^[0-9]{1,19}$'
	) daily ORDER BY day,collected_at DESC`, assetID)
	if err != nil {
		return nil, fmt.Errorf("read database capacity: %w", err)
	}
	defer rows.Close()
	points := []capacityPoint{}
	for rows.Next() {
		var point capacityPoint
		var raw string
		if err := rows.Scan(&point.Day, &raw, &point.SampleID); err != nil {
			return nil, err
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			continue
		}
		point.DatabaseBytes = value
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Day.Before(points[j].Day) })
	return forecastDatabaseCapacity(points), nil
}

func forecastDatabaseCapacity(points []capacityPoint) map[string]any {
	result := map[string]any{"status": "insufficient",
		"reason": "至少需要跨 3 个自然日的有效数据库容量样本", "points": points}
	if len(points) < 3 {
		return result
	}
	spanDays := points[len(points)-1].Day.Sub(points[0].Day).Hours() / 24
	if spanDays < 2 {
		return result
	}
	slopes := []float64{}
	for i := 1; i < len(points); i++ {
		days := points[i].Day.Sub(points[i-1].Day).Hours() / 24
		if days > 0 {
			slopes = append(slopes, math.Max(0,
				(points[i].DatabaseBytes-points[i-1].DatabaseBytes)/days))
		}
	}
	if len(slopes) == 0 {
		return result
	}
	sort.Float64s(slopes)
	middle := len(slopes) / 2
	median := slopes[middle]
	if len(slopes)%2 == 0 {
		median = (slopes[middle-1] + slopes[middle]) / 2
	}
	latest := points[len(points)-1]
	confidence := "low"
	if len(points) >= 7 && spanDays >= 7 {
		confidence = "medium"
	}
	return map[string]any{"status": "estimate", "points": points, "observedDays": spanDays,
		"dailyGrowthBytes": math.Round(median), "projected30DayBytes": math.Round(latest.DatabaseBytes + median*30),
		"latestBytes": latest.DatabaseBytes, "latestSampleId": latest.SampleID,
		"confidence": confidence,
		"method":     "最近 30 天每日末次样本的相邻增长速率中位数；下降按零增长处理"}
}
