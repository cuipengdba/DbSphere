package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/jackc/pgx/v5"
)

type alertRule struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Metric          string  `json:"metric"`
	Source          string  `json:"source"`
	Group           string  `json:"group"`
	Severity        string  `json:"severity"`
	Operator        string  `json:"operator"`
	Threshold       float64 `json:"threshold"`
	DurationSeconds int     `json:"durationSeconds"`
	Version         int     `json:"version"`
	Enabled         bool    `json:"enabled"`
}

var goDefaultRules = []alertRule{
	{"pg_unavailable", "PostgreSQL 无法连接", "pg.unavailable", "PostgreSQL", "availability", "critical", ">=", 1, 0, 1, true},
	{"pg_collection_failed", "PostgreSQL 采集失败", "collector.pg_failed", "collector", "telemetry", "warning", ">=", 1, 0, 1, true},
	{"collector_stale", "采样数据陈旧", "collector.missed_intervals", "collector", "telemetry", "warning", ">=", 2, 0, 1, true},
	{"k8s_collection_failed", "Kubernetes 采集失败", "collector.k8s_failed", "Kubernetes API", "telemetry", "warning", ">=", 1, 0, 1, true},
	{"linux_collection_failed", "Linux 主机采集失败", "collector.linux_failed", "node_exporter", "telemetry", "warning", ">=", 1, 0, 1, true},
	{"pg_connections_high", "PostgreSQL 连接使用率高", "pg.connection_utilization", "PostgreSQL", "capacity", "warning", ">=", 85, 120, 1, true},
	{"pg_database_growth_fast", "PostgreSQL 数据库容量增长过快", "pg.database_growth_bytes_per_day", "PostgreSQL 18 历史样本", "capacity", "warning", ">=", 10 * 1024 * 1024 * 1024, 120, 1, true},
	{"pg_lock_wait", "PostgreSQL 会话被锁阻塞", "pg.blocked_sessions", "PostgreSQL", "latency", "warning", ">=", 1, 60, 1, true},
	{"pg_long_transaction", "PostgreSQL 长事务", "pg.long_transactions", "PostgreSQL", "latency", "warning", ">=", 1, 300, 1, true},
	{"pg_replication_lag", "PostgreSQL 复制延迟", "pg.replication_lag_bytes", "PostgreSQL", "replication", "warning", ">=", 64 * 1024 * 1024, 120, 1, true},
	{"pg_wal_receiver_down", "PostgreSQL WAL 接收中断", "pg.wal_receiver_down", "PostgreSQL", "replication", "critical", ">=", 1, 60, 1, true},
	{"pg_archive_failures", "PostgreSQL WAL 归档失败增加", "pg.archive_fail_delta", "PostgreSQL", "storage", "warning", ">=", 1, 0, 1, true},
	{"pg_autovacuum_disabled", "PostgreSQL autovacuum 已关闭", "pg.autovacuum_disabled", "PostgreSQL", "maintenance", "critical", ">=", 1, 0, 1, true},
	{"pg_vacuum_backlog", "PostgreSQL 表死元组积压", "pg.vacuum_backlog_percent", "PostgreSQL", "maintenance", "warning", ">=", 20, 300, 1, true},
	{"linux_cpu_high", "Linux CPU 使用率高", "linux.cpu_used_percent", "node_exporter", "capacity", "warning", ">=", 90, 300, 1, true},
	{"linux_memory_high", "Linux 内存使用率高", "linux.memory_used_percent", "node_exporter", "capacity", "warning", ">=", 90, 300, 1, true},
	{"linux_disk_high", "Linux 文件系统使用率高", "linux.disk_used_percent", "node_exporter", "storage", "warning", ">=", 90, 300, 1, true},
	{"k8s_pod_not_ready", "Kubernetes Pod 未就绪", "k8s.pod_not_ready", "Kubernetes API", "availability", "critical", ">=", 1, 60, 1, true},
	{"k8s_pod_unscheduled", "Kubernetes Pod 调度失败", "k8s.pod_unscheduled", "Kubernetes API", "availability", "critical", ">=", 1, 60, 1, true},
	{"k8s_scheduling_event", "Kubernetes 近期调度或挂载失败", "k8s.scheduling_failure_recent", "Kubernetes Event", "availability", "warning", ">=", 1, 0, 1, true},
	{"k8s_pod_restarted", "Kubernetes 容器重启", "k8s.restart_delta", "Kubernetes API", "availability", "warning", ">=", 1, 0, 1, true},
	{"k8s_oom_killed", "Kubernetes 容器发生 OOMKilled", "k8s.oom_recent", "Kubernetes API", "availability", "critical", ">=", 1, 0, 1, true},
	{"k8s_node_pressure", "Kubernetes Node 资源压力", "k8s.node_pressure", "Kubernetes API", "capacity", "warning", ">=", 1, 60, 1, true},
	{"k8s_pvc_high", "Kubernetes PVC 使用率高", "k8s.pvc_used_percent", "Prometheus kubelet", "storage", "warning", ">=", 85, 300, 1, true},
	{"pgbouncer_waiting", "PgBouncer 客户端等待", "pgbouncer.waiting_clients", "PgBouncer", "latency", "warning", ">=", 1, 60, 1, true},
}

func seedGoRules(ctx context.Context, tx pgx.Tx) error {
	for _, rule := range goDefaultRules {
		body, err := json.Marshal(rule)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO alert_rules(id,definition_json)
			VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, rule.ID, body); err != nil {
			return err
		}
	}
	return nil
}

type alertState struct {
	Status         string     `json:"status"`
	PendingSince   time.Time  `json:"pendingSince"`
	FiredAt        *time.Time `json:"firedAt"`
	EventID        string     `json:"eventId"`
	MissingSince   *time.Time `json:"missingSince"`
	LastValue      float64    `json:"lastValue"`
	LastObservedAt time.Time  `json:"lastObservedAt"`
	MutedBy        *string    `json:"mutedBy"`
}

type alertSignal struct {
	Available bool
	Value     float64
	Source    string
	Reason    string
	Detail    any
}

func numeric(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

func supportedAlertMetric(metric string) bool {
	switch metric {
	case "pg.unavailable", "collector.pg_failed", "collector.missed_intervals",
		"collector.k8s_failed", "collector.linux_failed",
		"pg.connection_utilization", "pg.database_growth_bytes_per_day", "pg.blocked_sessions", "pg.long_transactions",
		"pg.replication_lag_bytes", "pg.wal_receiver_down", "pg.archive_fail_delta", "pg.autovacuum_disabled",
		"pg.vacuum_backlog_percent", "linux.cpu_used_percent",
		"linux.memory_used_percent", "linux.disk_used_percent",
		"k8s.pod_not_ready", "k8s.pod_unscheduled", "k8s.scheduling_failure_recent", "k8s.restart_delta",
		"k8s.oom_recent", "k8s.node_pressure", "k8s.pvc_used_percent", "pgbouncer.waiting_clients":
		return true
	}
	return false
}

func readGoSignal(metric string, metrics map[string]any, now time.Time) alertSignal {
	return readGoSignalWithPrevious(metric, metrics, nil, now)
}

func readGoSignalWithPrevious(metric string, metrics, previous map[string]any, now time.Time) alertSignal {
	signal := alertSignal{Source: "PostgreSQL", Reason: "signal unavailable"}
	switch metric {
	case "pg.unavailable", "collector.pg_failed":
		status := stringValue(object(metrics["collector"])["pgStatus"])
		if metric == "collector.pg_failed" {
			signal.Source = "collector"
			if status == "ok" || status == "availability" || status == "telemetry" {
				value := 0.0
				if status == "telemetry" {
					value = 1
				}
				return alertSignal{Available: true, Value: value, Source: signal.Source}
			}
			return signal
		}
		if status == "ok" || status == "availability" {
			value := 0.0
			if status == "availability" {
				value = 1
			}
			return alertSignal{Available: true, Value: value, Source: signal.Source}
		}
	case "collector.missed_intervals":
		signal.Source = "collector"
		if value, ok := numeric(metrics["collectorMissedIntervals"]); ok && value >= 0 {
			return alertSignal{Available: true, Value: value, Source: signal.Source}
		}
	case "collector.k8s_failed", "collector.linux_failed":
		section, source := "k8s", "Kubernetes API"
		if metric == "collector.linux_failed" {
			section, source = "linux", "node_exporter"
		}
		signal.Source = source
		status := stringValue(object(metrics[section])["status"])
		if status == "ok" || status == "error" {
			value := 0.0
			if status == "error" {
				value = 1
			}
			return alertSignal{Available: true, Value: value, Source: source,
				Detail: map[string]any{"reason": object(metrics[section])["reason"]}}
		}
	case "pg.connection_utilization":
		connections, hasConnections := numeric(metrics["connections"])
		maximum, hasMaximum := numeric(metrics["max_connections"])
		if hasConnections && hasMaximum && maximum > 0 {
			return alertSignal{Available: true, Value: connections / maximum * 100, Source: "PostgreSQL"}
		}
	case "pg.database_growth_bytes_per_day":
		signal.Source = "PostgreSQL 18 历史样本"
		growth := object(metrics["databaseGrowth"])
		if stringValue(growth["status"]) == "ok" {
			if value, ok := numeric(growth["growthBytesPerDay"]); ok && value >= 0 {
				return alertSignal{Available: true, Value: value, Source: signal.Source, Detail: growth}
			}
		}
	case "pg.blocked_sessions", "pg.long_transactions":
		section, field := "locks", "blockedSessions"
		if metric == "pg.long_transactions" {
			section, field = "transactions", "longTransactions"
		}
		item := object(object(metrics["diagnostics"])[section])
		if stringValue(item["status"]) == "ok" {
			if value, ok := numeric(object(item["data"])[field]); ok {
				return alertSignal{Available: true, Value: value, Source: "PostgreSQL"}
			}
		}
	case "pg.replication_lag_bytes":
		replicas, hasReplicas := numeric(metrics["replicas"])
		if stringValue(metrics["role"]) == "primary" && metrics["replication_observed"] == true &&
			hasReplicas && replicas > 0 {
			if value, ok := numeric(metrics["replication_lag_bytes"]); ok && value >= 0 {
				return alertSignal{Available: true, Value: value, Source: signal.Source}
			}
		}
	case "pg.wal_receiver_down":
		if stringValue(metrics["role"]) == "standby" && metrics["replication_observed"] == true {
			if status := stringValue(metrics["wal_receiver"]); status != "" {
				value := 1.0
				if status == "streaming" {
					value = 0
				}
				return alertSignal{Available: true, Value: value, Source: signal.Source,
					Detail: map[string]any{"receiver": status}}
			}
		}
	case "pg.archive_fail_delta":
		current := object(object(metrics["diagnostics"])["archiver"])
		before := object(object(previous["diagnostics"])["archiver"])
		if stringValue(current["status"]) == "ok" && stringValue(before["status"]) == "ok" {
			currentCount, currentOK := numeric(object(current["data"])["failedCount"])
			beforeCount, beforeOK := numeric(object(before["data"])["failedCount"])
			if currentOK && beforeOK && currentCount >= beforeCount {
				return alertSignal{Available: true, Value: currentCount - beforeCount, Source: signal.Source}
			}
		}
	case "pg.autovacuum_disabled":
		signal.Source = "PostgreSQL pg_settings"
		settings := object(object(metrics["diagnostics"])["settings"])
		if stringValue(settings["status"]) == "ok" {
			for _, raw := range list(settings["data"]) {
				setting := object(raw)
				if stringValue(setting["name"]) == "autovacuum" {
					switch stringValue(setting["value"]) {
					case "on":
						return alertSignal{Available: true, Value: 0, Source: signal.Source}
					case "off":
						return alertSignal{Available: true, Value: 1, Source: signal.Source}
					}
				}
			}
		}
	case "pg.vacuum_backlog_percent":
		signal.Source = "PostgreSQL pg_stat_user_tables"
		vacuum := object(object(metrics["diagnostics"])["vacuum"])
		if stringValue(vacuum["status"]) == "ok" {
			best, detail := 0.0, any(nil)
			for _, raw := range list(vacuum["data"]) {
				table := object(raw)
				dead, deadOK := numeric(table["deadRowsEstimate"])
				live, liveOK := numeric(table["liveRowsEstimate"])
				if !deadOK || !liveOK || dead < 10000 || live+dead <= 0 {
					continue
				}
				percent := dead / (live + dead) * 100
				if percent > best {
					best = percent
					detail = map[string]any{"schema": table["schema"], "table": table["table"],
						"deadRowsEstimate": dead}
				}
			}
			return alertSignal{Available: true, Value: best, Source: signal.Source, Detail: detail}
		}
	case "linux.cpu_used_percent":
		signal.Source = "node_exporter"
		linux := object(metrics["linux"])
		if stringValue(linux["status"]) == "ok" {
			if value, ok := numeric(object(linux["cpu"])["usedPercent"]); ok {
				return alertSignal{Available: true, Value: value, Source: signal.Source}
			}
		}
	case "linux.memory_used_percent":
		signal.Source = "node_exporter"
		linux := object(metrics["linux"])
		if stringValue(linux["status"]) == "ok" {
			if value, ok := numeric(object(linux["memory"])["usedPercent"]); ok {
				return alertSignal{Available: true, Value: value, Source: signal.Source}
			}
		}
	case "linux.disk_used_percent":
		signal.Source = "node_exporter"
		linux := object(metrics["linux"])
		if stringValue(linux["status"]) == "ok" {
			best, mount := -1.0, ""
			for _, item := range list(linux["filesystems"]) {
				filesystem := object(item)
				if value, ok := numeric(filesystem["usedPercent"]); ok && value > best {
					best, mount = value, stringValue(filesystem["mountpoint"])
				}
			}
			if best >= 0 {
				return alertSignal{Available: true, Value: best, Source: signal.Source,
					Detail: map[string]any{"mountpoint": mount}}
			}
		}
	case "k8s.pod_not_ready", "k8s.pod_unscheduled":
		signal.Source = "Kubernetes API"
		k8s := object(metrics["k8s"])
		if stringValue(k8s["status"]) == "ok" {
			wanted := "Ready"
			if metric == "k8s.pod_unscheduled" {
				wanted = "PodScheduled"
			}
			for _, raw := range list(object(k8s["pod"])["conditions"]) {
				condition := object(raw)
				if stringValue(condition["type"]) != wanted {
					continue
				}
				switch stringValue(condition["status"]) {
				case "True":
					return alertSignal{Available: true, Value: 0, Source: signal.Source}
				case "False":
					return alertSignal{Available: true, Value: 1, Source: signal.Source,
						Detail: map[string]any{"pod": object(k8s["pod"])["name"], "reason": condition["reason"]}}
				}
			}
		}
	case "k8s.scheduling_failure_recent":
		signal.Source = "Kubernetes Event"
		k8s := object(metrics["k8s"])
		if stringValue(k8s["status"]) == "ok" && k8s["eventsError"] == nil {
			count := 0
			for _, raw := range list(k8s["events"]) {
				item := object(raw)
				reason := stringValue(item["reason"])
				at, err := time.Parse(time.RFC3339Nano, stringValue(item["at"]))
				if err == nil && !at.After(now) && now.Sub(at) <= 15*time.Minute &&
					(reason == "FailedScheduling" || reason == "FailedMount" || reason == "FailedAttachVolume") {
					count++
				}
			}
			return alertSignal{Available: true, Value: float64(count), Source: signal.Source}
		}
	case "k8s.restart_delta":
		signal.Source = "Kubernetes API"
		k8s, old := object(metrics["k8s"]), object(previous["k8s"])
		uid := stringValue(object(k8s["pod"])["uid"])
		if stringValue(k8s["status"]) == "ok" && stringValue(old["status"]) == "ok" &&
			uid != "" && uid == stringValue(object(old["pod"])["uid"]) {
			previousCounts := map[string]float64{}
			for _, raw := range list(old["containers"]) {
				container := object(raw)
				if count, ok := numeric(container["restartCount"]); ok {
					previousCounts[stringValue(container["name"])] = count
				}
			}
			delta, compared := 0.0, 0
			for _, raw := range list(k8s["containers"]) {
				container := object(raw)
				beforeCount, exists := previousCounts[stringValue(container["name"])]
				currentCount, ok := numeric(container["restartCount"])
				if !exists || !ok || currentCount < beforeCount {
					return signal
				}
				delta += currentCount - beforeCount
				compared++
			}
			if compared > 0 {
				return alertSignal{Available: true, Value: delta, Source: signal.Source,
					Detail: map[string]any{"pod": object(k8s["pod"])["name"]}}
			}
		}
	case "k8s.oom_recent":
		signal.Source = "Kubernetes API"
		k8s := object(metrics["k8s"])
		if stringValue(k8s["status"]) == "ok" && stringValue(object(k8s["pod"])["uid"]) != "" && k8s["containers"] != nil {
			for _, raw := range list(k8s["containers"]) {
				container := object(raw)
				at, err := time.Parse(time.RFC3339Nano, stringValue(container["lastTerminatedAt"]))
				if err == nil && !at.After(now) && now.Sub(at) <= 10*time.Minute &&
					stringValue(container["terminatedReason"]) == "OOMKilled" {
					return alertSignal{Available: true, Value: 1, Source: signal.Source,
						Detail: map[string]any{"pod": object(k8s["pod"])["name"], "container": container["name"], "at": at}}
				}
			}
			return alertSignal{Available: true, Value: 0, Source: signal.Source}
		}
	case "k8s.node_pressure":
		signal.Source = "Kubernetes API"
		k8s := object(metrics["k8s"])
		conditions := list(object(k8s["node"])["conditions"])
		if stringValue(k8s["status"]) == "ok" && len(conditions) > 0 {
			for _, raw := range conditions {
				condition := object(raw)
				typeName := stringValue(condition["type"])
				if (typeName == "MemoryPressure" || typeName == "DiskPressure" || typeName == "PIDPressure") &&
					stringValue(condition["status"]) == "True" {
					return alertSignal{Available: true, Value: 1, Source: signal.Source,
						Detail: map[string]any{"node": object(k8s["node"])["name"], "condition": typeName}}
				}
			}
			return alertSignal{Available: true, Value: 0, Source: signal.Source}
		}
	case "k8s.pvc_used_percent":
		signal.Source = "Prometheus kubelet"
		k8s := object(metrics["k8s"])
		if stringValue(k8s["status"]) == "ok" && k8s["pvcError"] == nil {
			best, name := -1.0, ""
			for _, raw := range list(k8s["pvcs"]) {
				pvc := object(raw)
				usage := object(pvc["usage"])
				if stringValue(usage["status"]) != "ok" {
					return signal
				}
				used, valid := numeric(usage["usedBytes"])
				capacity, err := collector.KubernetesQuantityBytes(stringValue(pvc["capacity"]))
				if !valid || err != nil || used < 0 {
					return signal
				}
				percentage := used / capacity * 100
				if percentage > best {
					best, name = percentage, stringValue(pvc["name"])
				}
			}
			if best >= 0 {
				return alertSignal{Available: true, Value: best, Source: signal.Source,
					Detail: map[string]any{"pvc": name}}
			}
		}
	case "pgbouncer.waiting_clients":
		signal.Source = "PgBouncer"
		pooler := object(metrics["pgbouncer"])
		if stringValue(pooler["status"]) == "ok" {
			total := 0.0
			for _, raw := range list(pooler["pools"]) {
				if value, ok := numeric(object(raw)["clientWaiting"]); ok {
					total += value
				}
			}
			return alertSignal{Available: true, Value: total, Source: signal.Source}
		}
	}
	return signal
}

func nextGoAlert(rule alertRule, signal alertSignal, old *alertState, now time.Time) (*alertState, string) {
	if !signal.Available {
		if old != nil && old.Status == "firing" {
			copy := *old
			if copy.MissingSince == nil {
				copy.MissingSince = &now
				return &copy, "signal_missing"
			}
			return &copy, ""
		}
		return nil, ""
	}
	breach := false
	switch rule.Operator {
	case ">=":
		breach = signal.Value >= rule.Threshold
	case ">":
		breach = signal.Value > rule.Threshold
	case "<=":
		breach = signal.Value <= rule.Threshold
	case "<":
		breach = signal.Value < rule.Threshold
	}
	if !breach {
		if old != nil && old.Status == "firing" {
			return nil, "recovered"
		}
		return nil, ""
	}
	pendingSince := now
	if old != nil && !old.PendingSince.IsZero() {
		pendingSince = old.PendingSince
	}
	firing := now.Sub(pendingSince) >= time.Duration(rule.DurationSeconds)*time.Second ||
		old != nil && old.Status == "firing"
	next := &alertState{Status: "pending", PendingSince: pendingSince,
		LastValue: signal.Value, LastObservedAt: now}
	if old != nil {
		next.EventID, next.FiredAt, next.MutedBy = old.EventID, old.FiredAt, old.MutedBy
	}
	if firing {
		next.Status = "firing"
		if next.FiredAt == nil {
			next.FiredAt = &now
		}
		if old == nil || old.Status != "firing" {
			return next, "firing"
		}
	}
	return next, ""
}

func (s *Postgres) applyGoAlerts(ctx context.Context, tx pgx.Tx, assetID, sampleID string,
	metrics, previous map[string]any, now time.Time, onlyRuleID string) error {
	rows, err := tx.Query(ctx, `SELECT definition_json FROM alert_rules ORDER BY id`)
	if err != nil {
		return err
	}
	rules := []alertRule{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var rule alertRule
		if json.Unmarshal(raw, &rule) == nil && supportedAlertMetric(rule.Metric) {
			rules = append(rules, rule)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	states := map[string]*alertState{}
	stateRows, err := tx.Query(ctx, `SELECT rule_id,state_json FROM alert_states WHERE asset_id=$1`, assetID)
	if err != nil {
		return err
	}
	for stateRows.Next() {
		var ruleID string
		var raw []byte
		if err := stateRows.Scan(&ruleID, &raw); err != nil {
			stateRows.Close()
			return err
		}
		var state alertState
		if json.Unmarshal(raw, &state) == nil {
			states[ruleID] = &state
		}
	}
	if err := stateRows.Err(); err != nil {
		stateRows.Close()
		return err
	}
	stateRows.Close()
	var genericSilence *string
	ruleSilences := map[string]string{}
	silences, err := tx.Query(ctx, `SELECT id::text,rule_id FROM silences
		WHERE revoked_at IS NULL AND starts_at<=$1 AND ends_at>$1
		AND (asset_id IS NULL OR asset_id=$2) ORDER BY created_at DESC`, now, assetID)
	if err != nil {
		return err
	}
	for silences.Next() {
		var id string
		var ruleID *string
		if err := silences.Scan(&id, &ruleID); err != nil {
			silences.Close()
			return err
		}
		if ruleID == nil && genericSilence == nil {
			genericSilence = &id
		} else if ruleID != nil {
			if _, exists := ruleSilences[*ruleID]; !exists {
				ruleSilences[*ruleID] = id
			}
		}
	}
	if err := silences.Err(); err != nil {
		silences.Close()
		return err
	}
	silences.Close()
	for _, rule := range rules {
		if onlyRuleID != "" && rule.ID != onlyRuleID {
			continue
		}
		old := states[rule.ID]
		if !rule.Enabled {
			if old != nil {
				if _, err := tx.Exec(ctx, `DELETE FROM alert_states WHERE asset_id=$1 AND rule_id=$2`,
					assetID, rule.ID); err != nil {
					return err
				}
				if err := s.finishGoAlert(ctx, tx, assetID, rule, old, sampleID, now, "disabled"); err != nil {
					return err
				}
			}
			continue
		}
		signal := readGoSignalWithPrevious(rule.Metric, metrics, previous, now)
		next, transition := nextGoAlert(rule, signal, old, now)
		if next != nil {
			next.MutedBy = genericSilence
			if id, exists := ruleSilences[rule.ID]; exists {
				next.MutedBy = &id
			}
		}
		if transition == "firing" {
			eventID, err := s.openGoAlertEvent(ctx, tx, assetID, rule, now)
			if err != nil {
				return err
			}
			next.EventID = eventID
		}
		if next != nil {
			body, err := json.Marshal(next)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO alert_states(asset_id,rule_id,state_json)
				VALUES($1,$2,$3) ON CONFLICT(asset_id,rule_id)
				DO UPDATE SET state_json=excluded.state_json`, assetID, rule.ID, body); err != nil {
				return err
			}
		} else if old != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM alert_states WHERE asset_id=$1 AND rule_id=$2`, assetID, rule.ID); err != nil {
				return err
			}
		}
		if transition == "recovered" || transition == "disabled" {
			if err := s.finishGoAlert(ctx, tx, assetID, rule, old, sampleID, now, transition); err != nil {
				return err
			}
			continue
		}
		if transition == "firing" && next.MutedBy != nil {
			transition = "suppressed"
		} else if old != nil && old.Status == "firing" && next != nil &&
			old.MutedBy != nil && next.MutedBy == nil {
			transition = "unsuppressed"
		} else if old != nil && old.Status == "firing" && next != nil &&
			old.MutedBy == nil && next.MutedBy != nil {
			transition = "suppressed"
		}
		if transition != "" && next != nil && next.EventID != "" {
			payload, _ := json.Marshal(map[string]any{"ruleId": rule.ID, "ruleVersion": rule.Version,
				"metric": rule.Metric, "severity": rule.Severity, "source": signal.Source,
				"value": signal.Value, "threshold": rule.Threshold, "operator": rule.Operator,
				"sampleId": sampleID, "detail": signal.Detail, "reason": signal.Reason,
				"silenceId": next.MutedBy})
			var evidenceID string
			if err := tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
				VALUES(gen_random_uuid(),$1,$2,$3,$4) RETURNING id::text`, next.EventID,
				"alert_"+transition, now, payload).Scan(&evidenceID); err != nil {
				return err
			}
			if transition == "firing" || transition == "unsuppressed" {
				if err := queueGoNotifications(ctx, tx, evidenceID, now); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Postgres) openGoAlertEvent(ctx context.Context, tx pgx.Tx, assetID string, rule alertRule, now time.Time) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND event_group=$2
		AND status='open' ORDER BY started_at DESC LIMIT 1 FOR UPDATE`, assetID, rule.Group).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, assetID, now).Scan(&id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'alert_incident',$3,'open',$4,$5)`, id, assetID, rule.Title, now, rule.Group); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO open_event_groups(asset_id,event_group,event_id)
		VALUES($1,$2,$3) ON CONFLICT(asset_id,event_group)
		DO UPDATE SET event_id=excluded.event_id`, assetID, rule.Group, id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Postgres) finishGoAlert(ctx context.Context, tx pgx.Tx, assetID string, rule alertRule,
	old *alertState, sampleID string, now time.Time, transition string) error {
	if old == nil || old.EventID == "" {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"ruleId": rule.ID, "ruleVersion": rule.Version,
		"metric": rule.Metric, "sampleId": sampleID, "severity": rule.Severity,
		"source": rule.Source, "value": old.LastValue})
	var evidenceID string
	if err := tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,$2,$3,$4) RETURNING id::text`, old.EventID,
		"alert_"+transition, now, payload).Scan(&evidenceID); err != nil {
		return err
	}
	if transition == "recovered" && old.MutedBy == nil {
		if err := queueGoNotifications(ctx, tx, evidenceID, now); err != nil {
			return err
		}
	}
	var firing int
	if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM alert_states
		WHERE asset_id=$1 AND state_json->>'eventId'=$2 AND state_json->>'status'='firing'`,
		assetID, old.EventID).Scan(&firing); err != nil {
		return err
	}
	if firing == 0 {
		var resolved bool
		if err := tx.QueryRow(ctx, `UPDATE events SET status='resolved',ended_at=$1
			WHERE id=$2 AND status='open' AND (kind='alert_incident' OR
			EXISTS(SELECT 1 FROM evidence WHERE event_id=$2 AND kind='collection_recovered'
			AND observed_at >= coalesce((SELECT max(observed_at) FROM evidence
			WHERE event_id=$2 AND kind='collection_error'),'-infinity'::timestamptz)))
			RETURNING true`, now, old.EventID).Scan(&resolved); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if resolved {
			if _, err := tx.Exec(ctx, `DELETE FROM open_event_groups WHERE event_id=$1`, old.EventID); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateGoRuleUpdate(rule alertRule) error {
	if rule.ID == "" || !supportedAlertMetric(rule.Metric) || rule.Title == "" ||
		rule.DurationSeconds < 0 || rule.DurationSeconds > 86400 ||
		rule.Threshold < 0 || rule.Threshold > 1e12 ||
		(rule.Severity != "info" && rule.Severity != "warning" && rule.Severity != "critical") ||
		(rule.Operator != ">=" && rule.Operator != ">" && rule.Operator != "<=" && rule.Operator != "<") {
		return fmt.Errorf("invalid Go alert rule")
	}
	return nil
}

var ErrAlertRuleNotFound = errors.New("alert rule not found")
var ErrAlertRuleUnsupported = errors.New("alert rule collector is not available in Go")
var ErrInvalidAlertRule = errors.New("invalid alert rule update")

type AlertRulePatch struct {
	Enabled         *bool    `json:"enabled"`
	Severity        *string  `json:"severity"`
	Operator        *string  `json:"operator"`
	Threshold       *float64 `json:"threshold"`
	DurationSeconds *int     `json:"durationSeconds"`
}

func (s *Postgres) UpdateAlertRule(ctx context.Context, id string, patch AlertRulePatch) (any, error) {
	if patch.Enabled == nil && patch.Severity == nil && patch.Operator == nil &&
		patch.Threshold == nil && patch.DurationSeconds == nil {
		return nil, ErrInvalidAlertRule
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT definition_json FROM alert_rules WHERE id=$1 FOR UPDATE`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlertRuleNotFound
	}
	if err != nil {
		return nil, err
	}
	var rule alertRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		return nil, err
	}
	if !supportedAlertMetric(rule.Metric) {
		return nil, ErrAlertRuleUnsupported
	}
	if patch.Enabled != nil {
		rule.Enabled = *patch.Enabled
	}
	if patch.Severity != nil {
		rule.Severity = *patch.Severity
	}
	if patch.Operator != nil {
		rule.Operator = *patch.Operator
	}
	if patch.Threshold != nil {
		rule.Threshold = *patch.Threshold
	}
	if patch.DurationSeconds != nil {
		rule.DurationSeconds = *patch.DurationSeconds
	}
	if validateGoRuleUpdate(rule) != nil {
		return nil, ErrInvalidAlertRule
	}
	rule.Version++
	body, err := json.Marshal(rule)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE alert_rules SET definition_json=$1,updated_at=now()
		WHERE id=$2`, body, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"rule": rule}, nil
}
