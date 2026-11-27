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
	GrafanaURL    *url.URL
}

func LoadObservabilityConfig(prometheus, grafana string) (ObservabilityConfig, error) {
	promURL, err := observabilityOrigin(prometheus)
	if err != nil {
		return ObservabilityConfig{}, fmt.Errorf("DBSPHERE_OBSERVABILITY_URL: %w", err)
	}
	grafanaURL, err := observabilityOrigin(grafana)
	if err != nil {
		return ObservabilityConfig{}, fmt.Errorf("DBSPHERE_GRAFANA_URL: %w", err)
	}
	return ObservabilityConfig{PrometheusURL: promURL, GrafanaURL: grafanaURL}, nil
}

func observabilityOrigin(origin string) (*url.URL, error) {
	if origin == "" {
		return nil, nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must be an absolute HTTP origin")
	}
	return u, nil
}

var prometheusReadinessClient = &http.Client{
	Timeout:       1200 * time.Millisecond,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func readLiveIntegrations(w http.ResponseWriter, r *http.Request, cfg ObservabilityConfig, platformAdmin bool) {
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
	respond(w, http.StatusOK, map[string]any{"prometheus": state, "grafana": platformAdmin && cfg.GrafanaURL != nil})
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
		{"collector_queue_seconds", "Wait from the due time to the last collection lease.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.CollectorQueue }},
		{"collector_query_seconds", "Target query duration during the last scheduled collection.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.CollectorQuery }},
		{"collector_store_seconds", "Time spent writing the last scheduled sample and evidence before commit.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.CollectorStore }},
		{"collector_permission_gaps", "Permission-limited signals in the last scheduled adapter report.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.CollectorGapSignals }},
		{"collector_isolated", "Whether this collection task is assigned to the isolated lane.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.CollectorIsolated }},
		{"collector_backoff", "Whether this collection task is waiting for a retry.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.CollectorBackoff }},
		{"asset_connections", "Database connections on the sampled instance.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.Connections }},
		{"asset_max_connections", "Connection limit from the sampled database.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.MaxConnections }},
		{"asset_pg_blocked_sessions", "PostgreSQL sessions with known blockers.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.Blocked }},
		{"asset_pg_waiting_sessions", "PostgreSQL active client sessions with a wait event.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.PGWaiting }},
		{"asset_pg_sql_fingerprints_observed", "Fingerprints available in the bounded pg_stat_statements sample.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.PGSQLFingerprints }},
		{"asset_pg_replication_lag_bytes", "Observed PostgreSQL WAL replication lag.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.ReplicationLag }},
		{"asset_pg_transactions_total", "PostgreSQL database commits and rollbacks.", "counter", func(a storepkg.PrometheusAsset) *float64 { return a.Transactions }},
		{"asset_linux_cpu_percent", "Linux CPU use observed by the collector.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.LinuxCPU }},
		{"asset_linux_memory_percent", "Linux memory use observed by the collector.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.LinuxRAM }},
		{"asset_linux_network_receive_bytes_per_second", "Linux network receive rate from the collector interval.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.LinuxRX }},
		{"asset_linux_network_transmit_bytes_per_second", "Linux network transmit rate from the collector interval.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.LinuxTX }},
		{"asset_linux_disk_read_bytes_per_second", "Linux disk read rate from the collector interval.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.LinuxDiskRead }},
		{"asset_linux_disk_write_bytes_per_second", "Linux disk write rate from the collector interval.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.LinuxDiskWrite }},
		{"asset_k8s_pod_ready", "Pod readiness reported by Kubernetes.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.PodReady }},
		{"asset_k8s_pod_cpu_cores", "Pod CPU cores reported by the Kubernetes resource metrics API.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.PodCPU }},
		{"asset_k8s_pod_memory_bytes", "Pod memory bytes reported by the Kubernetes resource metrics API.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.PodMemory }},
		{"asset_mysql_threads_running", "MySQL running threads from Performance Schema.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.MySQLRunning }},
		{"asset_mysql_lock_waits_observed", "MySQL InnoDB lock waits in the bounded sample.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.MySQLLockWaits }},
		{"asset_mysql_buffer_hit_percent", "MySQL InnoDB buffer pool hit percent over adjacent samples.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.MySQLBufferHit }},
		{"asset_mysql_buffer_dirty_pages", "MySQL InnoDB dirty buffer pool pages.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.MySQLDirtyPages }},
		{"asset_mysql_questions_per_second", "MySQL Questions counter rate over adjacent samples.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.MySQLQuestions }},
		{"asset_dm8_sessions", "Dameng sessions excluding the probe session.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.DM8Sessions }},
		{"asset_dm8_active_sessions", "Dameng active sessions excluding the probe session.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.DM8Active }},
		{"asset_dm8_transactions", "Dameng active transactions excluding the probe session.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.DM8Transactions }},
		{"asset_dm8_lock_waits_observed", "Dameng transaction lock waits in the bounded sample.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.DM8Waits }},
		{"asset_dm8_standby_replay_gap_lsn", "Dameng standby position difference between flushed and replayed REDO.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.DM8ReplayGap }},
		{"asset_dm8_data_watch_switches_total", "Role changes reported by the Dameng watcher group.", "counter", func(a storepkg.PrometheusAsset) *float64 { return a.DM8Switches }},
		{"asset_kingbase_sessions", "Kingbase client sessions in the monitored database.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.KingbaseSessions }},
		{"asset_kingbase_active_sessions", "Active Kingbase client sessions in the monitored database.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.KingbaseActive }},
		{"asset_kingbase_transactions", "Kingbase sessions with a transaction.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.KingbaseTransactions }},
		{"asset_kingbase_lock_pairs_observed", "Kingbase waiting and blocking session pairs in the bounded sample.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.KingbaseWaits }},
		{"asset_kingbase_database_bytes", "Size of the monitored Kingbase database.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.KingbaseSize }},
		{"asset_kingbase_max_replay_lag_bytes", "Kingbase WAL position difference to the most delayed visible replica.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.KingbaseLag }},
		{"asset_kingbase_standby_replay_gap_bytes", "Kingbase standby difference between WAL received and replayed.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.KingbaseStandbyLag }},
		{"asset_kingbase_archiver_failures_total", "Failed Kingbase WAL archival attempts since statistics reset.", "counter", func(a storepkg.PrometheusAsset) *float64 { return a.KingbaseArchiveFailures }},
		{"asset_opengauss_sessions", "openGauss sessions in the monitored database.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.OpenGaussSessions }},
		{"asset_opengauss_active_sessions", "Active openGauss sessions in the monitored database.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.OpenGaussActive }},
		{"asset_opengauss_transactions", "openGauss sessions with a transaction.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.OpenGaussTransactions }},
		{"asset_opengauss_lock_waits_observed", "openGauss heavyweight lock waits in the bounded sample.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.OpenGaussWaits }},
		{"asset_opengauss_database_bytes", "Size of the monitored openGauss database.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.OpenGaussSize }},
		{"asset_redis_ops_per_second", "Redis node operations per second from INFO stats.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.RedisOps }},
		{"asset_redis_clients", "Redis node connected clients.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.RedisClients }},
		{"asset_redis_memory_bytes", "Redis reported memory usage in bytes.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.RedisMemory }},
		{"asset_redis_hit_percent", "Redis hit percentage between two samples from the same process.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.RedisHit }},
		{"asset_redis_interval_evictions", "Keys evicted between two Redis samples from the same process.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.RedisEvicted }},
		{"asset_redis_interval_rejected_connections", "Connections rejected between two Redis samples.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.RedisRejected }},
		{"asset_redis_observation_latency_milliseconds", "Collector elapsed time including Redis connection and INFO server.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.RedisLatency }},
		{"asset_redis_cluster_slots_ok", "Healthy slots as visible to the sampled Redis Cluster node.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.RedisSlots }},
		{"asset_etcd_has_leader", "Whether the observed etcd member reports a leader.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.EtcdHasLeader }},
		{"asset_etcd_leader_changes_total", "Leader changes observed by the etcd member.", "counter", func(a storepkg.PrometheusAsset) *float64 { return a.EtcdElections }},
		{"asset_etcd_proposals_pending", "Pending proposals as reported by the etcd member.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.EtcdPending }},
		{"asset_etcd_backend_size_bytes", "Allocated etcd backend database size.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.EtcdSize }},
		{"asset_etcd_backend_quota_bytes", "Configured etcd backend quota.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.EtcdQuota }},
		{"asset_etcd_wal_ms", "Mean etcd WAL fsync latency between two samples.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.EtcdWAL }},
		{"asset_etcd_backend_commit_ms", "Mean etcd backend commit latency between two samples.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.EtcdCommit }},
		{"asset_etcd_kube_apiserver_ms", "Mean API server etcd request latency between two samples.", "gauge", func(a storepkg.PrometheusAsset) *float64 { return a.EtcdAPILatency }},
	} {
		name := "dbsphere_" + metric.name
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, metric.help, name, metric.kind)
		for i, asset := range assets {
			if value := metric.value(asset); value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) {
				fmt.Fprintf(w, "%s{%s} %g\n", name, labels[i], *value)
			}
		}
	}
	for i, asset := range assets {
		writeContainerMetrics(w, labels[i], asset.Container)
	}
}
