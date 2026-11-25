package store

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/collectorstate"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrAssetNotFound = errors.New("asset not found")
var ErrCollectionUnavailable = errors.New("cross-layer Go collector is not yet available")

type CheckResult struct {
	SampleID    string    `json:"sampleId"`
	Status      string    `json:"status"`
	CollectedAt time.Time `json:"collectedAt"`
}

// DueAssetIDs schedules database assets, including
// configured PgBouncer endpoints.
func (s *Postgres) DueAssetIDs(ctx context.Context, interval time.Duration, limit int) ([]string, error) {
	if interval < collectorstate.MinimumIntervalSeconds*time.Second || interval > time.Hour || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid Go collection schedule")
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM assets
		WHERE runtime IN ('linux','k8s','managed','mysql','dm8','kingbase','redis','etcd')
		AND (last_sample_at IS NULL OR last_sample_at <= now() - make_interval(secs=>$1))
		ORDER BY last_sample_at NULLS FIRST,id LIMIT $2`, int(interval.Seconds()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CheckStaleAsset evaluates scheduler delay without writing a synthetic sample.
// It can fire before collection and recover on the same event ID after a sample lands.
func (s *Postgres) CheckStaleAsset(ctx context.Context, id string, interval time.Duration, now time.Time) error {
	return s.checkStaleAsset(ctx, id, interval, now, nil)
}

func (s *Postgres) checkStaleAsset(ctx context.Context, id string, interval time.Duration, now time.Time, lease *CollectionLease) error {
	if interval < collectorstate.MinimumIntervalSeconds*time.Second || interval > time.Hour {
		return fmt.Errorf("invalid Go collection schedule")
	}
	if err := s.ensureCollectionPartitions(ctx, now); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var created time.Time
	var last *time.Time
	if err := tx.QueryRow(ctx, `SELECT created_at,last_sample_at FROM assets WHERE id=$1 FOR UPDATE`, id).
		Scan(&created, &last); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAssetNotFound
		}
		return err
	}
	base := created
	if last != nil {
		base = *last
	}
	if lease != nil {
		if err := lockCollectionLease(ctx, tx, *lease); err != nil {
			return err
		}
		base = lease.DueAt.Add(-interval)
	}
	missed := now.Sub(base).Seconds() / interval.Seconds()
	if missed < 0 {
		missed = 0
	}
	metrics := map[string]any{"collectorMissedIntervals": missed}
	if err := s.applyGoAlerts(ctx, tx, id, "", metrics, nil, now, "collector_stale"); err != nil {
		return fmt.Errorf("evaluate Go stale alert: %w", err)
	}
	return tx.Commit(ctx)
}

func collectionError(err error) (string, string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if strings.HasPrefix(pgErr.Code, "08") || pgErr.Code == "53300" ||
			pgErr.Code == "57P01" || pgErr.Code == "57P02" || pgErr.Code == "57P03" {
			return "availability", "PostgreSQL temporarily unavailable (SQLSTATE " + pgErr.Code + ")"
		}
		if strings.HasPrefix(pgErr.Code, "28") {
			return "telemetry", "PostgreSQL authentication failed (SQLSTATE " + pgErr.Code + ")"
		}
		if pgErr.Code == "42501" {
			return "telemetry", "PostgreSQL monitoring role lacks permission (SQLSTATE 42501)"
		}
		return "telemetry", "PostgreSQL collection query failed (SQLSTATE " + pgErr.Code + ")"
	}
	if strings.Contains(err.Error(), "outside the supported") {
		return "telemetry", err.Error()
	}
	var certificateError *tls.CertificateVerificationError
	if errors.As(err, &certificateError) {
		return "telemetry", "PostgreSQL TLS certificate or hostname verification failed"
	}
	if strings.Contains(strings.ToLower(err.Error()), "connection refused") {
		return "availability", "PostgreSQL connection refused"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "availability", "PostgreSQL connection timed out"
	}
	return "availability", "PostgreSQL connection or collection failed"
}

func (s *Postgres) CheckAsset(ctx context.Context, id string, key []byte) (CheckResult, error) {
	return s.checkAsset(ctx, id, key, nil, 0)
}

func (s *Postgres) checkAsset(ctx context.Context, id string, key []byte, lease *CollectionLease, interval time.Duration) (CheckResult, error) {
	var target collector.Target
	var encrypted string
	var runtime string
	var linuxHost *string
	var exporterPort *int
	var poolerEncrypted *string
	var patroniEncrypted *string
	var routeSettings RoutingSettings
	var statsEncrypted *string
	var k8sCluster, k8sNamespace, k8sWorkload, k8sPod *string
	err := s.pool.QueryRow(ctx, `SELECT host,port,database_name,username,ssl_mode,password_enc,
		runtime,linux_host,linux_exporter_port,pooler_url_enc,patroni_url_enc,
		k8s_cluster,k8s_namespace,k8s_workload,k8s_pod,
		coalesce(r.kind,''),coalesce(r.entry,''),coalesce(r.expected_role,''),coalesce(r.match_direct,false),
		coalesce(r.haproxy_backend,''),r.haproxy_stats_enc,coalesce(r.read_entry,'')
		FROM assets a LEFT JOIN asset_route_settings r ON r.asset_id=a.id WHERE a.id=$1`, id).Scan(&target.Host, &target.Port, &target.Database,
		&target.Username, &target.SSLMode, &encrypted, &runtime, &linuxHost, &exporterPort,
		&poolerEncrypted, &patroniEncrypted, &k8sCluster, &k8sNamespace, &k8sWorkload, &k8sPod,
		&routeSettings.Kind, &routeSettings.Entry, &routeSettings.ExpectedRole, &routeSettings.MatchDirect,
		&routeSettings.HAProxyBackend, &statsEncrypted, &routeSettings.ReadEntry)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckResult{}, ErrAssetNotFound
	}
	if err != nil {
		return CheckResult{}, fmt.Errorf("load monitoring target: %w", err)
	}
	if !assets.IsSupportedRuntime(runtime) {
		return CheckResult{}, ErrCollectionUnavailable
	}
	target.Runtime = runtime
	password, err := security.Decrypt(key, encrypted)
	if err != nil {
		return CheckResult{}, fmt.Errorf("decrypt monitoring credential: %w", err)
	}
	if runtime == assets.RuntimeMySQL || runtime == assets.RuntimeDM8 || runtime == assets.RuntimeKingbase {
		infra := nativeInfraTarget{host: linuxHost, exporterPort: exporterPort, kube: collector.KubernetesTarget{
			Cluster: dereference(k8sCluster), Namespace: dereference(k8sNamespace), Pod: dereference(k8sPod), Workload: dereference(k8sWorkload),
		}}
		if runtime == assets.RuntimeMySQL {
			return s.checkMySQL(ctx, id, target, password, lease, interval, infra)
		}
		if runtime == assets.RuntimeDM8 {
			return s.checkDM8(ctx, id, target, password, lease, interval, infra)
		}
		return s.checkKingbase(ctx, id, target, password, lease, interval, infra)
	}
	if runtime == assets.RuntimeRedis {
		return s.checkRedis(ctx, id, target, password, lease, interval, nativeInfraTarget{host: linuxHost, exporterPort: exporterPort})
	}
	if runtime == assets.RuntimeEtcd {
		target.KubernetesCluster = dereference(k8sCluster)
		return s.checkEtcd(ctx, id, target, password, lease, interval, nativeInfraTarget{host: linuxHost, exporterPort: exporterPort})
	}
	poolerURL := ""
	if poolerEncrypted != nil {
		poolerURL, err = security.Decrypt(key, *poolerEncrypted)
		if err != nil {
			return CheckResult{}, fmt.Errorf("decrypt PgBouncer credential: %w", err)
		}
	}
	patroniURL := ""
	if patroniEncrypted != nil {
		patroniURL, err = security.Decrypt(key, *patroniEncrypted)
		if err != nil {
			return CheckResult{}, fmt.Errorf("decrypt Patroni credential: %w", err)
		}
	}
	if lease != nil {
		lease.startedAt = time.Now()
	}
	var metrics, kube, linux, pooler, patroni, haproxy map[string]any
	var routeProbe, readProbe collector.RouteProbe
	var collectErr, linuxErr, poolerErr, patroniErr error
	var poolerDone chan struct{}
	var groupWait sync.WaitGroup
	groupWait.Add(1)
	go func() {
		defer groupWait.Done()
		queryStarted := time.Now()
		metrics, collectErr = collector.PostgreSQL(ctx, target, password)
		lease.recordQueryDuration(queryStarted)
	}()
	if runtime == "k8s" {
		groupWait.Add(1)
		go func() {
			defer groupWait.Done()
			kube = collector.Kubernetes(ctx, collector.KubernetesTarget{Cluster: dereference(k8sCluster),
				Namespace: dereference(k8sNamespace), Pod: dereference(k8sPod), Workload: dereference(k8sWorkload)})
		}()
	} else if runtime == "linux" && exporterPort != nil && linuxHost != nil {
		groupWait.Add(1)
		go func() {
			defer groupWait.Done()
			linux, linuxErr = collector.Linux(ctx, *linuxHost, *exporterPort)
		}()
	}
	if poolerURL != "" {
		poolerDone = make(chan struct{})
		groupWait.Add(1)
		go func() {
			defer groupWait.Done()
			defer close(poolerDone)
			pooler, poolerErr = collector.PgBouncer(ctx, poolerURL, target.Database)
		}()
	}
	if patroniURL != "" {
		groupWait.Add(1)
		go func() {
			defer groupWait.Done()
			patroni, patroniErr = collector.Patroni(ctx, patroniURL)
		}()
	}
	probeEntry := func(entry string, into *collector.RouteProbe) {
		groupWait.Add(1)
		go func() {
			defer groupWait.Done()
			// Read the existing queue before this monitor adds a client to the same pool.
			if routeSettings.Kind == "pgbouncer" && poolerDone != nil {
				<-poolerDone
			}
			routeTarget := target
			routeTarget.Host, routeTarget.Port, _ = assets.ParsePoolerEndpoint(entry)
			*into = collector.ProbePostgreSQLRoute(ctx, routeTarget, password)
		}()
	}
	if routeSettings.Kind != "" {
		probeEntry(routeSettings.Entry, &routeProbe)
		if routeSettings.ReadEntry != "" {
			probeEntry(routeSettings.ReadEntry, &readProbe)
		}
	}
	if statsEncrypted != nil {
		groupWait.Add(1)
		go func() {
			defer groupWait.Done()
			endpoint, err := security.Decrypt(key, *statsEncrypted)
			if err != nil {
				haproxy = map[string]any{"status": "credential_unavailable", "source": "HAProxy HTTP CSV"}
				return
			}
			haproxy = collector.HAProxyStats(ctx, endpoint, routeSettings.HAProxyBackend)
		}()
	}
	groupWait.Wait()
	group, reason := "", ""
	if collectErr != nil {
		group, reason = collectionError(collectErr)
	}
	if metrics == nil {
		metrics = map[string]any{}
	}
	pgStatus := "ok"
	if collectErr != nil {
		pgStatus = group
	}
	metrics["collector"] = map[string]any{"pgStatus": pgStatus}
	if runtime == "k8s" {
		collector.CompareCNPGRole(kube, metrics)
		metrics["k8s"] = kube
		if kube["status"] != "ok" && collectErr == nil {
			group, reason = "telemetry", "Kubernetes resource collection is unavailable"
		}
	} else if runtime == "linux" && exporterPort != nil && linuxHost != nil {
		if linuxErr != nil {
			metrics["linux"] = map[string]any{"status": "error", "reason": linuxErr.Error()}
			if collectErr == nil {
				group, reason = "telemetry", "Linux node_exporter collection failed"
			}
		} else {
			linux["endpoint"] = net.JoinHostPort(*linuxHost, strconv.Itoa(*exporterPort))
			metrics["linux"] = linux
		}
	} else if runtime == "linux" {
		metrics["linux"] = map[string]any{"status": "not_configured",
			"reason": "Linux node_exporter host and port are not configured"}
	}
	storage, nodeStorage := s.collectContainerStorage(ctx, id, runtime, target.Port, kube)
	if storage != nil {
		metrics["containerStorage"] = storage
		if nodeStorage != nil && object(metrics["linux"])["status"] != "ok" {
			metrics["linux"] = nodeStorage
		}
	}
	if poolerURL != "" {
		if poolerErr != nil {
			metrics["pgbouncer"] = map[string]any{"status": "error",
				"reason": "PgBouncer admin connection or query failed"}
			if collectErr == nil && reason == "" {
				group, reason = "telemetry", "PgBouncer collection failed"
			}
		} else {
			metrics["pgbouncer"] = pooler
			if pooler["status"] != "ok" && collectErr == nil && reason == "" {
				group, reason = "telemetry", "PgBouncer pool mapping is unavailable"
			}
		}
	}
	if patroniURL != "" {
		if patroniErr != nil {
			metrics["patroni"] = map[string]any{"status": "error", "reason": patroniErr.Error()}
			if collectErr == nil && reason == "" {
				group, reason = "telemetry", "Patroni REST API collection failed"
			}
		} else {
			metrics["patroni"] = patroni
		}
	}
	if backup := collector.Backup(id, time.Now().UTC()); backup != nil {
		metrics["backup"] = backup
	}
	if routeSettings.Kind != "" {
		route := routeAssessment(routeSettings, routeProbe, metrics, kube, pooler, haproxy, target.Username)
		if routeSettings.ReadEntry != "" {
			readStatus := readProbe.Status
			if readStatus == "ok" && readProbe.Role != "standby" {
				readStatus = "misrouted"
			}
			route["readEntry"], route["readProbe"], route["readStatus"] = routeSettings.ReadEntry, readProbe, readStatus
		}
		metrics["routing"] = route
	}
	return s.recordCheckWithLease(ctx, id, metrics, group, reason, time.Now().UTC(), lease, interval)
}

func dereference(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *Postgres) recordCheck(ctx context.Context, id string, metrics map[string]any, group, reason string) (CheckResult, error) {
	return s.recordCheckAt(ctx, id, metrics, group, reason, time.Now().UTC())
}

func (s *Postgres) recordCheckAt(ctx context.Context, id string, metrics map[string]any, group, reason string, now time.Time) (CheckResult, error) {
	return s.recordCheckWithLease(ctx, id, metrics, group, reason, now, nil, 0)
}

func (s *Postgres) recordCheckWithLease(ctx context.Context, id string, metrics map[string]any, group, reason string, now time.Time, lease *CollectionLease, interval time.Duration) (CheckResult, error) {
	writeStarted := time.Now()
	if err := s.ensureCollectionPartitions(ctx, now); err != nil {
		return CheckResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	defer tx.Rollback(ctx)
	var runtime string
	if err := tx.QueryRow(ctx, "SELECT runtime FROM assets WHERE id=$1 FOR UPDATE", id).Scan(&runtime); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CheckResult{}, ErrAssetNotFound
		}
		return CheckResult{}, err
	}
	isMySQL := runtime == assets.RuntimeMySQL
	label, source := "PostgreSQL", "go_postgres_collector"
	if isMySQL {
		label, source = "MySQL", "go_mysql_collector"
	} else if runtime == assets.RuntimeDM8 {
		label, source = "达梦 DM8", "go_dm8_collector"
	} else if runtime == assets.RuntimeKingbase {
		label, source = "金仓 V9", "go_kingbase_collector"
	} else if runtime == assets.RuntimeRedis {
		label, source = "Redis", "go_redis_collector"
	} else if runtime == assets.RuntimeEtcd {
		label, source = "etcd", "go_etcd_collector"
	}
	if lease != nil {
		if err := lockCollectionLease(ctx, tx, *lease); err != nil {
			return CheckResult{}, err
		}
		layer := ensureCollectorLayer(metrics)
		layer["scheduledFor"] = lease.DueAt
		layer["intervalSeconds"] = int(interval.Seconds())
		layer["isolated"] = lease.Isolated
		if delay := collectionQueueDelay(*lease); delay != nil {
			layer["queueMs"] = *delay
		}
		if lease.queryMs != nil {
			layer["queryMs"] = *lease.queryMs
		}
		if s.platformReplicaID != "" {
			layer["replicaId"] = s.platformReplicaID
		}
	}
	var previous map[string]any
	var previousID string
	var previousAt time.Time
	var previousJSON []byte
	if err := tx.QueryRow(ctx, `SELECT id::text,collected_at,metrics_json FROM samples WHERE asset_id=$1
		AND collected_at<$2 ORDER BY collected_at DESC LIMIT 1`, id, now).Scan(&previousID, &previousAt, &previousJSON); err == nil {
		_ = json.Unmarshal(previousJSON, &previous)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return CheckResult{}, fmt.Errorf("read previous monitoring sample: %w", err)
	}
	if lease != nil && previousID != "" && interval > 0 && now.Sub(previousAt) >= 2*interval {
		layer := ensureCollectorLayer(metrics)
		layer["gapSince"] = previousAt
		layer["gapSeconds"] = int64(now.Sub(previousAt).Seconds())
	}
	recordLinuxInterval(object(metrics["linux"]), object(previous["linux"]), now.Sub(previousAt))
	recordContainerStorage(metrics, previous, interval)
	status := "ok"
	if reason != "" {
		status = "error"
	}
	var redisBefore collector.RedisMetrics
	var redisWindow *redisIntervalMetrics
	if current, ok := metrics["mysql"].(collector.MySQLMetrics); ok && runtime == assets.RuntimeMySQL {
		if window := readMySQLInterval(current, previousJSON, now.Sub(previousAt), interval); window != nil {
			metrics["mysqlInterval"] = window
		}
	}
	if current, ok := metrics["redis"].(collector.RedisMetrics); ok && runtime == assets.RuntimeRedis {
		redisBefore = priorRedisSample(previousJSON)
		redisWindow = redisInterval(current, redisBefore, now.Sub(previousAt), interval, previousID)
		if redisWindow != nil {
			metrics["redisInterval"] = redisWindow
		}
	}
	var etcdBefore collector.EtcdMetrics
	var etcdReading *etcdWindow
	if current, ok := metrics["etcd"].(collector.EtcdMetrics); ok && runtime == assets.RuntimeEtcd {
		etcdBefore = priorEtcdSample(previousJSON)
		etcdReading = etcdInterval(current, etcdBefore, previousAt, now, interval, previousID)
		if etcdReading != nil {
			metrics["etcdInterval"] = etcdReading
		}
	}
	if status == "ok" && assets.IsPostgreSQLRuntime(runtime) {
		if err := attachDatabaseGrowth(ctx, tx, id, metrics, now); err != nil {
			return CheckResult{}, err
		}
		if stringValue(object(metrics["sqlPerformance"])["status"]) == "ok" &&
			stringValue(object(previous["sqlPerformance"])["status"]) == "ok" &&
			now.Sub(previousAt) <= 2*time.Hour {
			var olderID string
			var olderAt time.Time
			var olderJSON []byte
			err := tx.QueryRow(ctx, `SELECT id::text,collected_at,metrics_json FROM samples WHERE asset_id=$1
				AND collected_at<$2 ORDER BY collected_at DESC LIMIT 1 OFFSET 1`, id, now).
				Scan(&olderID, &olderAt, &olderJSON)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return CheckResult{}, fmt.Errorf("read SQL baseline sample: %w", err)
			}
			if err == nil && previousAt.After(olderAt) && previousAt.Sub(olderAt) <= 2*time.Hour {
				var older map[string]any
				if json.Unmarshal(olderJSON, &older) == nil {
					regression := sqlRegressionSignal(object(metrics["sqlPerformance"]),
						object(previous["sqlPerformance"]), object(older["sqlPerformance"]))
					if regression != nil {
						regression["previousSampleId"] = previousID
						regression["baselineSampleId"] = olderID
						metrics["sqlRegression"] = regression
					}
				}
			}
		}
	}
	encoded, err := json.Marshal(metrics)
	if err != nil {
		return CheckResult{}, err
	}
	var sampleID string
	err = tx.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,$5) RETURNING id`, id, now, status, encoded, nullable(reason)).Scan(&sampleID)
	if err != nil {
		return CheckResult{}, fmt.Errorf("save monitoring sample: %w", err)
	}
	var version, role any
	if metrics != nil {
		version = metrics["version_num"]
		role = metrics["role"]
	}
	_, err = tx.Exec(ctx, `UPDATE assets SET last_sample_at=$1,last_status=$2,last_error=$3,
		version_num=coalesce($4,version_num),role=coalesce($5,role) WHERE id=$6`,
		now, status, nullable(reason), version, role, id)
	if err != nil {
		return CheckResult{}, fmt.Errorf("update asset status: %w", err)
	}
	if reason != "" {
		var eventID string
		err = tx.QueryRow(ctx, `SELECT id FROM events WHERE asset_id=$1 AND event_group=$2
			AND status='open' ORDER BY started_at DESC LIMIT 1 FOR UPDATE`, id, group).Scan(&eventID)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
				VALUES(gen_random_uuid(),$1,$2) RETURNING id`, id, now).Scan(&eventID)
			if err != nil {
				return CheckResult{}, err
			}
			title := label + " 采集失败"
			if group == "availability" {
				title = label + " 无法连接"
			}
			_, err = tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
				VALUES($1,$2,'collection_failure',$3,'open',$4,$5)`, eventID, id, title, now, group)
			if err != nil {
				return CheckResult{}, err
			}
			_, err = tx.Exec(ctx, `INSERT INTO open_event_groups(asset_id,event_group,event_id)
				VALUES($1,$2,$3) ON CONFLICT(asset_id,event_group)
				DO UPDATE SET event_id=excluded.event_id`, id, group, eventID)
			if err != nil {
				return CheckResult{}, err
			}
		} else if err != nil {
			return CheckResult{}, err
		}
		payload, _ := json.Marshal(map[string]any{"sampleId": sampleID, "error": reason, "source": source})
		var collectionEvidenceID string
		err = tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'collection_error',$2,$3) RETURNING id::text`, eventID, now, payload).
			Scan(&collectionEvidenceID)
		if err != nil {
			return CheckResult{}, err
		}
		if k8s := object(metrics["k8s"]); len(k8s) != 0 {
			k8sPayload := encodeStateEvidence(k8s, sampleID)
			var k8sEvidenceID string
			if err := tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
				VALUES(gen_random_uuid(),$1,'k8s_state',$2,$3) RETURNING id::text`,
				eventID, now, k8sPayload).Scan(&k8sEvidenceID); err != nil {
				return CheckResult{}, err
			}
			if group == "availability" && stringValue(k8s["status"]) == "ok" {
				for _, raw := range list(k8s["containers"]) {
					container := object(raw)
					causeAt, parseErr := time.Parse(time.RFC3339Nano, stringValue(container["lastTerminatedAt"]))
					if stringValue(container["terminatedReason"]) != "OOMKilled" || parseErr != nil ||
						now.Before(causeAt) || now.Sub(causeAt) >= 10*time.Minute {
						continue
					}
					var exists bool
					if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence
						WHERE event_id=$1 AND kind='root_cause_candidate')`, eventID).Scan(&exists); err != nil {
						return CheckResult{}, err
					}
					if !exists {
						candidate, _ := json.Marshal(map[string]any{"hypothesis": "Pod 容器 " + stringValue(container["name"]) +
							" 最近发生 OOMKilled，可能导致 PostgreSQL 连接失败", "confidence": "medium",
							"supports": []string{collectionEvidenceID, k8sEvidenceID}, "causeObservedAt": causeAt,
							"limitation": "时间关联不等于因果；仍需核对容器日志、探针和服务端点。"})
						if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
							VALUES(gen_random_uuid(),$1,'root_cause_candidate',$2,$3)`, eventID, now, candidate); err != nil {
							return CheckResult{}, err
						}
					}
					break
				}
			}
		}
		if linux := object(metrics["linux"]); len(linux) != 0 {
			linuxPayload := encodeStateEvidence(linux, sampleID)
			if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
				VALUES(gen_random_uuid(),$1,'linux_state',$2,$3)`, eventID, now, linuxPayload); err != nil {
				return CheckResult{}, err
			}
		}
		if pooler := object(metrics["pgbouncer"]); len(pooler) != 0 {
			poolerPayload := encodeStateEvidence(pooler, sampleID)
			if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
				VALUES(gen_random_uuid(),$1,'pgbouncer_state',$2,$3)`, eventID, now, poolerPayload); err != nil {
				return CheckResult{}, err
			}
		}
		if patroni := object(metrics["patroni"]); len(patroni) != 0 {
			patroniPayload := encodeStateEvidence(patroni, sampleID)
			if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
				VALUES(gen_random_uuid(),$1,'patroni_state',$2,$3)`, eventID, now, patroniPayload); err != nil {
				return CheckResult{}, err
			}
		}
	}
	if isMySQL {
		if status == "ok" {
			if err := recordMySQLLockEvent(ctx, tx, id, sampleID, metrics, now); err != nil {
				return CheckResult{}, fmt.Errorf("record MySQL lock event: %w", err)
			}
			if err := recordMySQLReplicationEvent(ctx, tx, id, sampleID, metrics, now); err != nil {
				return CheckResult{}, fmt.Errorf("record MySQL replication event: %w", err)
			}
		}
	} else if runtime == assets.RuntimeDM8 {
		if status == "ok" {
			if err := recordDM8LockEvent(ctx, tx, id, sampleID, metrics, now); err != nil {
				return CheckResult{}, fmt.Errorf("record DM8 lock event: %w", err)
			}
			if details, ok := metrics["dm8"].(collector.DM8Metrics); ok {
				if err := recordNativeHAEvents(ctx, tx, id, sampleID, previousID, "dm8", now, previousAt, details.HA, previous, interval); err != nil {
					return CheckResult{}, fmt.Errorf("record DM8 replication event: %w", err)
				}
			}
		}
	} else if runtime == assets.RuntimeKingbase {
		if status == "ok" {
			if err := recordKingbaseLockEvent(ctx, tx, id, sampleID, metrics, now); err != nil {
				return CheckResult{}, fmt.Errorf("record Kingbase lock event: %w", err)
			}
			if details, ok := metrics["kingbase"].(collector.KingbaseMetrics); ok {
				if err := recordNativeHAEvents(ctx, tx, id, sampleID, previousID, "kingbase", now, previousAt, details.HA, previous, interval); err != nil {
					return CheckResult{}, fmt.Errorf("record Kingbase replication event: %w", err)
				}
			}
		}
	} else if runtime == assets.RuntimeRedis {
		if current, ok := metrics["redis"].(collector.RedisMetrics); ok && status == "ok" {
			if err := recordRedisEvents(ctx, tx, id, sampleID, previousID, now, current, redisBefore, redisWindow); err != nil {
				return CheckResult{}, fmt.Errorf("record Redis event: %w", err)
			}
		}
	} else if runtime == assets.RuntimeEtcd {
		if current, ok := metrics["etcd"].(collector.EtcdMetrics); ok && status == "ok" {
			if err := recordEtcdEvents(ctx, tx, id, sampleID, previousID, now, current, etcdBefore, etcdReading); err != nil {
				return CheckResult{}, fmt.Errorf("record etcd event: %w", err)
			}
		}
	} else {
		var observed map[string]any
		if err := json.Unmarshal(encoded, &observed); err != nil {
			return CheckResult{}, err
		}
		if err := recordObservedChanges(ctx, tx, id, previousID, sampleID, previousAt, now, previous, observed); err != nil {
			return CheckResult{}, fmt.Errorf("record observed changes: %w", err)
		}
		if err := s.applyGoAlerts(ctx, tx, id, sampleID, observed, previous, now, ""); err != nil {
			return CheckResult{}, fmt.Errorf("evaluate Go alert rules: %w", err)
		}
	}
	if err := s.resolveGoCollectionFailures(ctx, tx, id, sampleID, group, source, now); err != nil {
		return CheckResult{}, fmt.Errorf("resolve Go collection failures: %w", err)
	}
	if lease != nil {
		if err := s.applyGoAlerts(ctx, tx, id, sampleID, map[string]any{"collectorMissedIntervals": 0}, nil, now, "collector_stale"); err != nil {
			return CheckResult{}, fmt.Errorf("resolve Go stale alert: %w", err)
		}
		outcome := "ok"
		if status == "error" {
			outcome = "target_error"
		}
		var workMs *int64
		if !lease.startedAt.IsZero() {
			elapsed := writeStarted.Sub(lease.startedAt).Milliseconds()
			workMs = &elapsed
		}
		if _, err := tx.Exec(ctx, finishCollectionSQL,
			lease.AssetID, lease.Token, outcome, sampleID, int(interval.Seconds()), collectionQueueDelay(*lease),
			lease.queryMs, time.Since(writeStarted).Milliseconds(), s.platformReplicaID, now, workMs); err != nil {
			return CheckResult{}, fmt.Errorf("advance collection schedule: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CheckResult{}, err
	}
	return CheckResult{SampleID: sampleID, Status: status, CollectedAt: now}, nil
}

func encodeStateEvidence(state map[string]any, sampleID string) []byte {
	payload := maps.Clone(state)
	payload["sampleId"] = sampleID
	encoded, _ := json.Marshal(payload)
	return encoded
}

func (s *Postgres) resolveGoCollectionFailures(ctx context.Context, tx pgx.Tx, assetID, sampleID, currentGroup, source string, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND status='open'
		AND kind='collection_failure' FOR UPDATE`, assetID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, eventID := range ids {
		var eventGroup string
		if err := tx.QueryRow(ctx, `SELECT event_group FROM events WHERE id=$1`, eventID).Scan(&eventGroup); err != nil {
			return err
		}
		if eventGroup == currentGroup {
			continue
		}
		var recovered bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM evidence
			WHERE event_id=$1 AND kind='collection_recovered' AND observed_at >=
			coalesce((SELECT max(observed_at) FROM evidence WHERE event_id=$1
			AND kind='collection_error'),'-infinity'::timestamptz))`, eventID).Scan(&recovered); err != nil {
			return err
		}
		if !recovered {
			payload, _ := json.Marshal(map[string]any{"sampleId": sampleID, "source": source})
			if _, err := tx.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
				VALUES(gen_random_uuid(),$1,'collection_recovered',$2,$3)`, eventID, now, payload); err != nil {
				return err
			}
		}
		var firing int
		if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM alert_states WHERE asset_id=$1
			AND state_json->>'eventId'=$2 AND state_json->>'status'='firing'`, assetID, eventID).Scan(&firing); err != nil {
			return err
		}
		if firing != 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE events SET status='resolved',ended_at=$1 WHERE id=$2`, now, eventID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM open_event_groups WHERE event_id=$1`, eventID); err != nil {
			return err
		}
		var alertRecoveryID string
		err := tx.QueryRow(ctx, `SELECT id::text FROM evidence WHERE event_id=$1
			AND kind='alert_recovered' AND payload_json->>'silenceId' IS NULL
			ORDER BY observed_at DESC,id DESC LIMIT 1`, eventID).Scan(&alertRecoveryID)
		if err == nil {
			if err := s.queueGoNotifications(ctx, tx, alertRecoveryID, now); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

func (s *Postgres) ensureCollectionPartitions(ctx context.Context, at time.Time) error {
	month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := month.AddDate(0, 1, 0)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1742026)"); err != nil {
		return err
	}
	for _, table := range []string{"samples", "events"} {
		name := fmt.Sprintf("%s_%04d%02d", table, month.Year(), int(month.Month()))
		query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')",
			name, table, month.Format(time.RFC3339), end.Format(time.RFC3339))
		if _, err := tx.Exec(ctx, query); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
