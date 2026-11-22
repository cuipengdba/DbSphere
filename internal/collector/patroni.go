package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const patroniMaxBody = 1 << 20

// Patroni reads topology and optional health evidence from the configured endpoint.
func Patroni(ctx context.Context, endpoint string) (map[string]any, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Patroni REST endpoint")
	}
	username, password := "", ""
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	u.User = nil
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	read := func(path string) ([]byte, int, error) {
		target := *u
		target.Path = path
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, 0, errors.New("invalid Patroni REST endpoint")
		}
		if username != "" {
			request.SetBasicAuth(username, password)
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, 0, errors.New("Patroni endpoint unavailable")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, response.StatusCode, fmt.Errorf("Patroni endpoint returned HTTP %d", response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, patroniMaxBody+1))
		if err != nil || len(body) > patroniMaxBody {
			return nil, response.StatusCode, errors.New("Patroni response is too large or unreadable")
		}
		return body, response.StatusCode, nil
	}
	body, _, err := read("/cluster")
	if err != nil {
		return nil, err
	}
	var cluster struct {
		Members []struct {
			Name      string          `json:"name"`
			Role      string          `json:"role"`
			State     string          `json:"state"`
			Host      string          `json:"host"`
			Port      int             `json:"port"`
			Timeline  int             `json:"timeline"`
			Lag       json.RawMessage `json:"lag"`
			ReplayLag json.RawMessage `json:"replay_lag"`
			Tags      map[string]any  `json:"tags"`
		} `json:"members"`
		Pause bool `json:"pause"`
	}
	if json.Unmarshal(body, &cluster) != nil || cluster.Members == nil || len(cluster.Members) > 100 {
		return nil, errors.New("invalid Patroni cluster response")
	}
	members := make([]map[string]any, 0, len(cluster.Members))
	leaders, replicas := 0, 0
	for _, member := range cluster.Members {
		if strings.TrimSpace(member.Name) == "" || len(member.Name) > 150 || len(member.Role) > 40 ||
			len(member.State) > 60 || len(member.Host) > 255 || member.Port < 0 || member.Port > 65535 {
			return nil, errors.New("invalid Patroni cluster member")
		}
		if member.Role == "leader" || member.Role == "standby_leader" || member.Role == "primary" {
			leaders++
		} else {
			replicas++
		}
		item := map[string]any{"name": member.Name, "role": member.Role, "state": member.State,
			"host": member.Host, "port": member.Port, "timeline": member.Timeline}
		if lag, ok := patroniLag(member.Lag); ok {
			item["lagBytes"] = lag
		}
		if lag, ok := patroniLag(member.ReplayLag); ok {
			item["replayLagBytes"] = lag
		}
		for _, tag := range []string{"nosync", "nofailover", "noloadbalance"} {
			if value, ok := member.Tags[tag].(bool); ok {
				item[tag] = value
			}
		}
		members = append(members, item)
	}
	var state, history, heartbeat map[string]any
	var wait sync.WaitGroup
	wait.Add(3)
	go func() {
		defer wait.Done()
		body, _, _ := read("/patroni")
		state = patroniNodeState(body, time.Now().UTC())
	}()
	go func() {
		defer wait.Done()
		body, _, _ := read("/history")
		history = patroniTimelineHistory(body)
	}()
	go func() {
		defer wait.Done()
		_, status, err := read("/liveness")
		heartbeat = map[string]any{"status": "unavailable", "source": "Patroni GET /liveness"}
		if status == http.StatusOK && err == nil {
			heartbeat["status"] = "healthy"
		} else if status == http.StatusServiceUnavailable {
			heartbeat["status"] = "unhealthy"
		}
	}()
	wait.Wait()
	return map[string]any{"status": "ok", "source": "Patroni GET /cluster", "observedAt": time.Now().UTC(),
		"leaderCount": leaders, "replicaCount": replicas, "paused": cluster.Pause, "members": members,
		"nodeState": state, "history": history, "heartbeat": heartbeat}, nil
}

func patroniNodeState(body []byte, at time.Time) map[string]any {
	result := map[string]any{"status": "unavailable", "source": "Patroni GET /patroni", "observedAt": at}
	var row struct {
		DCSLastSeen int64  `json:"dcs_last_seen"`
		Timeline    int    `json:"timeline"`
		Role        string `json:"role"`
		Unlocked    bool   `json:"cluster_unlocked"`
		Failsafe    bool   `json:"failsafe_mode_is_active"`
		Patroni     struct {
			Name string `json:"name"`
		} `json:"patroni"`
		Replication []struct {
			Name     string `json:"application_name"`
			State    string `json:"state"`
			Sync     string `json:"sync_state"`
			Priority int    `json:"sync_priority"`
		} `json:"replication"`
	}
	if json.Unmarshal(body, &row) != nil || row.Patroni.Name == "" || len(row.Patroni.Name) > 150 {
		return result
	}
	result["status"], result["name"] = "ok", row.Patroni.Name
	result["timeline"], result["role"] = row.Timeline, row.Role
	result["unlocked"], result["failsafe"] = row.Unlocked, row.Failsafe
	if row.DCSLastSeen > 0 {
		lastSeen := time.Unix(row.DCSLastSeen, 0).UTC()
		result["dcsLastSeen"] = lastSeen
		if !lastSeen.After(at.Add(time.Second)) {
			result["dcsAgeSeconds"] = max(0, at.Sub(lastSeen).Seconds())
		}
	}
	links := []map[string]any{}
	for _, link := range row.Replication {
		if link.Name != "" && len(link.Name) <= 150 && len(link.State) <= 40 && len(link.Sync) <= 40 && len(links) < 50 {
			links = append(links, map[string]any{"name": link.Name, "state": link.State, "syncState": link.Sync, "syncPriority": link.Priority})
		}
	}
	result["replication"] = links
	return result
}

func patroniTimelineHistory(body []byte) map[string]any {
	result := map[string]any{"status": "unavailable", "source": "Patroni GET /history"}
	var entries [][]json.RawMessage
	if json.Unmarshal(body, &entries) != nil || entries == nil {
		return result
	}
	if len(entries) > 20 {
		entries = entries[len(entries)-20:]
		result["truncated"] = true
	}
	rows := []map[string]any{}
	for _, fields := range entries {
		var timeline int
		var lsn json.Number
		var reason, timestamp, leader string
		if len(fields) < 4 || json.Unmarshal(fields[0], &timeline) != nil || timeline <= 0 ||
			json.Unmarshal(fields[3], &timestamp) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			continue
		}
		row := map[string]any{"previousTimeline": timeline, "switchedAt": at.UTC()}
		if json.Unmarshal(fields[1], &lsn) == nil {
			if _, err := strconv.ParseUint(lsn.String(), 10, 64); err == nil {
				row["switchLSN"] = lsn.String()
			}
		}
		if json.Unmarshal(fields[2], &reason) == nil && len(reason) <= 256 {
			row["reason"] = reason
		}
		if len(fields) >= 5 && json.Unmarshal(fields[4], &leader) == nil && len(leader) <= 150 {
			row["newLeader"] = leader
		}
		rows = append(rows, row)
	}
	result["status"], result["entries"] = "ok", rows
	if len(rows) < len(entries) {
		result["invalidRows"] = len(entries) - len(rows)
	}
	return result
}

func patroniLag(raw json.RawMessage) (float64, bool) {
	var value float64
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &value) != nil || value < 0 {
		return 0, false
	}
	return value, true
}
