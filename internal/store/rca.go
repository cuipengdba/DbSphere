package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type sampleObservation struct {
	ID      string
	At      time.Time
	Status  string
	Metrics map[string]any
}

type graphAdder func(id, kind, label, source string, at any)

func object(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func list(value any) []any {
	result, _ := value.([]any)
	return result
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func number(value any) float64 {
	result, _ := value.(float64)
	return result
}

func addSampleLayers(assetID string, sample sampleObservation, add graphAdder, edges *[]map[string]string) {
	source := "/api/assets/" + assetID + "/samples/" + sample.ID
	sampleNode := "sample:" + sample.ID
	metrics := sample.Metrics
	if linux := object(metrics["linux"]); stringValue(linux["status"]) == "ok" {
		hostNode := "linux:" + assetID
		add(hostNode, "linux", "Linux 主机", source, sample.At)
		*edges = append(*edges, map[string]string{"from": sampleNode, "to": hostNode, "relation": "collected_with"})
	}
	if k8s := object(metrics["k8s"]); stringValue(k8s["status"]) == "ok" {
		pod := object(k8s["pod"])
		if uid := stringValue(pod["uid"]); uid != "" {
			podNode := "pod:" + uid
			add(podNode, "pod", stringValue(pod["name"]), source, sample.At)
			*edges = append(*edges, map[string]string{"from": sampleNode, "to": podNode, "relation": "collected_with"})
			if nodeName := stringValue(object(k8s["node"])["name"]); nodeName != "" {
				nodeID := "node:" + nodeName
				add(nodeID, "k8s_node", nodeName, source, sample.At)
				*edges = append(*edges, map[string]string{"from": podNode, "to": nodeID, "relation": "scheduled_on"})
			}
			for _, row := range list(k8s["pvcs"]) {
				pvc := object(row)
				name := stringValue(pvc["name"])
				if name == "" {
					continue
				}
				pvcID := "pvc:" + stringValue(pvc["namespace"]) + ":" + name
				add(pvcID, "pvc", name, source, sample.At)
				*edges = append(*edges, map[string]string{"from": sampleNode, "to": pvcID, "relation": "collected_with"})
			}
			for _, row := range list(k8s["services"]) {
				service := object(row)
				name := stringValue(service["name"])
				if name == "" {
					continue
				}
				serviceID := "service:" + stringValue(k8s["namespace"]) + ":" + name
				add(serviceID, "k8s_service", name, source, sample.At)
				*edges = append(*edges, map[string]string{"from": serviceID, "to": podNode, "relation": "routes_to"})
			}
		}
	}
	if sample.Status != "ok" && metrics["version_num"] == nil {
		return
	}
	diagnostics := object(metrics["diagnostics"])
	if chain := object(diagnostics["lockChain"]); stringValue(chain["status"]) == "ok" {
		for _, row := range list(chain["data"]) {
			pair := object(row)
			waiterPID := fmt.Sprint(pair["waitingPid"])
			blockerPID := fmt.Sprint(pair["blockerPid"])
			if waiterPID == "<nil>" || blockerPID == "<nil>" {
				continue
			}
			waiter := "session:" + sample.ID + ":" + waiterPID
			blocker := "session:" + sample.ID + ":" + blockerPID
			add(waiter, "session", "PID "+waiterPID, source, sample.At)
			add(blocker, "session", "PID "+blockerPID, source, sample.At)
			*edges = append(*edges, map[string]string{"from": sampleNode, "to": waiter, "relation": "observed_session"},
				map[string]string{"from": waiter, "to": blocker, "relation": "blocked_by"})
		}
	}
	if sql := object(metrics["sqlPerformance"]); stringValue(sql["status"]) == "ok" {
		for _, row := range list(sql["statements"]) {
			statement := object(row)
			fingerprint := stringValue(statement["fingerprint"])
			if fingerprint == "" {
				continue
			}
			sqlNode := "sql:" + assetID + ":" + fingerprint
			add(sqlNode, "sql_fingerprint", "Query ID "+fmt.Sprint(statement["queryId"]), source, sample.At)
			*edges = append(*edges, map[string]string{"from": sampleNode, "to": sqlNode, "relation": "observed_sql"})
		}
	}
}

func analyzeCandidates(event *eventRecord, samples map[string]sampleObservation, add graphAdder, edges *[]map[string]string) []map[string]any {
	candidates := []map[string]any{}
	seen := map[string]bool{}
	reviews := map[string]map[string]any{}
	for _, item := range event.Evidence {
		if item.Kind != "rca_decision" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(item.Payload, &payload) == nil {
			reviews[stringValue(payload["code"])] = map[string]any{
				"decision": payload["decision"], "actor": payload["actor"],
				"reason": payload["reason"], "at": item.ObservedAt, "evidenceId": item.ID}
		}
	}
	addCandidate := func(code, title, confidence string, supports, missing []string, limitation string, detail any, at time.Time, condition any) {
		if seen[code] {
			return
		}
		seen[code] = true
		relation := "same_window"
		if at.Before(event.StartedAt) {
			relation = "before_event"
		} else if at.After(event.StartedAt.Add(5 * time.Minute)) {
			relation = "after_event"
		}
		conditions := []any{}
		if condition != nil {
			conditions = append(conditions, condition)
		}
		candidate := map[string]any{
			"code": code, "title": title, "confidence": confidence,
			"supports": supports, "against": []string{}, "missing": missing,
			"limitation": limitation, "detail": detail, "state": "hypothesis",
			"matchedConditions": conditions, "review": reviews[code],
			"timeOrder": map[string]any{"firstObservedAt": at, "eventStartedAt": event.StartedAt, "relation": relation},
		}
		candidates = append(candidates, candidate)
		candidateNode := "candidate:" + code
		add(candidateNode, "candidate", title, "/api/events/"+event.ID, at)
		*edges = append(*edges, map[string]string{"from": "event:" + event.ID, "to": candidateNode, "relation": "hypothesizes"})
		for _, support := range supports {
			supportNode := support
			if !strings.HasPrefix(support, "sample:") {
				supportNode = "evidence:" + support
			}
			*edges = append(*edges, map[string]string{"from": candidateNode, "to": supportNode, "relation": "supported_by"})
		}
		if review := reviews[code]; review != nil {
			*edges = append(*edges, map[string]string{"from": candidateNode,
				"to": "evidence:" + stringValue(review["evidenceId"]), "relation": "reviewed_by"})
		}
	}
	for _, item := range event.Evidence {
		var payload map[string]any
		if json.Unmarshal(item.Payload, &payload) != nil {
			continue
		}
		if item.Kind == "root_cause_candidate" {
			title := stringValue(payload["hypothesis"])
			if title == "" {
				continue
			}
			confidence := stringValue(payload["confidence"])
			if confidence == "" {
				confidence = "low"
			}
			supports := []string{item.ID}
			for _, raw := range list(payload["supports"]) {
				if id := stringValue(raw); id != "" {
					supports = append(supports, id)
				}
			}
			causeAt := item.ObservedAt
			if observed, err := time.Parse(time.RFC3339Nano, stringValue(payload["causeObservedAt"])); err == nil {
				causeAt = observed
			}
			addCandidate("pod_oom_correlated", title, confidence, supports,
				[]string{"容器日志和探针与数据库故障的先后顺序未完全验证"},
				stringValue(payload["limitation"]), nil, causeAt, nil)
			continue
		}
		if item.Kind != "alert_firing" && item.Kind != "alert_unsuppressed" {
			continue
		}
		sampleID := stringValue(payload["sampleId"])
		sample, ok := samples[sampleID]
		if !ok {
			continue
		}
		metric := stringValue(payload["metric"])
		if strings.HasPrefix(metric, "pg.") && sample.Status != "ok" && sample.Metrics["version_num"] == nil {
			continue
		}
		condition := map[string]any{"metric": metric, "observed": payload["value"],
			"operator": payload["operator"], "threshold": payload["threshold"],
			"at": item.ObservedAt, "evidenceId": item.ID}
		supports := []string{item.ID, "sample:" + sampleID}
		switch metric {
		case "pg.connection_utilization":
			connections := number(sample.Metrics["connections"])
			maximum := number(sample.Metrics["max_connections"])
			if maximum > 0 && connections/maximum >= 0.85 {
				addCandidate("connection_pressure", "PostgreSQL 连接数接近上限", "medium", supports,
					[]string{"连接来源与池使用率未关联"},
					"高使用率是容量风险，尚未证明已发生连接拒绝。",
					map[string]any{"connections": connections, "maxConnections": maximum}, item.ObservedAt, condition)
			}
		case "pg.blocked_sessions":
			chain := object(object(sample.Metrics["diagnostics"])["lockChain"])
			if stringValue(chain["status"]) == "ok" && len(list(chain["data"])) > 0 {
				addCandidate("lock_chain", "PostgreSQL 锁阻塞链与延迟告警同时出现", "medium", supports,
					[]string{"阻塞会话执行 SQL 与业务请求未关联"},
					"锁链解释等待关系，尚不能证明业务慢请求的唯一根因。",
					map[string]any{"pairs": chain["data"]}, item.ObservedAt, condition)
			}
		case "linux.memory_used_percent":
			if stringValue(object(sample.Metrics["linux"])["status"]) == "ok" && number(payload["value"]) >= 90 {
				addCandidate("linux_memory_pressure", "Linux 主机内存使用率高", "low", supports,
					[]string{"尚未证明 PostgreSQL 进程是内存压力来源"},
					"主机内存压力与事件同期出现，不能单独解释数据库故障。",
					map[string]any{"usedPercent": payload["value"]}, item.ObservedAt, condition)
			}
		case "linux.disk_used_percent":
			if stringValue(object(sample.Metrics["linux"])["status"]) == "ok" && number(payload["value"]) >= 90 {
				addCandidate("linux_disk_pressure", "Linux 文件系统空间压力", "medium", supports,
					[]string{"PG 数据/WAL 目录与该挂载点映射未验证"},
					"空间压力与事件同期出现，尚未证明 PostgreSQL 写入已经失败。",
					map[string]any{"usedPercent": payload["value"],
						"mountpoint": object(payload["detail"])["mountpoint"]}, item.ObservedAt, condition)
			}
		case "pgbouncer.waiting_clients":
			if stringValue(object(sample.Metrics["pgbouncer"])["status"]) == "ok" && number(payload["value"]) > 0 {
				addCandidate("pooler_queue", "PgBouncer 客户端连接等待", "low", supports,
					[]string{"连接池等待与 PostgreSQL 锁/连接限制的因果关系未确认"},
					"连接池排队是延迟现象，需要与数据库会话和应用请求联合分析。",
					map[string]any{"waitingClients": payload["value"]}, item.ObservedAt, condition)
			}
		case "k8s.pvc_used_percent":
			if stringValue(object(sample.Metrics["k8s"])["status"]) == "ok" && number(payload["value"]) >= 85 {
				addCandidate("pvc_pressure", "Kubernetes PVC 容量压力", "medium", supports,
					[]string{"该 PVC 与 PG 数据/WAL 目录映射未验证"},
					"PVC 用量高不等于 PostgreSQL 已发生磁盘写入故障。",
					map[string]any{"usedPercent": payload["value"], "pvc": object(payload["detail"])["pvc"]},
					item.ObservedAt, condition)
			}
		case "k8s.node_pressure":
			k8s := object(sample.Metrics["k8s"])
			if stringValue(k8s["status"]) != "ok" || number(payload["value"]) != 1 {
				continue
			}
			conditions := []string{}
			for _, raw := range list(object(k8s["node"])["conditions"]) {
				item := object(raw)
				name := stringValue(item["type"])
				if stringValue(item["status"]) == "True" &&
					(name == "MemoryPressure" || name == "DiskPressure" || name == "PIDPressure") {
					conditions = append(conditions, name)
				}
			}
			if len(conditions) > 0 {
				addCandidate("k8s_node_pressure", "承载 PostgreSQL Pod 的 Node 发生资源压力", "low", supports,
					[]string{"Node 压力与 PG 性能退化的先后关系未证实"},
					"确认同 Node 压力，不能直接归因数据库故障。",
					map[string]any{"node": object(k8s["node"])["name"], "conditions": conditions},
					item.ObservedAt, condition)
			}
		case "k8s.pod_not_ready", "k8s.pod_unscheduled":
			k8s := object(sample.Metrics["k8s"])
			if stringValue(k8s["status"]) != "ok" || number(payload["value"]) != 1 {
				continue
			}
			wanted := "Ready"
			code, title := "k8s_pod_not_ready", "PostgreSQL Pod 未就绪"
			if metric == "k8s.pod_unscheduled" {
				wanted, code, title = "PodScheduled", "k8s_scheduling", "PostgreSQL Pod 未能调度到 Node"
			}
			for _, raw := range list(object(k8s["pod"])["conditions"]) {
				state := object(raw)
				if stringValue(state["type"]) == wanted && stringValue(state["status"]) == "False" {
					addCandidate(code, title, "low", supports,
						[]string{"探针、容器状态与数据库连接故障的先后顺序需核对"},
						"Pod 条件是异常现象，需要结合事件和数据库连接证据判断原因。",
						map[string]any{"pod": object(k8s["pod"])["name"], "reason": state["reason"]},
						item.ObservedAt, condition)
					break
				}
			}
		case "k8s.oom_recent":
			k8s := object(sample.Metrics["k8s"])
			if stringValue(k8s["status"]) != "ok" || number(payload["value"]) != 1 {
				continue
			}
			for _, raw := range list(k8s["containers"]) {
				container := object(raw)
				at, err := time.Parse(time.RFC3339Nano, stringValue(container["lastTerminatedAt"]))
				if err == nil && stringValue(container["terminatedReason"]) == "OOMKilled" &&
					!at.After(item.ObservedAt) && item.ObservedAt.Sub(at) <= 10*time.Minute {
					addCandidate("k8s_oom_killed", "PostgreSQL Pod 容器近期 OOMKilled", "medium", supports,
						[]string{"容器内存限制与数据库内存负载未核对"},
						"OOM 发生在事件时间窗内，仍需排除同时发生的其他故障。",
						map[string]any{"pod": object(k8s["pod"])["name"], "container": container["name"], "at": at},
						at, condition)
					break
				}
			}
		case "k8s.scheduling_failure_recent":
			k8s := object(sample.Metrics["k8s"])
			if stringValue(k8s["status"]) != "ok" || number(payload["value"]) < 1 {
				continue
			}
			reasons := []string{}
			for _, raw := range list(k8s["events"]) {
				entry := object(raw)
				reason := stringValue(entry["reason"])
				at, err := time.Parse(time.RFC3339Nano, stringValue(entry["at"]))
				if err == nil && !at.After(item.ObservedAt) && item.ObservedAt.Sub(at) <= 15*time.Minute &&
					(reason == "FailedScheduling" || reason == "FailedMount" || reason == "FailedAttachVolume") {
					reasons = append(reasons, reason)
				}
			}
			if len(reasons) > 0 {
				addCandidate("k8s_scheduling_event", "同 Pod 近期调度或卷挂载失败", "low", supports,
					[]string{"调度/挂载异常与 PG 服务退化需进一步关联"},
					"K8S Event 证明调度或挂载异常，不能单独证明数据库连接故障。",
					map[string]any{"pod": object(k8s["pod"])["name"], "reasons": reasons},
					item.ObservedAt, condition)
			}
		}
	}
	return candidates
}
