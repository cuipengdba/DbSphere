package store

import (
	"context"
	"encoding/json"
	"net/url"
	"time"
)

type wallboardSample struct {
	id, status string
	at, bucket time.Time
	metrics    map[string]any
}

type wallboardInterval struct{ current, previous wallboardSample }

type wallboardPoint struct {
	SampleID string             `json:"sampleId"`
	At       time.Time          `json:"at"`
	Bucket   time.Time          `json:"bucket"`
	HostAt   string             `json:"hostAt,omitempty"`
	SQLCount int                `json:"sqlCount,omitempty"`
	Boundary string             `json:"boundary,omitempty"`
	Values   map[string]float64 `json:"values"`
}

func (s *Postgres) wallboardHistory(ctx context.Context, asset string, params url.Values) (any, error) {
	from, to, err := timeRange(params, time.Hour, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	// Pick the newest sample per bucket. Use its actual predecessor for rates
	// so coarse zoom levels do not change the interval represented by a point.
	minutes := max(1, int((to.Sub(from)+120*time.Minute-1)/(120*time.Minute)))
	org := OrganizationFromContext(ctx)
	rows, err := s.pool.Query(ctx, `SELECT b.id::text,b.collected_at,b.status,b.metrics_json,b.bucket,
		p.collected_at,coalesce(p.status,''),p.metrics_json FROM (
		SELECT DISTINCT ON (bucket) s.id,s.collected_at,s.status,s.metrics_json,
			date_bin(make_interval(mins=>$4),s.collected_at,$2::timestamptz) AS bucket
		FROM samples s JOIN assets a ON a.id=s.asset_id
		WHERE s.asset_id=$1 AND s.collected_at >= $2::timestamptz AND s.collected_at < $3
			AND ($5='' OR a.organization_id::text=$5)
		ORDER BY bucket,s.collected_at DESC,s.id DESC
	) b LEFT JOIN LATERAL (
		SELECT s.collected_at,s.status,s.metrics_json FROM samples s WHERE s.asset_id=$1
		AND s.collected_at>=b.collected_at-interval '10 minutes' AND s.collected_at<b.collected_at
		ORDER BY s.collected_at DESC,s.id DESC LIMIT 1
	) p ON true ORDER BY b.bucket`, asset, from, to, minutes, org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	samples := []wallboardInterval{}
	for rows.Next() {
		var interval wallboardInterval
		sample, previous := &interval.current, &interval.previous
		var metrics, priorMetrics []byte
		var priorAt *time.Time
		if err := rows.Scan(&sample.id, &sample.at, &sample.status, &metrics, &sample.bucket, &priorAt, &previous.status, &priorMetrics); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(metrics, &sample.metrics); err != nil {
			return nil, err
		}
		if priorAt != nil {
			previous.at = *priorAt
			if err := json.Unmarshal(priorMetrics, &previous.metrics); err != nil {
				return nil, err
			}
		}
		samples = append(samples, interval)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	points := []wallboardPoint{}
	host, container, containerName := "", "", ""
	if len(samples) > 0 {
		current := samples[len(samples)-1].current.metrics
		container, _ = wallboardContainer(current)
		if container != "" {
			containerName = stringValue(object(current["containerStorage"])["primary"])
		}
		// Keep the latest verified source when a failed read has no identity.
		for i := len(samples) - 1; i >= 0 && host == ""; i-- {
			host = wallboardHost(samples[i].current.metrics)
		}
	}
	for _, sample := range samples {
		points = append(points, readWallboardPoint(sample.current, sample.previous, host, container))
	}
	events, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',e.id,'title',e.title,'status',e.status,
		'startedAt',e.started_at,'endedAt',e.ended_at) FROM events e JOIN assets a ON a.id=e.asset_id
		WHERE e.asset_id=$1 AND e.started_at<$3 AND (e.ended_at IS NULL OR e.ended_at >= $2)
		AND ($4='' OR a.organization_id::text=$4)
		ORDER BY (e.status='open') DESC,e.started_at DESC,e.id LIMIT 40`, asset, from, to, org)
	if err != nil {
		return nil, err
	}
	changes, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',c.id,'at',c.occurred_at,'label',c.label)
		FROM asset_changes c JOIN assets a ON a.id=c.asset_id WHERE c.asset_id=$1 AND c.occurred_at>=$2 AND c.occurred_at<$3
		AND ($4='' OR a.organization_id::text=$4) ORDER BY c.occurred_at DESC,c.id LIMIT 40`, asset, from, to, org)
	if err != nil {
		return nil, err
	}
	return map[string]any{"points": points, "events": events, "changes": changes,
		"from": from, "to": to, "bucketMinutes": minutes, "containerObserved": container != "", "container": containerName}, nil
}

func wallboardHost(metrics map[string]any) string {
	return stringValue(object(metrics["linux"])["endpoint"])
}

func wallboardContainer(metrics map[string]any) (string, map[string]any) {
	storage := object(metrics["containerStorage"])
	container := object(object(storage["containers"])[stringValue(storage["primary"])])
	return stringValue(container["id"]), container
}

func wallboardValue(values map[string]float64, key string, raw any) {
	if value, ok := numeric(raw); ok && value >= 0 {
		values[key] = value
	}
}

func readWallboardPoint(sample, previous wallboardSample, host, containerID string) wallboardPoint {
	values := map[string]float64{}
	point := wallboardPoint{SampleID: sample.id, At: sample.at, Bucket: sample.bucket, Values: values}
	metrics := sample.metrics
	elapsed := sample.at.Sub(previous.at).Seconds()
	bounded := elapsed >= 1 && elapsed <= 600
	if bounded {
		started, priorStart := stringValue(metrics["postgresStartedAt"]), stringValue(previous.metrics["postgresStartedAt"])
		dbReset := stringValue(object(object(object(metrics["diagnostics"])["database"])["data"])["statsReset"])
		priorReset := stringValue(object(object(object(previous.metrics["diagnostics"])["database"])["data"])["statsReset"])
		container, _ := wallboardContainer(metrics)
		priorContainer, _ := wallboardContainer(previous.metrics)
		switch {
		case started != "" && priorStart != "" && started != priorStart:
			point.Boundary = "PostgreSQL 实例重启"
		case dbReset != "" && priorReset != "" && dbReset != priorReset:
			point.Boundary = "数据库统计重置"
		case container != "" && priorContainer != "" && container != priorContainer:
			point.Boundary = "容器身份变化"
		}
	}

	linux := object(metrics["linux"])
	if host != "" && wallboardHost(metrics) == host && linux["status"] == "ok" {
		point.HostAt = stringValue(linux["observedAt"])
		wallboardValue(values, "cpu", object(linux["cpu"])["usedPercent"])
		wallboardValue(values, "memory", object(linux["memory"])["usedPercent"])
		interval := object(linux["interval"])
		for _, key := range []string{"diskReadBps", "diskWriteBps", "networkReceiveBps", "networkTransmitBps"} {
			wallboardValue(values, key, interval[key])
		}
		if bounded {
			wallboardDiskLatency(values, linux, object(previous.metrics["linux"]))
		}
	}
	if currentID, container := wallboardContainer(metrics); containerID != "" && currentID == containerID && container["status"] == "ok" {
		memory := object(container["memory"])
		wallboardValue(values, "activeFile", memory["active_file"])
		wallboardValue(values, "inactiveFile", memory["inactive_file"])
	}
	if pooler := object(metrics["pgbouncer"]); pooler["status"] == "ok" {
		if _, ok := pooler["pools"].([]any); ok {
			if signal := readGoSignal("pgbouncer.waiting_clients", metrics, sample.at); signal.Available {
				values["poolWaiting"] = signal.Value
			}
		}
	}
	if sample.status == "ok" && wallboardNativeValues(values, metrics) {
		return point
	}
	if !wallboardPGAvailable(sample) {
		return point
	}
	if used, ok := numeric(metrics["connections"]); ok {
		if limit, ok := numeric(metrics["max_connections"]); ok && limit > 0 {
			wallboardValue(values, "connections", 100*used/limit)
		}
	}
	if signal := readGoSignal("pg.replication_lag_bytes", metrics, sample.at); signal.Available {
		values["lag"] = signal.Value
	}
	diagnostics := object(metrics["diagnostics"])
	for group, names := range map[string][]string{
		"instanceSessions": {"waiting"}, "locks": {"blockedSessions"},
		"transactions": {"longTransactions", "idleInTransaction"},
	} {
		data := object(diagnostics[group])
		if data["status"] == "ok" {
			for _, name := range names {
				wallboardValue(values, name, object(data["data"])[name])
			}
		}
	}
	if bounded && wallboardPGAvailable(previous) && wallboardSamePG(metrics, previous.metrics) {
		prior := previous.metrics
		newReset := object(object(diagnostics["database"])["data"])["statsReset"]
		oldReset := object(object(object(prior["diagnostics"])["database"])["data"])["statsReset"]
		commits, commitsKnown := storageDelta(metrics, prior, "xact_commit")
		rollbacks, rollbacksKnown := storageDelta(metrics, prior, "xact_rollback")
		if sqlResetAt(newReset) == sqlResetAt(oldReset) && commitsKnown && rollbacksKnown && commits >= 0 && rollbacks >= 0 {
			values["tps"] = (commits + rollbacks) / elapsed
		}
		if current, earlier := object(metrics["sqlPerformance"]), object(prior["sqlPerformance"]); current["status"] == "ok" && earlier["status"] == "ok" {
			deltas := sqlDelta(current, earlier)
			var calls, ms float64
			for _, delta := range deltas {
				calls += number(delta["calls"])
				ms += number(delta["totalExecMs"])
			}
			if calls > 0 {
				values["sqlMs"] = ms / calls
				point.SQLCount = len(deltas)
			}
		}
	}
	return point
}

func wallboardNativeValues(values map[string]float64, metrics map[string]any) bool {
	copyValues := func(source map[string]any, fields map[string]string) {
		for series, key := range fields {
			wallboardValue(values, series, source[key])
		}
	}
	signalOK := func(key string) bool {
		for _, raw := range list(object(metrics["adapter"])["signals"]) {
			signal := object(raw)
			if signal["key"] == key {
				return signal["status"] == "ok"
			}
		}
		return false
	}
	putWaits := func(series string, raw any) {
		if waits, ok := raw.([]any); ok && signalOK("lockWaits") {
			values[series] = float64(len(waits))
		}
	}
	switch {
	case metrics["mysql"] != nil:
		m := object(metrics["mysql"])
		copyValues(m, map[string]string{"mysqlClients": "connections", "mysqlRunning": "running"})
		putWaits("mysqlWaits", m["lockWaits"])
		if channels, ok := m["replication"].([]any); ok && signalOK("replication") {
			values["mysqlChannels"] = float64(len(channels))
		}
	case metrics["dm8"] != nil:
		m := object(metrics["dm8"])
		copyValues(m, map[string]string{"dm8Sessions": "sessions", "dm8Active": "activeSessions", "dm8Transactions": "transactions"})
		copyValues(object(m["ha"]), map[string]string{"dm8ReplayGap": "replayGap", "dm8Switches": "switches"})
		putWaits("dm8Waits", m["waits"])
		files, ok := m["files"].([]any)
		if ok && m["filesTruncated"] != true && signalOK("datafiles") {
			var allocated, free float64
			for _, raw := range files {
				file := object(raw)
				allocated += number(file["allocatedBytes"])
				free += number(file["freeBytes"])
			}
			wallboardValue(values, "dm8Allocated", allocated)
			wallboardValue(values, "dm8Used", allocated-free)
		}
	case metrics["kingbase"] != nil:
		m := object(metrics["kingbase"])
		copyValues(m, map[string]string{"kingbaseSessions": "sessions", "kingbaseActive": "activeSessions",
			"kingbaseTransactions": "transactions", "kingbaseSize": "databaseBytes"})
		putWaits("kingbaseWaits", m["waits"])
		copyValues(object(m["replication"]), map[string]string{"kingbaseLag": "maxReplayLagBytes"})
		copyValues(object(m["ha"]), map[string]string{"kingbaseLag": "replayGap"})
		copyValues(object(object(m["ha"])["archive"]), map[string]string{"kingbaseArchiveFailures": "failed"})
	case metrics["redis"] != nil:
		m := object(metrics["redis"])
		copyValues(m, map[string]string{"redisOps": "ops", "redisClients": "clients", "redisMemory": "memoryBytes",
			"redisLatency": "latencyMs", "redisSlots": "slotsOk", "redisSlotsFail": "slotsFail"})
		copyValues(object(metrics["redisInterval"]), map[string]string{"redisHit": "hitPercent", "redisEvicted": "evicted", "redisRejected": "rejected"})
		if sentinel := object(m["sentinel"]); sentinel != nil {
			copyValues(sentinel, map[string]string{"redisPeers": "peers", "redisReplicas": "replicas"})
			switch sentinel["state"] {
			case "ok":
				values["redisMaster"] = 1
			case "unavailable":
				values["redisMaster"] = 0
			}
		}
	case metrics["etcd"] != nil:
		m := object(metrics["etcd"])
		copyValues(m, map[string]string{"etcdLeader": "hasLeader", "etcdPending": "proposalsPending", "etcdSize": "sizeBytes",
			"etcdQuota": "quotaBytes", "etcdInUse": "inUseBytes"})
		interval := object(metrics["etcdInterval"])
		copyValues(object(interval["wal"]), map[string]string{"etcdWAL": "meanMs", "etcdWALP95": "p95UpperMs"})
		copyValues(object(interval["backend"]), map[string]string{"etcdCommit": "meanMs", "etcdCommitP95": "p95UpperMs"})
		copyValues(object(interval["kubernetes"]), map[string]string{"etcdAPI": "meanMs", "etcdAPIP95": "p95UpperMs"})
	default:
		return false
	}
	return true
}

func wallboardPGAvailable(sample wallboardSample) bool {
	return sample.status == "ok" || object(sample.metrics["collector"])["pgStatus"] == "ok"
}

func wallboardSamePG(current, earlier map[string]any) bool {
	from, to := object(current["postgresEndpoint"]), object(earlier["postgresEndpoint"])
	start := stringValue(current["postgresStartedAt"])
	database, address := stringValue(current["databaseName"]), stringValue(from["address"])
	port, portKnown := numeric(from["port"])
	previousPort, previousPortKnown := numeric(to["port"])
	return start != "" && start == stringValue(earlier["postgresStartedAt"]) &&
		database != "" && database == stringValue(earlier["databaseName"]) && stringValue(current["role"]) == stringValue(earlier["role"]) &&
		address != "" && address == stringValue(to["address"]) && portKnown && previousPortKnown && port == previousPort
}

func wallboardDiskLatency(values map[string]float64, linux, previous map[string]any) {
	boot, bootKnown := numeric(linux["bootTimeSeconds"])
	previousBoot, previousBootKnown := numeric(previous["bootTimeSeconds"])
	if previous["status"] != "ok" || linux["endpoint"] != previous["endpoint"] ||
		!bootKnown || !previousBootKnown || boot != previousBoot {
		return
	}
	devices := object(object(linux["devices"])["disk"])
	priorDevices := object(object(previous["devices"])["disk"])
	numbers := object(linux["deviceNumbers"])
	priorNumbers := object(previous["deviceNumbers"])
	for _, direction := range []struct{ counter, time, value string }{
		{"reads", "readSeconds", "diskReadMs"}, {"writes", "writeSeconds", "diskWriteMs"},
	} {
		var operations, spent float64
		for device := range devices {
			identity := stringValue(object(numbers[device])["number"])
			if identity == "" || identity != stringValue(object(priorNumbers[device])["number"]) {
				operations = 0
				break
			}
			current, prior := object(devices[device]), object(priorDevices[device])
			count, countKnown := storageDelta(current, prior, direction.counter)
			seconds, secondsKnown := storageDelta(current, prior, direction.time)
			if !countKnown || !secondsKnown || count < 0 || seconds < 0 {
				operations = 0
				break
			}
			operations += count
			spent += seconds
		}
		if operations > 0 {
			values[direction.value] = 1000 * spent / operations
		}
	}
}
