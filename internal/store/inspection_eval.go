package store

import (
	"regexp"
	"strings"
	"time"
)

type inspectionFinding struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	Severity       string   `json:"severity"`
	Metric         string   `json:"metric"`
	Observed       *float64 `json:"observed"`
	Threshold      float64  `json:"threshold"`
	Source         string   `json:"source"`
	Reason         string   `json:"reason"`
	Detail         any      `json:"detail"`
	PreviousStatus *string  `json:"previousStatus"`
}

type inspectionSummary struct {
	Risk          int `json:"risk"`
	Pass          int `json:"pass"`
	Unknown       int `json:"unknown"`
	NotApplicable int `json:"notApplicable"`
	Changed       int `json:"changed"`
}

var permissionWords = regexp.MustCompile(`(?i)401|403|forbidden|permission|unauthoriz`)
var probeWords = regexp.MustCompile(`(?i)Unhealthy|ProbeWarning|ProbeError`)

func inspectionSignal(metric string, metrics, previous map[string]any, now time.Time) (alertSignal, bool) {
	missing := func(source, reason string) (alertSignal, bool) {
		return alertSignal{Source: source, Reason: reason}, true
	}
	observed := func(value float64, source string) (alertSignal, bool) {
		return alertSignal{Available: true, Value: value, Source: source}, true
	}
	role := stringValue(metrics["role"])
	if metric == "pg.replication_lag_bytes" && (role == "standby" ||
		(role == "primary" && metrics["replication_observed"] == true && number(metrics["replicas"]) == 0)) {
		return alertSignal{Source: "PostgreSQL", Reason: "当前实例没有适用的主库复制连接"}, false
	}
	if metric == "pg.wal_receiver_down" && role == "primary" {
		return alertSignal{Source: "PostgreSQL", Reason: "主库不运行 WAL 接收器"}, false
	}
	switch metric {
	case "pg.monitor_role_missing", "pg.monitor_privileged":
		field := "monitor_role"
		if metric == "pg.monitor_privileged" {
			field = "privileged_role"
		}
		if value, ok := metrics[field].(bool); ok {
			if metric == "pg.monitor_role_missing" {
				value = !value
			}
			if value {
				return observed(1, "PostgreSQL pg_roles")
			}
			return observed(0, "PostgreSQL pg_roles")
		}
		return missing("PostgreSQL pg_roles", "监控账号权限未采集")
	case "pg.pg14_eol":
		if version, ok := numeric(metrics["version_num"]); ok {
			if int(version)/10000 == 14 {
				return observed(1, "PostgreSQL")
			}
			return observed(0, "PostgreSQL")
		}
		return missing("PostgreSQL", "PostgreSQL 版本未识别")
	case "pg.freeze_age_percent":
		freeze := object(object(metrics["diagnostics"])["freeze"])
		if stringValue(freeze["status"]) == "ok" {
			if value, ok := numeric(object(freeze["data"])["usedPercent"]); ok {
				return observed(value, "PostgreSQL pg_database")
			}
		}
		return missing("PostgreSQL pg_database", "事务冻结年龄未采集")
	case "pg.inactive_slots", "pg.slot_retained_bytes", "pg.lost_slots":
		slots := object(object(metrics["diagnostics"])["slotDetails"])
		if stringValue(slots["status"]) != "ok" {
			return missing("PostgreSQL pg_replication_slots", "复制槽明细未采集")
		}
		rows, ok := slots["data"].([]any)
		if !ok {
			return missing("PostgreSQL pg_replication_slots", "复制槽明细不完整")
		}
		value, known := 0.0, metric != "pg.slot_retained_bytes" || len(rows) == 0
		for _, raw := range rows {
			row := object(raw)
			switch metric {
			case "pg.inactive_slots":
				if row["active"] == false {
					value++
				}
			case "pg.lost_slots":
				if stringValue(row["walStatus"]) == "lost" {
					value++
				}
			case "pg.slot_retained_bytes":
				if retained, ok := numeric(row["retainedBytes"]); ok && retained >= 0 {
					known = true
					if retained > value {
						value = retained
					}
				}
			}
		}
		if !known {
			return missing("PostgreSQL pg_replication_slots", "复制槽保留量不可用")
		}
		return observed(value, "PostgreSQL pg_replication_slots")
	case "linux.permission_error":
		linux := object(metrics["linux"])
		if stringValue(linux["status"]) == "ok" {
			return observed(0, "node_exporter")
		}
		if stringValue(linux["status"]) == "error" && permissionWords.MatchString(stringValue(linux["reason"])) {
			return observed(1, "node_exporter")
		}
		return missing("node_exporter", "node_exporter 权限状态未知或未配置")
	case "k8s.probe_failure_recent", "k8s.unbound_pvc":
		k8s := object(metrics["k8s"])
		if stringValue(k8s["status"]) != "ok" {
			return missing("Kubernetes API", "Kubernetes 资源未采集")
		}
		if metric == "k8s.unbound_pvc" {
			rows, ok := k8s["pvcs"].([]any)
			if !ok {
				return missing("Kubernetes API", "PVC 状态未采集")
			}
			count := 0.0
			for _, raw := range rows {
				phase := stringValue(object(raw)["phase"])
				if phase == "" {
					return missing("Kubernetes API", "PVC 状态不完整")
				}
				if phase != "Bound" {
					count++
				}
			}
			return observed(count, "Kubernetes API")
		}
		if k8s["eventsError"] != nil {
			return missing("Kubernetes Event", "Pod 事件读取失败")
		}
		rows, ok := k8s["events"].([]any)
		if !ok {
			return missing("Kubernetes Event", "Pod 事件未采集")
		}
		count := 0.0
		for _, raw := range rows {
			item := object(raw)
			at, err := time.Parse(time.RFC3339Nano, stringValue(item["at"]))
			if err == nil && now.Sub(at) >= -15*time.Minute && now.Sub(at) <= 15*time.Minute &&
				probeWords.MatchString(stringValue(item["reason"])) {
				count++
			}
		}
		return observed(count, "Kubernetes Event")
	case "backup.age_hours", "backup.repository_problem", "backup.last_attempt_failed":
		backup := object(metrics["backup"])
		if stringValue(backup["status"]) != "ok" {
			return missing("pgBackRest info", "备份适配器未接入或采集失败")
		}
		source := stringValue(backup["source"])
		if source == "" {
			source = "pgBackRest info"
		}
		switch metric {
		case "backup.repository_problem":
			if healthy, ok := backup["repositoryHealthy"].(bool); ok {
				if healthy {
					return observed(0, source)
				}
				return observed(1, source)
			}
		case "backup.last_attempt_failed":
			if status := stringValue(backup["lastAttemptStatus"]); status != "" {
				if status == "failed" {
					return observed(1, source)
				}
				return observed(0, source)
			}
		case "backup.age_hours":
			if at, err := time.Parse(time.RFC3339Nano, stringValue(backup["lastSuccessAt"])); err == nil && !now.Before(at) {
				return observed(now.Sub(at).Hours(), source)
			}
		}
		return missing(source, "备份状态字段不完整")
	}
	if !supportedAlertMetric(metric) {
		return missing("collector", "当前 Go 版本未支持此巡检信号")
	}
	signal := readGoSignalWithPrevious(metric, metrics, previous, now)
	if !signal.Available && strings.TrimSpace(signal.Reason) == "" {
		signal.Reason = "信号未采集"
	}
	return signal, true
}

func evaluateGoInspection(template inspectionTemplate, metrics, previous map[string]any,
	now time.Time, prior map[string]string) ([]inspectionFinding, inspectionSummary) {
	findings := make([]inspectionFinding, 0, len(template.Checks))
	summary := inspectionSummary{}
	for _, check := range template.Checks {
		signal, applicable := inspectionSignal(check.Metric, metrics, previous, now)
		finding := inspectionFinding{ID: check.ID, Title: check.Title, Status: "unknown",
			Severity: check.Severity, Metric: check.Metric, Threshold: check.Threshold,
			Source: signal.Source, Reason: signal.Reason, Detail: signal.Detail}
		if !applicable {
			finding.Status = "not_applicable"
			summary.NotApplicable++
		} else if !signal.Available {
			summary.Unknown++
		} else {
			value := signal.Value
			finding.Observed = &value
			finding.Reason = ""
			finding.Status = "pass"
			breach := false
			switch check.Operator {
			case ">=":
				breach = value >= check.Threshold
			case ">":
				breach = value > check.Threshold
			case "<=":
				breach = value <= check.Threshold
			case "<":
				breach = value < check.Threshold
			default:
				finding.Status = "unknown"
				finding.Reason = "巡检比较运算符不受支持"
				summary.Unknown++
				break
			}
			if finding.Status == "unknown" {
				findings = append(findings, finding)
				continue
			}
			if breach {
				finding.Status = "risk"
				summary.Risk++
			} else {
				finding.Status = "pass"
				summary.Pass++
			}
		}
		if old, ok := prior[check.ID]; ok {
			finding.PreviousStatus = &old
			if old != finding.Status {
				summary.Changed++
			}
		}
		findings = append(findings, finding)
	}
	return findings, summary
}
