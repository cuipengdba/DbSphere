package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
)

var ErrRoutingSettings = errors.New("入口地址、代理类型或期望角色无效")

type RoutingSettings struct {
	Kind            string `json:"kind"`
	Entry           string `json:"entry"`
	ExpectedRole    string `json:"expectedRole"`
	MatchDirect     bool   `json:"matchDirect"`
	HAProxyBackend  string `json:"haproxyBackend"`
	StatsConfigured bool   `json:"statsConfigured"`
}

type RoutingInput struct {
	RoutingSettings
	HAProxyStatsURL string `json:"haproxyStatsUrl"`
}

func migrateRouting(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS asset_route_settings (
		asset_id uuid PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
		kind text NOT NULL, entry text NOT NULL, expected_role text NOT NULL,
		match_direct boolean NOT NULL, haproxy_backend text NOT NULL, haproxy_stats_enc text,
		updated_at timestamptz NOT NULL DEFAULT now()
	);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(39) ON CONFLICT DO NOTHING`)
	return err
}

func (s *Postgres) ReadRouting(ctx context.Context, id string) (RoutingSettings, error) {
	var settings RoutingSettings
	err := s.pool.QueryRow(ctx, `SELECT coalesce(r.kind,''),coalesce(r.entry,''),coalesce(r.expected_role,''),
		coalesce(r.match_direct,false),coalesce(r.haproxy_backend,''),r.haproxy_stats_enc IS NOT NULL
		FROM assets a LEFT JOIN asset_route_settings r ON r.asset_id=a.id WHERE a.id=$1`, id).
		Scan(&settings.Kind, &settings.Entry, &settings.ExpectedRole, &settings.MatchDirect,
			&settings.HAProxyBackend, &settings.StatsConfigured)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrAssetNotFound
	}
	return settings, err
}

func (s *Postgres) SetRouting(ctx context.Context, id string, input RoutingInput, key []byte) (RoutingSettings, error) {
	settings := input.RoutingSettings
	if settings.Kind != "" {
		host, port, err := assets.ParsePoolerEndpoint(strings.TrimSpace(settings.Entry))
		if err != nil || (settings.Kind != "pgbouncer" && settings.Kind != "haproxy" && settings.Kind != "k8s_service") ||
			(settings.ExpectedRole != "" && settings.ExpectedRole != "primary" && settings.ExpectedRole != "standby") {
			return RoutingSettings{}, ErrRoutingSettings
		}
		settings.Entry = net.JoinHostPort(host, strconv.Itoa(port))
	}
	var encrypted *string
	if settings.Kind == "haproxy" {
		settings.HAProxyBackend = strings.TrimSpace(settings.HAProxyBackend)
		if len(settings.HAProxyBackend) > 128 || strings.ContainsAny(settings.HAProxyBackend, "\r\n\x00") {
			return RoutingSettings{}, ErrRoutingSettings
		}
		if input.HAProxyStatsURL != "" {
			if _, err := collector.HAProxyStatsURL(input.HAProxyStatsURL); err != nil || settings.HAProxyBackend == "" {
				return RoutingSettings{}, ErrRoutingSettings
			}
			value, err := security.Encrypt(key, input.HAProxyStatsURL)
			if err != nil {
				return RoutingSettings{}, err
			}
			encrypted = &value
		}
	} else {
		settings.HAProxyBackend = ""
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RoutingSettings{}, err
	}
	defer tx.Rollback(ctx)
	var runtime string
	if err := tx.QueryRow(ctx, `SELECT runtime FROM assets WHERE id=$1 FOR UPDATE`, id).Scan(&runtime); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrAssetNotFound
		}
		return RoutingSettings{}, err
	}
	if settings.Kind == "k8s_service" && runtime != "k8s" {
		return RoutingSettings{}, ErrRoutingSettings
	}
	if settings.Kind == "" {
		_, err = tx.Exec(ctx, `DELETE FROM asset_route_settings WHERE asset_id=$1`, id)
		settings = RoutingSettings{}
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO asset_route_settings(asset_id,kind,entry,expected_role,match_direct,
			haproxy_backend,haproxy_stats_enc) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(asset_id) DO UPDATE SET kind=excluded.kind,entry=excluded.entry,expected_role=excluded.expected_role,
			match_direct=excluded.match_direct,haproxy_backend=excluded.haproxy_backend,updated_at=now(),
			haproxy_stats_enc=CASE WHEN excluded.kind='haproxy' AND excluded.haproxy_backend<>''
				THEN coalesce(excluded.haproxy_stats_enc,asset_route_settings.haproxy_stats_enc) ELSE NULL END
			RETURNING haproxy_stats_enc IS NOT NULL`, id, settings.Kind, settings.Entry, settings.ExpectedRole,
			settings.MatchDirect, settings.HAProxyBackend, encrypted).Scan(&settings.StatsConfigured)
	}
	if err != nil {
		return RoutingSettings{}, err
	}
	return settings, tx.Commit(ctx)
}

func routeAssessment(settings RoutingSettings, probe collector.RouteProbe, metrics, kube, pooler, haproxy map[string]any,
	monitorUser string) map[string]any {
	result := map[string]any{
		"kind": settings.Kind, "entry": settings.Entry, "probe": probe, "expectedRole": settings.ExpectedRole,
		"matchDirect": settings.MatchDirect,
	}
	directEndpoint := object(metrics["postgresEndpoint"])
	directAt, _ := metrics["postgresStartedAt"].(time.Time)
	directAddress := stringValue(directEndpoint["address"])
	directPort, _ := directEndpoint["port"].(int)
	if directAddress != "" && directPort > 0 {
		result["direct"] = map[string]any{"backend": net.JoinHostPort(directAddress, strconv.Itoa(directPort)),
			"role": metrics["role"], "source": directEndpoint["source"]}
	}
	different := probe.Backend != "" && directAddress != "" && directPort > 0 &&
		probe.Backend != net.JoinHostPort(directAddress, strconv.Itoa(directPort)) &&
		!directAt.IsZero() && !probe.StartedAt.IsZero() && !directAt.Equal(probe.StartedAt)
	roleMismatch := probe.Status == "ok" && settings.ExpectedRole != "" && probe.Role != settings.ExpectedRole
	databaseMismatch := probe.Status == "ok" && stringValue(metrics["databaseName"]) != "" &&
		probe.Database != stringValue(metrics["databaseName"])
	poolExhausted := false
	if settings.Kind == "pgbouncer" && pooler["status"] == "ok" {
		result["pooler"] = pooler
		rows, _ := pooler["pools"].([]map[string]any)
		for _, row := range rows {
			waiting, _ := numeric(row["clientWaiting"])
			active, _ := numeric(row["serverActive"])
			idle, idleObserved := numeric(row["serverIdle"])
			if row["user"] == monitorUser && waiting > 0 && active > 0 && idleObserved && idle == 0 {
				poolExhausted = true
			}
		}
	}
	if haproxy != nil {
		result["haproxy"] = haproxy
	}
	var service map[string]any
	if settings.Kind == "k8s_service" {
		service = selectedServiceRoute(settings.Entry, kube)
		if service != nil {
			result["service"] = service
		}
	}
	status := probe.Status
	switch {
	case roleMismatch || databaseMismatch || (settings.MatchDirect && different):
		status = "misrouted"
	case poolExhausted:
		status = "pool_exhausted"
	case probe.Status == "unreachable" && service != nil && service["podReady"] == true && directAddress != "":
		status = "ready_pod_entry_unreachable"
	}
	if probe.Status == "ok" {
		result["roleMismatch"], result["databaseMismatch"], result["differentBackend"] = roleMismatch, databaseMismatch, different
		if settings.MatchDirect && (directAt.IsZero() || probe.Backend == "") && status == "ok" {
			status = "backend_unknown"
		}
	}
	result["status"] = status
	return result
}

func selectedServiceRoute(entry string, kube map[string]any) map[string]any {
	if kube["status"] != "ok" {
		return nil
	}
	host, port, err := net.SplitHostPort(entry)
	if err != nil {
		return nil
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	ns := stringValue(kube["namespace"])
	pod := object(kube["pod"])
	ready := false
	for _, raw := range list(pod["conditions"]) {
		row := object(raw)
		ready = ready || (row["type"] == "Ready" && row["status"] == "True")
	}
	for _, raw := range list(kube["services"]) {
		service := object(raw)
		name := stringValue(service["name"])
		dns := name + "." + ns
		clusterIP := stringValue(service["clusterIP"])
		if host != dns && host != dns+".svc.cluster.local" && host != dns+".svc" &&
			(clusterIP == "" || clusterIP == "None" || host != clusterIP) {
			continue
		}
		for _, rawPort := range list(service["portMappings"]) {
			mapping := object(rawPort)
			if fmt.Sprint(mapping["port"]) == port && mapping["protocol"] == "TCP" {
				routes := object(kube["endpointRoutes"])
				matched := map[string]any{}
				for key, rawRoute := range object(routes["slices"]) {
					if object(rawRoute)["service"] == name {
						matched[key] = rawRoute
					}
				}
				return map[string]any{"name": name, "uid": service["uid"], "namespace": ns, "pod": pod["name"],
					"podUid": pod["uid"], "podReady": ready, "portMapping": mapping, "endpointStatus": routes["status"],
					"slices": matched, "endpointTruncated": routes["truncated"],
					"source": "Kubernetes Service / Pod / EndpointSlice"}
			}
		}
	}
	return nil
}
