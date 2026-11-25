package collector

import (
	"context"
	"time"

	"github.com/cuipengdba/dbsphere/internal/loganalysis"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
)

type highGoWaitSample struct {
	Process     int32     `json:"process"`
	Blockers    []int32   `json:"blockers"`
	Application string    `json:"application"`
	Category    string    `json:"category"`
	Event       string    `json:"event"`
	StartedAt   time.Time `json:"startedAt"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

func collectHighGoWaits(ctx context.Context, conn *pgx.Conn, readStats bool) ([]highGoWaitSample, collectoradapter.Signal) {
	signal := collectoradapter.Signal{Key: "highgoWaitIdentity", Source: "pg_stat_activity / pg_blocking_pids", Status: "ok"}
	if !readStats {
		signal.Status, signal.Reason = "limited_permissions", "当前账号只能核对自身可见会话"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rows, err := conn.Query(ctx, `SELECT pid,pg_blocking_pids(pid),left(coalesce(application_name,''),128),wait_event_type,coalesce(wait_event,''),backend_start,
		CASE WHEN octet_length(query)<pg_size_bytes(current_setting('track_activity_query_size'))-1 THEN query ELSE '' END
		FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active'
		AND wait_event_type IS NOT NULL AND wait_event_type NOT IN ('Client','Timeout')
		AND ($1::boolean OR usename=current_user) ORDER BY query_start NULLS LAST,pid LIMIT 32`, readStats)
	if err != nil {
		signal.Status, signal.Reason = "unavailable", "未取得瀚高会话等待"
		return nil, signal
	}
	defer rows.Close()
	result := []highGoWaitSample{}
	for rows.Next() {
		var sample highGoWaitSample
		var query string
		if err := rows.Scan(&sample.Process, &sample.Blockers, &sample.Application,
			&sample.Category, &sample.Event, &sample.StartedAt, &query); err != nil {
			signal.Status, signal.Reason = "unavailable", "瀚高会话字段与当前构建不兼容"
			return nil, signal
		}
		_, sample.Fingerprint = loganalysis.SQLShape("highgo", query)
		result = append(result, sample)
	}
	if rows.Err() != nil {
		signal.Status, signal.Reason = "unavailable", "未完成瀚高会话等待采样"
		return nil, signal
	}
	return result, signal
}
