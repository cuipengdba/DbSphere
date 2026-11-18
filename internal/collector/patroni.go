package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const patroniMaxBody = 1 << 20

// Patroni reads only GET /cluster. It never invokes REST write endpoints.
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
	u.Path = "/cluster"
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid Patroni REST endpoint")
	}
	if username != "" {
		request.SetBasicAuth(username, password)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 5 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("Patroni cluster endpoint unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Patroni cluster endpoint returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, patroniMaxBody+1))
	if err != nil || len(body) > patroniMaxBody {
		return nil, errors.New("Patroni cluster response is too large or unreadable")
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
		members = append(members, item)
	}
	return map[string]any{"status": "ok", "source": "Patroni GET /cluster", "observedAt": time.Now().UTC(),
		"leaderCount": leaders, "replicaCount": replicas, "paused": cluster.Pause, "members": members}, nil
}

func patroniLag(raw json.RawMessage) (float64, bool) {
	var value float64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value < 0 {
		return 0, false
	}
	return value, true
}
