package collector

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const waitCaptureDuration = 10 * time.Second

type WaitFrame struct {
	Slot     int             `json:"slot"`
	At       time.Time       `json:"at"`
	QueryMS  float64         `json:"queryMs"`
	Status   string          `json:"status"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"`
}

type WaitCapture struct {
	Profile        string      `json:"profile"`
	StartedAt      time.Time   `json:"startedAt"`
	EndedAt        time.Time   `json:"endedAt"`
	IntervalMS     int64       `json:"intervalMs"`
	Expected       int         `json:"expected"`
	Observed       int         `json:"observed"`
	QueryTotalMS   float64     `json:"queryTotalMs"`
	QueryMaximumMS float64     `json:"queryMaximumMs"`
	Frames         []WaitFrame `json:"frames"`
}

// CaptureWaits takes a bounded series on one read-only connection. The lock
// manager is queried at most once a second even in the enhanced profile.
func CaptureWaits(ctx context.Context, target Target, password, profile string) (WaitCapture, error) {
	period := time.Second
	if profile == "enhanced" {
		period = 250 * time.Millisecond
	}
	conf, err := readOnlyPostgresConfig(target, password)
	if err != nil {
		return WaitCapture{}, err
	}
	conf.RuntimeParams["statement_timeout"] = "220"
	conf.RuntimeParams["application_name"] = "dbsphere-wait-capture"
	connect, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(connect, conf)
	if err != nil {
		return WaitCapture{}, errors.New("PostgreSQL wait capture connection unavailable")
	}
	defer conn.Close(context.Background())
	var version int
	if err := conn.QueryRow(connect, `SELECT current_setting('server_version_num')::int`).Scan(&version); err != nil || !PostgresMajorSupported(version/10000) {
		return WaitCapture{}, errors.New("PostgreSQL wait capture requires a reachable supported PostgreSQL version")
	}
	start := time.Now()
	capture := WaitCapture{Profile: profile, StartedAt: start.UTC(), IntervalMS: period.Milliseconds(), Expected: int(waitCaptureDuration / period), Frames: []WaitFrame{}}
	nextGraph := start
	for slot := 0; slot < capture.Expected && ctx.Err() == nil; slot++ {
		due := start.Add(time.Duration(slot) * period)
		if time.Since(due) >= period {
			continue
		}
		if !waitUntil(ctx, due) {
			break
		}
		begin := time.Now()
		graph := !begin.Before(nextGraph)
		if graph {
			nextGraph = begin.Add(time.Second)
		}
		frame := WaitFrame{Slot: slot, At: begin.UTC(), Status: "unavailable"}
		var raw []byte
		if err := conn.QueryRow(ctx, waitCaptureSQL, graph).Scan(&raw); err == nil {
			frame.Status = "observed"
			frame.Snapshot = raw
			capture.Observed++
		}
		frame.QueryMS = float64(time.Since(begin).Microseconds()) / 1000
		capture.QueryTotalMS += frame.QueryMS
		capture.QueryMaximumMS = max(capture.QueryMaximumMS, frame.QueryMS)
		capture.Frames = append(capture.Frames, frame)
	}
	capture.EndedAt = time.Now().UTC()
	return capture, nil
}

func waitUntil(ctx context.Context, at time.Time) bool {
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

const waitCaptureSQL = `WITH visibility AS MATERIALIZED (
	SELECT pg_has_role(current_user,'pg_read_all_stats','USAGE') AND current_setting('track_activities')='on' AS full_access
), activity AS MATERIALIZED (
	SELECT a.pid,a.leader_pid AS "leaderPid",a.backend_start AS "backendStart",a.xact_start AS "xactStart",
		a.query_start AS "queryStart",a.query_id::text AS "queryId",a.state,a.wait_event_type AS "waitType",
		a.wait_event AS "waitEvent",a.backend_type AS "backendType"
	FROM pg_stat_activity a CROSS JOIN visibility v
	WHERE v.full_access AND a.datname=current_database() AND a.pid<>pg_backend_pid()
		AND a.application_name NOT IN ('dbsphere-monitor-go','dbsphere-wait-capture')
		AND a.backend_type IN ('client backend','parallel worker')
), candidates AS MATERIALIZED (
	SELECT * FROM activity WHERE state IS DISTINCT FROM 'idle'
	ORDER BY ("waitType"='Lock') DESC NULLS LAST,"queryStart" NULLS LAST,pid LIMIT 65
), lock_sources AS MATERIALIZED (
	SELECT * FROM candidates WHERE $1::boolean AND "waitType"='Lock' ORDER BY "queryStart" NULLS LAST,pid LIMIT 17
), edges AS MATERIALIZED (
	SELECT q.pid AS "waitingPid",b.pid AS "blockerPid",q."waitType",q."waitEvent",
		q."queryId" AS "waitingQueryId",holder."queryId" AS "blockerQueryId",q."xactStart" AS "waitingSince",
		holder."xactStart" AS "blockerXactStart",q."backendStart" AS "waitingBackendStart",holder."backendStart" AS "blockerBackendStart"
	FROM (SELECT * FROM lock_sources LIMIT 16) q
	CROSS JOIN LATERAL (SELECT DISTINCT pid FROM unnest(pg_blocking_pids(q.pid)) p(pid)) b
	LEFT JOIN activity holder ON holder.pid=b.pid
	ORDER BY q.pid,b.pid LIMIT 65
)
SELECT jsonb_build_object('at',clock_timestamp(),'serverStartedAt',pg_postmaster_start_time(),
	'visibility',CASE WHEN (SELECT full_access FROM visibility) THEN 'full' ELSE 'limited' END,
	'sessionsTruncated',(SELECT count(*) FROM candidates)>64,
	'lockObserved',$1 AND (SELECT full_access FROM visibility),
	'lockTruncated',(SELECT count(*) FROM lock_sources)>16 OR (SELECT count(*) FROM edges)>64,
	'sessions',coalesce((SELECT jsonb_agg(to_jsonb(q)) FROM (SELECT * FROM candidates LIMIT 64) q),'[]'::jsonb),
	'edges',coalesce((SELECT jsonb_agg(to_jsonb(q)) FROM (SELECT * FROM edges LIMIT 64) q),'[]'::jsonb))`
