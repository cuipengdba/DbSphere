package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

const liveIntegrationsPath = "/api/live/integrations"

type ObservabilityConfig struct {
	PrometheusURL *url.URL
}

func LoadObservabilityConfig(origin string) (ObservabilityConfig, error) {
	if origin == "" {
		return ObservabilityConfig{}, nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ObservabilityConfig{}, errors.New("DBSPHERE_OBSERVABILITY_URL must be an absolute HTTP origin")
	}
	return ObservabilityConfig{PrometheusURL: u}, nil
}

var prometheusReadinessClient = &http.Client{
	Timeout:       1200 * time.Millisecond,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func readLiveIntegrations(w http.ResponseWriter, r *http.Request, cfg ObservabilityConfig) {
	state := "disabled"
	if cfg.PrometheusURL != nil {
		state = "unavailable"
		endpoint := cfg.PrometheusURL.ResolveReference(&url.URL{Path: "/-/ready"})
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, endpoint.String(), nil)
		if err == nil {
			if response, err := prometheusReadinessClient.Do(request); err == nil {
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					state = "ready"
				}
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	respond(w, http.StatusOK, map[string]string{"prometheus": state})
}

type prometheusAssetReader interface {
	PrometheusAssets(context.Context, int) ([]storepkg.PrometheusAsset, error)
}

var prometheusLabelEscapes = strings.NewReplacer("\x5c", "\x5c\x5c", "\n", "\x5cn", "\x22", "\x5c\x22")

func writeAssetMetrics(w io.Writer, assets []storepkg.PrometheusAsset) {
	labels := make([]string, len(assets))
	for i, a := range assets {
		escape := prometheusLabelEscapes.Replace
		labels[i] = fmt.Sprintf("asset_id=%q,asset_name=\"%s\",organization_id=%q,cluster=\"%s\",engine=%q,runtime=%q",
			a.ID, escape(a.Name), a.Organization, escape(a.Cluster), a.Engine, a.Runtime)
	}
	one := 1.0
	for _, metric := range []struct {
		name, help, kind string
		value            func(storepkg.PrometheusAsset) *float64
	}{
		{"asset_info", "Registered asset identity.", "gauge", func(storepkg.PrometheusAsset) *float64 { return &one }},
		{"asset_last_sample_timestamp_seconds", "Time of the last stored sample.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.SampleAt }},
		{"asset_sample_fresh", "Whether the latest successful collection is fresh.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.Fresh }},
		{"asset_open_events", "Open DbSphere events for the asset.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.OpenEvents }},
		{"asset_connections", "Database connections on the sampled instance.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.Connections }},
		{"asset_max_connections", "Connection limit from the sampled database.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.MaxConnections }},
		{"asset_pg_blocked_sessions", "PostgreSQL sessions with known blockers.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.Blocked }},
		{"asset_pg_replication_lag_bytes", "Observed PostgreSQL WAL replication lag.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.ReplicationLag }},
		{"asset_pg_transactions_total", "PostgreSQL database commits and rollbacks.", "counter", func(a storepkg.PrometheusAsset) *float64 { return a.Transactions }},
		{"asset_linux_cpu_percent", "Linux CPU use observed by the collector.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.LinuxCPU }},
		{"asset_linux_memory_percent", "Linux memory use observed by the collector.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.LinuxRAM }},
		{"asset_k8s_pod_ready", "Pod readiness reported by Kubernetes.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.PodReady }},
	} {
		name := "dbsphere_" + metric.name
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, metric.help, name, metric.kind)
		for i, asset := range assets {
			if value := metric.value(asset); value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) {
				fmt.Fprintf(w, "%s{%s} %g\n", name, labels[i], *value)
			}
		}
	}
}
