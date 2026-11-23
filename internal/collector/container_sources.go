package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type StorageSource struct {
	URL          string `json:"url"`
	Job          string `json:"job,omitempty"`
	NodeExporter string `json:"nodeExporter,omitempty"`
}

type DockerStorageBinding struct {
	Host        string `json:"host"`
	ContainerID string `json:"containerId"`
}

type ContainerStorageConfig struct {
	Nodes                       map[string]StorageSource        `json:"nodes"`
	Docker                      map[string]DockerStorageBinding `json:"docker"`
	Primary                     map[string]string               `json:"primary"`
	AgentToken, PrometheusToken string                          `json:"-"`
}

var storageAssetID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

func splitStorageEndpoint(endpoint string) (string, int, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", 0, err
	}
	value, err := strconv.Atoi(port)
	return host, value, err
}

func (source StorageSource) Linux(ctx context.Context) (map[string]any, error) {
	host, port, err := splitStorageEndpoint(source.NodeExporter)
	if err != nil {
		return nil, err
	}
	values, err := Linux(ctx, host, port)
	if err == nil {
		values["endpoint"] = source.NodeExporter
	}
	return values, err
}

func ParseContainerStorageConfig(raw, agentToken, promToken string) (ContainerStorageConfig, error) {
	var cfg ContainerStorageConfig
	if raw == "" {
		return cfg, nil
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&cfg) != nil || d.Decode(new(any)) != io.EOF || len(raw) > 262144 {
		return cfg, fmt.Errorf("invalid container storage bindings JSON")
	}
	cfg.AgentToken, cfg.PrometheusToken = agentToken, promToken
	for node, source := range cfg.Nodes {
		parsed, err := url.Parse(source.URL)
		if node == "" || len(node) > 305 || err != nil || parsed.Hostname() == "" || parsed.User != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" ||
			len(source.Job) > 200 {
			return cfg, fmt.Errorf("invalid container storage source URL or node")
		}
		if source.Job == "" && len(agentToken) < 24 {
			return cfg, fmt.Errorf("container cgroup source requires DBSPHERE_CONTAINER_AGENT_TOKEN")
		}
		if source.NodeExporter != "" {
			if _, port, err := splitStorageEndpoint(source.NodeExporter); err != nil || port < 1 || port > 65535 {
				return cfg, fmt.Errorf("invalid container storage nodeExporter endpoint")
			}
		}
	}
	for asset, binding := range cfg.Docker {
		if !storageAssetID.MatchString(asset) || !containerID.MatchString(binding.ContainerID) || cfg.Nodes[binding.Host].URL == "" {
			return cfg, fmt.Errorf("invalid Docker asset, runtime ID, or node source")
		}
	}
	for asset, name := range cfg.Primary {
		if !storageAssetID.MatchString(asset) || len(name) == 0 || len(name) > 63 {
			return cfg, fmt.Errorf("invalid Kubernetes primary container binding")
		}
	}
	return cfg, nil
}

var containerHTTP = &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (cfg ContainerStorageConfig) Poll(ctx context.Context, source StorageSource, id string) ContainerPoll {
	failed := ContainerPoll{Status: "unavailable", ID: id, ObservedAt: time.Now().UTC(), Source: "cgroup"}
	if source.URL == "" || !containerID.MatchString(id) {
		failed.Status = "unbound"
		return failed
	}
	if source.Job != "" {
		return cfg.pollPrometheus(ctx, source, id)
	}
	u, _ := url.Parse(source.URL)
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/container/" + id
	status, err := readContainerJSON(ctx, u, cfg.AgentToken, &failed)
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		failed.Status = "denied"
	} else if err != nil || failed.ID != id || failed.ObservedAt.IsZero() ||
		time.Since(failed.ObservedAt) > time.Minute || time.Until(failed.ObservedAt) > 10*time.Second {
		return ContainerPoll{ID: id, Status: "unavailable", Source: "cgroup", ObservedAt: time.Now().UTC()}
	}
	return failed
}

// Prometheus sources use a container-specific cAdvisor job or normalized
// recording rules when the exporter cannot report both LRU lists directly.
func (cfg ContainerStorageConfig) pollPrometheus(ctx context.Context, source StorageSource, id string) ContainerPoll {
	p := ContainerPoll{Status: "unavailable", ID: id, Source: "prometheus", ObservedAt: time.Now().UTC(),
		Memory: map[string]uint64{}, Devices: map[string]map[string]uint64{}, States: map[string]string{}}
	names := map[string]string{
		"container_fs_reads_bytes_total": "rbytes", "container_fs_writes_bytes_total": "wbytes",
		"container_fs_reads_total": "rios", "container_fs_writes_total": "wios",
		"dbsphere_source_container_active_file_bytes": "active_file", "dbsphere_source_container_inactive_file_bytes": "inactive_file",
		"dbsphere_source_container_refault_file_total": "workingset_refault_file", "container_memory_cache": "file",
		"container_last_seen": "clock", "container_start_time_seconds": "started",
	}
	namePattern := make([]string, 0, len(names))
	for name := range names {
		namePattern = append(namePattern, name)
	}
	idPattern := `(.*/)?(docker-|cri-containerd-|crio-|libpod-)?` + id + `(\.scope)?`
	query := `{job=` + strconv.Quote(source.Job) + `,id=~` + strconv.Quote(idPattern) + `,__name__=~` + strconv.Quote(strings.Join(namePattern, "|")) + `}`
	u, _ := url.Parse(source.URL)
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/query"
	u.RawQuery = url.Values{"query": {query}}.Encode()
	var result struct {
		Status string
		Data   struct {
			Result []struct {
				Metric map[string]string
				Value  []json.RawMessage
			}
		}
	}
	status, err := readContainerJSON(ctx, u, cfg.PrometheusToken, &result)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		p.Status = "denied"
		return p
	}
	if err != nil || result.Status != "success" {
		return p
	}
	if len(result.Data.Result) == 0 {
		p.Status = "not_found"
		return p
	}
	seen := map[string]bool{}
	clock := false
	for _, row := range result.Data.Result {
		if len(row.Value) != 2 {
			continue
		}
		key, device := names[row.Metric["__name__"]], row.Metric["device"]
		if key == "" || len(device) > 128 {
			continue
		}
		var raw string
		if json.Unmarshal(row.Value[1], &raw) != nil {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || !(value >= 0 && value < math.Exp2(64)) {
			continue
		}
		series := key + "\x00" + device
		if seen[series] {
			p.Status = "ambiguous"
			return p
		}
		seen[series] = true
		switch key {
		case "clock":
			p.ObservedAt = time.Unix(0, int64(value*1e9)).UTC()
			clock = true
		case "started":
			p.Generation = raw
		case "rbytes", "wbytes", "rios", "wios":
			if device == "" {
				device = "aggregate"
			}
			if p.Devices[device] == nil && len(p.Devices) < 64 {
				p.Devices[device] = map[string]uint64{}
			}
			if counters := p.Devices[device]; counters != nil {
				counters[key] = uint64(value)
			}
		default:
			p.Memory[key] = uint64(value)
		}
	}
	if !clock || time.Since(p.ObservedAt) > time.Minute || time.Until(p.ObservedAt) > 10*time.Second {
		p.Status = "stale"
		return p
	}
	if len(p.Devices)+len(p.Memory) == 0 {
		p.Status = "unsupported"
		return p
	}
	p.Status = "ok"
	return p
}

func readContainerJSON(ctx context.Context, target *url.URL, token string, out any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := containerHTTP.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, fmt.Errorf("source returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 512*1024+1))
	if err != nil || len(data) > 512*1024 {
		return response.StatusCode, fmt.Errorf("invalid source response size")
	}
	return response.StatusCode, json.Unmarshal(data, out)
}
