package store

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

type haPoint struct {
	ID          string          `json:"sampleId"`
	At          time.Time       `json:"at"`
	Status      string          `json:"status"`
	Role        string          `json:"role"`
	Node        string          `json:"node"`
	Control     string          `json:"control"`
	ControlRole string          `json:"controlRole"`
	Controller  string          `json:"controller"`
	Entry       string          `json:"entry"`
	EntryStatus string          `json:"entryStatus"`
	EntryRole   string          `json:"entryRole"`
	Backend     string          `json:"backend"`
	BackendID   string          `json:"backendId"`
	Position    string          `json:"position"`
	Timeline    string          `json:"timeline"`
	ReplayGap   *float64        `json:"replayGap"`
	ReplayState string          `json:"replayState"`
	ReplayUnit  string          `json:"replayUnit"`
	ReplayScope string          `json:"replayScope"`
	Conflict    bool            `json:"conflict"`
	Changes     []string        `json:"changes"`
	Events      json.RawMessage `json:"events"`
}

func haHistoryPoint(runtime string, sample sampleObservation) haPoint {
	m := sample.Metrics
	point := haPoint{ID: sample.ID, At: sample.At, Status: sample.Status, Changes: []string{}}
	route := object(m["routing"])
	probe := object(route["probe"])
	point.Entry, point.EntryStatus = stringValue(route["entry"]), stringValue(route["status"])
	point.EntryRole, point.Backend = stringValue(probe["role"]), stringValue(probe["backend"])
	point.BackendID = stringValue(probe["serverUuid"])
	if sample.Status != "ok" && object(m["collector"])["pgStatus"] != "ok" {
		return point
	}
	point.Role = stringValue(m["role"])
	switch {
	case assets.IsPostgreSQLRuntime(runtime):
		endpoint := object(m["postgresEndpoint"])
		if host, port := stringValue(endpoint["address"]), number(endpoint["port"]); host != "" && port > 0 && port <= 65535 {
			point.Node = net.JoinHostPort(host, strconv.Itoa(int(port)))
		}
		diagnostics := object(m["diagnostics"])
		position := object(object(diagnostics["walPosition"])["data"])
		point.Position, point.Timeline = stringValue(position["position"]), stringValue(position["timeline"])
		point.ReplayUnit = "bytes"
		switch point.Role {
		case "standby":
			point.ReplayScope = "local_wal"
			point.setGap(position["localReplayGapBytes"])
			if position["replayPaused"] == true {
				point.ReplayState = "paused"
			} else if object(object(diagnostics["walReceiver"])["data"])["status"] != "streaming" && point.ReplayState == "caught_up" {
				point.ReplayState = ""
			}
		case "primary":
			point.ReplayScope = "sender_wal"
			links := object(object(diagnostics["replicationLinks"])["data"])
			peers := list(links["links"])
			if total, ok := numeric(links["total"]); ok && total == float64(len(peers)) && total > 0 {
				var maxGap float64
				complete := true
				for _, peer := range peers {
					gap, ok := numeric(object(peer)["replayGapBytes"])
					complete = complete && ok && gap >= 0 && object(peer)["state"] == "streaming"
					maxGap = max(maxGap, gap)
				}
				if complete {
					point.setGap(maxGap)
				}
			}
		}
		cnpg := object(object(m["k8s"])["cloudNativePG"])
		patroni := object(m["patroni"])
		if cnpg["uid"] != nil {
			point.Controller, point.Control = "CloudNativePG", stringValue(cnpg["currentPrimary"])
			observation := object(cnpg["databaseObservation"])
			point.ControlRole = stringValue(observation["operatorRole"])
			point.Conflict = observation["status"] == "mismatch"
		} else if patroni["status"] == "ok" {
			point.Controller = "Patroni"
			if patroni["leaderCount"] == float64(1) {
				for _, raw := range list(patroni["members"]) {
					member := object(raw)
					role := stringValue(member["role"])
					if role == "primary" || role == "leader" || role == "standby_leader" {
						point.Control = stringValue(member["name"])
					}
					if host, port := stringValue(member["host"]), number(member["port"]); net.ParseIP(host) != nil &&
						port > 0 && port <= 65535 && net.JoinHostPort(host, strconv.Itoa(int(port))) == point.Node {
						if role == "primary" || role == "leader" {
							point.ControlRole = "primary"
						} else if role == "replica" || role == "sync_standby" || role == "standby_leader" {
							point.ControlRole = "standby"
						}
					}
				}
				point.Conflict = point.ControlRole != "" && point.ControlRole != point.Role
			}
		}
	case runtime == assets.RuntimeMySQL:
		details := object(m["mysql"])
		point.Node = stringValue(details["serverUuid"])
		if flag, ok := details["serverReadOnly"].(bool); ok {
			point.Role = "writable"
			if flag {
				point.Role = "read_only"
			}
		} else {
			point.Role = ""
		}
		point.ReplayScope = "received_gtid"
		channels := list(details["replication"])
		allApplied := len(channels) > 0 && details["replicationTruncated"] != true
		for _, raw := range channels {
			channel := object(raw)
			switch {
			case channel["receiver"] == "OFF" || channel["applier"] == "OFF":
				point.ReplayState = "stopped"
			case channel["receivedApplied"] == false && point.ReplayState != "stopped":
				point.ReplayState = "lagging"
			}
			allApplied = allApplied && channel["receivedApplied"] == true && channel["receiver"] == "ON" && channel["applier"] == "ON"
		}
		if allApplied {
			point.ReplayState = "caught_up"
		}
	case runtime == assets.RuntimeDM8 || runtime == assets.RuntimeKingbase:
		details := object(object(m[runtime])["ha"])
		point.Node, point.Role = stringValue(details["node"]), stringValue(details["role"])
		point.ReplayUnit, point.ReplayScope = stringValue(details["unit"]), "local_native"
		point.Position = stringValue(details["replayed"])
		if point.Position == "" {
			point.Position = stringValue(details["current"])
		}
		point.setGap(details["replayGap"])
		if point.ReplayState == "caught_up" {
			point.ReplayState = ""
			for _, raw := range list(details["peers"]) {
				peer := object(raw)
				if peer["kind"] == "upstream" && (peer["state"] == "streaming" || peer["state"] == "VALID") {
					point.ReplayState = "caught_up"
				}
			}
		}
		if runtime == assets.RuntimeDM8 {
			point.Controller, point.Control = "Data Watch", stringValue(details["watchState"])
		}
	}
	return point
}

func (p *haPoint) setGap(value any) {
	gap, ok := numeric(value)
	if !ok || gap < 0 {
		return
	}
	p.ReplayGap, p.ReplayState = &gap, "lagging"
	if gap == 0 {
		p.ReplayState = "caught_up"
	}
}

func haChanges(previous, current haPoint) []string {
	changes := []string{}
	continuous := current.At.After(previous.At) && current.At.Sub(previous.At) <= 10*time.Minute
	if !continuous || current.Role == "" || previous.Role == "" {
		changes = append(changes, "gap")
	}
	if !continuous {
		return changes
	}
	for _, value := range []struct{ key, before, after string }{
		{"role", previous.Role, current.Role}, {"node", previous.Node, current.Node},
		{"control", previous.Control, current.Control}, {"timeline", previous.Timeline, current.Timeline},
	} {
		if value.before != "" && value.after != "" && value.before != value.after {
			changes = append(changes, value.key)
		}
	}
	if current.Node != "" && current.Node == previous.Node && current.ReplayScope == previous.ReplayScope &&
		current.ReplayState != "" && previous.ReplayState != "" && current.ReplayState != previous.ReplayState {
		changes = append(changes, "replay")
	}
	if current.Entry != previous.Entry {
		changes = append(changes, "entry_configuration")
	} else if current.Entry != "" && (current.EntryStatus != previous.EntryStatus ||
		current.EntryRole != previous.EntryRole || current.Backend != previous.Backend || current.BackendID != previous.BackendID) {
		changes = append(changes, "route")
	}
	return changes
}

func (s *Postgres) haHistory(ctx context.Context, assetID string, params url.Values) (any, error) {
	from, to, err := timeRange(params, time.Hour, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	event := params.Get("event")
	if event != "" && !evidenceUUID.MatchString(event) {
		return nil, ErrInvalidRange
	}
	rows, err := s.pool.Query(ctx, `WITH focus AS MATERIALIZED (
		SELECT id FROM event_keys WHERE id=NULLIF($5,'')::uuid AND asset_id=$1
	), recent AS MATERIALIZED (
		SELECT s.id,s.collected_at,s.status,s.metrics_json,a.runtime FROM samples s JOIN assets a ON a.id=s.asset_id
		WHERE s.asset_id=$1 AND s.collected_at>=$2 AND s.collected_at<$3
		AND a.runtime IN ('linux','k8s','managed','mysql','dm8','kingbase')
		AND ($4='' OR a.organization_id::text=$4) AND ($5='' OR EXISTS(SELECT 1 FROM focus))
		ORDER BY s.collected_at DESC,s.id DESC LIMIT 101
	), evidence_refs AS MATERIALIZED (
		SELECT DISTINCT r.id AS sample_id,e.id AS event_id,e.title,e.status,
			CASE WHEN ev.kind IN ('collection_recovered','alert_recovered','recovery_verified') THEN 'recovery' ELSE 'observed' END AS phase
		FROM events e JOIN evidence ev ON ev.event_id=e.id
		JOIN recent r ON ev.payload_json->>'sampleId'=r.id::text
		WHERE e.asset_id=$1 AND (e.ended_at IS NULL OR e.ended_at>=$2) AND e.started_at<$3
		AND ev.observed_at>=$2 AND ev.observed_at<$3 AND ($5='' OR e.id=NULLIF($5,'')::uuid)
		AND e.event_group IN ('availability','telemetry','replication','mysql_ha_entry','mysql_ha_replication',
			'dm8_ha_role','dm8_ha_replication','dm8_ha_archive','kingbase_ha_role','kingbase_ha_replication','kingbase_ha_archive')
		AND (ev.kind IN ('routing_observed','alert_firing','alert_recovered','collection_error','collection_recovered','recovery_verified')
			OR ev.kind LIKE '%\_ha\_%' ESCAPE '\')
	) SELECT r.id::text,r.collected_at,r.status,r.metrics_json,r.runtime,
		coalesce((SELECT jsonb_agg(jsonb_build_object('id',q.event_id,'title',q.title,'status',q.status,'phase',q.phase))
			FROM (SELECT * FROM evidence_refs e WHERE e.sample_id=r.id ORDER BY event_id,phase LIMIT 12) q),'[]'::jsonb)
	FROM recent r ORDER BY r.collected_at DESC,r.id DESC`, assetID, from, to, OrganizationFromContext(ctx), event)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := []haPoint{}
	for rows.Next() {
		var sample sampleObservation
		var runtime string
		var data, events []byte
		if err := rows.Scan(&sample.ID, &sample.At, &sample.Status, &data, &runtime, &events); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &sample.Metrics); err != nil {
			return nil, err
		}
		point := haHistoryPoint(runtime, sample)
		point.Events = json.RawMessage(events)
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(points) && i < 100; i++ {
		points[i].Changes = haChanges(points[i+1], points[i])
	}
	return map[string]any{"from": from, "to": to, "truncated": len(points) > 100, "samples": points[:min(len(points), 100)]}, nil
}
