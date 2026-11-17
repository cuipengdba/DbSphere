package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var pvcName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// PVCUsage reads only kubelet_volume_stats_used_bytes for a mapped PVC.
func PVCUsage(ctx context.Context, namespace, name string) map[string]any {
	endpoint := os.Getenv("DBSPHERE_PROMETHEUS_URL")
	if endpoint == "" {
		return map[string]any{"status": "not_configured", "reason": "Prometheus PVC usage endpoint is not configured"}
	}
	if !pvcName.MatchString(namespace) || !pvcName.MatchString(name) || len(namespace) > 63 || len(name) > 253 {
		return map[string]any{"status": "error", "reason": "Invalid Kubernetes PVC scope"}
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return map[string]any{"status": "error", "reason": "Invalid Prometheus URL"}
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/") + "/api/v1/query"
	parsed.RawQuery = url.Values{"query": {fmt.Sprintf(`kubelet_volume_stats_used_bytes{namespace="%s",persistentvolumeclaim="%s"}`, namespace, name)}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return map[string]any{"status": "error", "reason": "Prometheus request could not be created"}
	}
	if token := os.Getenv("DBSPHERE_PROMETHEUS_TOKEN"); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 4 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return map[string]any{"status": "error", "reason": "Prometheus request failed"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return map[string]any{"status": "error", "reason": fmt.Sprintf("Prometheus HTTP %d", response.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return map[string]any{"status": "error", "reason": "Prometheus response exceeds 1 MiB or could not be read"}
	}
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Status != "success" || payload.Data.ResultType != "vector" {
		return map[string]any{"status": "error", "reason": "Prometheus query did not return a vector"}
	}
	best := -1.0
	observed := time.Time{}
	for _, item := range payload.Data.Result {
		if len(item.Value) != 2 {
			continue
		}
		used, err := strconv.ParseFloat(fmt.Sprint(item.Value[1]), 64)
		if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 {
			continue
		}
		if used > best {
			best = used
		}
		if timestamp, ok := item.Value[0].(float64); ok && !math.IsNaN(timestamp) && !math.IsInf(timestamp, 0) {
			at := time.Unix(int64(timestamp), 0).UTC()
			if at.After(observed) {
				observed = at
			}
		}
	}
	if best < 0 {
		return map[string]any{"status": "unavailable", "reason": "kubelet PVC used bytes metric is absent"}
	}
	result := map[string]any{"status": "ok", "source": "Prometheus kubelet_volume_stats_used_bytes", "usedBytes": best}
	if !observed.IsZero() {
		result["observedAt"] = observed
	}
	return result
}

func KubernetesQuantityBytes(value string) (float64, error) {
	if value == "" {
		return 0, errors.New("empty Kubernetes quantity")
	}
	factor := 1.0
	for suffix, scale := range map[string]float64{"Ki": 1024, "Mi": 1024 * 1024, "Gi": 1024 * 1024 * 1024,
		"Ti": math.Pow(1024, 4), "Pi": math.Pow(1024, 5), "K": 1e3, "M": 1e6, "G": 1e9,
		"T": 1e12, "P": 1e15} {
		if strings.HasSuffix(value, suffix) {
			value = strings.TrimSuffix(value, suffix)
			factor = scale
			break
		}
	}
	bytes, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(bytes) || math.IsInf(bytes, 0) || bytes <= 0 {
		return 0, errors.New("invalid Kubernetes quantity")
	}
	return bytes * factor, nil
}
