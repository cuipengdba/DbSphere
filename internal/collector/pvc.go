package collector

import (
	"context"
	"crypto/sha256"
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

type pvcMetricPoint struct {
	Value float64
	At    time.Time
}

func queryPVCMetric(ctx context.Context, client *http.Client, endpoint url.URL,
	metric, namespace, name string, since time.Time) (map[string]pvcMetricPoint, map[string]any) {
	selector := fmt.Sprintf(`%s{namespace="%s",persistentvolumeclaim="%s"}`, metric, namespace, name)
	query := selector
	if !since.IsZero() {
		cutoff := time.Now().UTC().Add(-5 * time.Minute)
		if since.After(cutoff) {
			cutoff = since
		}
		query = fmt.Sprintf(`%s and (timestamp(%s) >= %.3f)`, selector, selector, float64(cutoff.UnixMilli())/1000)
	}
	endpoint.RawQuery = url.Values{"query": {query}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, map[string]any{"status": "error", "reason": "Prometheus request could not be created"}
	}
	if token := os.Getenv("DBSPHERE_PROMETHEUS_TOKEN"); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, map[string]any{"status": "error", "reason": "Prometheus request failed"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, map[string]any{"status": "error", "reason": fmt.Sprintf("Prometheus HTTP %d", response.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, map[string]any{"status": "error", "reason": "Prometheus response exceeds 1 MiB or could not be read"}
	}
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Status != "success" || payload.Data.ResultType != "vector" {
		return nil, map[string]any{"status": "error", "reason": "Prometheus query did not return a vector"}
	}
	points := map[string]pvcMetricPoint{}
	for _, item := range payload.Data.Result {
		if len(item.Value) != 2 || item.Metric["namespace"] != namespace ||
			item.Metric["persistentvolumeclaim"] != name {
			continue
		}
		value, valueErr := strconv.ParseFloat(fmt.Sprint(item.Value[1]), 64)
		atNumber, atOK := item.Value[0].(float64)
		if valueErr != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 ||
			!atOK || math.IsNaN(atNumber) || math.IsInf(atNumber, 0) {
			continue
		}
		at := time.Unix(int64(atNumber), 0).UTC()
		if at.Before(time.Now().UTC().Add(-5 * time.Minute)) {
			continue
		}
		delete(item.Metric, "__name__")
		key, _ := json.Marshal(item.Metric)
		if prior, exists := points[string(key)]; !exists || at.After(prior.At) {
			points[string(key)] = pvcMetricPoint{Value: value, At: at}
		}
	}
	if len(points) == 0 {
		return nil, map[string]any{"status": "unavailable", "reason": metric + " is absent or stale"}
	}
	return points, nil
}

// PVCUsage compares used and capacity from the same kubelet volume series.
// The PVC request size may differ from the driver's actual filesystem capacity.
func PVCUsage(ctx context.Context, namespace, name string) map[string]any {
	return pvcUsageSince(ctx, namespace, name, time.Time{})
}

func pvcUsageSince(ctx context.Context, namespace, name string, since time.Time) map[string]any {
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
	client := &http.Client{Timeout: 4 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	used, problem := queryPVCMetric(ctx, client, *parsed, "kubelet_volume_stats_used_bytes", namespace, name, since)
	if problem != nil {
		return problem
	}
	capacity, problem := queryPVCMetric(ctx, client, *parsed, "kubelet_volume_stats_capacity_bytes", namespace, name, since)
	if problem != nil {
		return problem
	}
	best := -1.0
	bestSeries := ""
	result := map[string]any{}
	for labels, usage := range used {
		size, exists := capacity[labels]
		if !exists || size.Value <= 0 || math.Abs(usage.At.Sub(size.At).Seconds()) > 300 {
			continue
		}
		percent := usage.Value / size.Value * 100
		series := fmt.Sprintf("%x", sha256.Sum256([]byte(labels)))
		if percent > best || (percent == best && series < bestSeries) {
			best = percent
			bestSeries = series
			result = map[string]any{"status": "ok", "source": "Prometheus kubelet volume stats",
				"usedBytes": usage.Value, "capacityBytes": size.Value,
				"usedPercent": percent, "observedAt": usage.At, "seriesKey": series}
		}
	}
	if best < 0 {
		return map[string]any{"status": "unavailable", "reason": "matching kubelet PVC used/capacity metrics are absent"}
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
