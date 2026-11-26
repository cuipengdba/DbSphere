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
	route := object(metrics["routing"])
	if entry := stringValue(route["entry"]); entry != "" {
		entryNode := "route:" + sample.ID
		add(entryNode, "network_entry", entry, source, sample.At)
		*edges = append(*edges, map[string]string{"from": sampleNode, "to": entryNode, "relation": "probed_entry"})
		probe := object(route["probe"])
		if backend := stringValue(probe["backend"]); backend != "" {
			backendNode := "route-backend:" + sample.ID
			add(backendNode, "postgres", backend+" · "+stringValue(probe["role"]), source, sample.At)
			*edges = append(*edges, map[string]string{"from": entryNode, "to": backendNode, "relation": "observed_backend"})
		}
	}
	if linux := object(metrics["linux"]); stringValue(linux["status"]) == "ok" {
		hostNode := "linux:" + assetID
		add(hostNode, "linux", "Linux 主机", source, sample.At)
		*edges = append(*edges, map[string]string{"from": sampleNode, "to": hostNode, "relation": "collected_with"})
	}
	storage := object(metrics["containerStorage"])
	for name, raw := range object(storage["containers"]) {
		container := object(raw)
		if container["status"] != "ok" {
			continue
		}
		node := "container:" + stringValue(container["id"])
		add(node, "container", name+" · "+stringValue(container["source"]), source, sample.At)
		*edges = append(*edges, map[string]string{"from": sampleNode, "to": node, "relation": "collected_with"})
		if uid := stringValue(storage["podUID"]); uid != "" {
			*edges = append(*edges, map[string]string{"from": node, "to": "pod:" + uid, "relation": "collected_with"})
		}
		for device := range object(container["deviceRates"]) {
			deviceNode := "container-disk:" + stringValue(storage["node"]) + ":" + device
			add(deviceNode, "block_device", device, source, sample.At)
			*edges = append(*edges, map[string]string{"from": node, "to": deviceNode, "relation": "collected_with"})
		}
	}
	if pooler := object(metrics["pgbouncer"]); stringValue(pooler["status"]) == "ok" {
		poolerNode := "pooler:" + assetID
		add(poolerNode, "pgbouncer", "PgBouncer", source, sample.At)
		*edges = append(*edges, map[string]string{"from": sampleNode, "to": poolerNode, "relation": "collected_with"})
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
				relation := "routes_to"
				if route["status"] == "ready_pod_entry_unreachable" && object(route["service"])["uid"] == service["uid"] {
					relation = "selects_pod"
					*edges = append(*edges, map[string]string{"from": "route:" + sample.ID, "to": serviceID, "relation": "configured_service"})
				}
				*edges = append(*edges, map[string]string{"from": serviceID, "to": podNode, "relation": relation})
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
	candidateIndex := map[string]int{}
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
		against := []string{}
		if at.Before(event.StartedAt) {
			relation = "before_event"
		} else if at.After(event.StartedAt.Add(5 * time.Minute)) {
			relation = "after_event"
			against = append(against, "当前支撑信号的观测时间晚于事件开始超过 5 分钟；缺少先发证据，不能单独解释最初异常")
			if confidence == "high" || confidence == "medium" {
				confidence = "low"
			}
		}
		conditions := []any{}
		if condition != nil {
			conditions = append(conditions, condition)
		}
		candidate := map[string]any{
			"code": code, "title": title, "confidence": confidence,
			"supports": supports, "against": against, "missing": missing,
			"limitation": limitation, "detail": detail, "state": "hypothesis",
			"matchedConditions": conditions, "review": reviews[code],
			"timeOrder": map[string]any{"firstObservedAt": at, "eventStartedAt": event.StartedAt, "relation": relation},
		}
		candidates = append(candidates, candidate)
		candidateIndex[code] = len(candidates) - 1
		candidateNode := "candidate:" + code
		add(candidateNode, "candidate", title, "/api/events/"+event.ID, at)
		*edges = append(*edges, map[string]string{"from": "event:" + event.ID, "to": candidateNode, "relation": "hypothesizes"})
		for _, support := range supports {
			supportNode := support
			if !strings.HasPrefix(support, "sample:") && !strings.HasPrefix(support, "container:") {
				supportNode = "evidence:" + support
			}
			*edges = append(*edges, map[string]string{"from": candidateNode, "to": supportNode, "relation": "supported_by"})
		}
		if review := reviews[code]; review != nil {
			*edges = append(*edges, map[string]string{"from": candidateNode,
				"to": "evidence:" + stringValue(review["evidenceId"]), "relation": "reviewed_by"})
		}
	}
	appendCandidateEvidence := func(code string, supports []string, condition any) {
		index, ok := candidateIndex[code]
		if !ok {
			return
		}
		candidate := candidates[index]
		existing := candidate["supports"].([]string)
		for _, support := range supports {
			found := false
			for _, current := range existing {
				if current == support {
					found = true
					break
				}
			}
			if found {
				continue
			}
			existing = append(existing, support)
			supportNode := support
			if !strings.HasPrefix(support, "sample:") {
				supportNode = "evidence:" + support
			}
			*edges = append(*edges, map[string]string{"from": "candidate:" + code,
				"to": supportNode, "relation": "supported_by"})
		}
		candidate["supports"] = existing
		if condition != nil {
			candidate["matchedConditions"] = append(candidate["matchedConditions"].([]any), condition)
		}
	}
	for _, item := range event.Evidence {
		if engine, action := nativeHAEvent(item.Kind); engine != "" {
			var payload map[string]any
			if json.Unmarshal(item.Payload, &payload) != nil {
				continue
			}
			supports := []string{item.ID}
			for _, key := range []string{"sampleId", "previousSampleId"} {
				if id := stringValue(payload[key]); id != "" {
					supports = append(supports, "sample:"+id)
				}
			}
			label := nativeHALabel(engine)
			switch action {
			case "role":
				addCandidate(item.Kind, label+" 已观测到主备角色变化", "high", supports,
					[]string{"对照切换前后节点身份、角色及恢复采样"}, "同一个节点在相邻两个有效样本中报告了不同角色。", nil, item.ObservedAt, nil)
			case "replication":
				title := label + " 已观测到复制异常或回放积压"
				if engine == "mysql" {
					title = "MySQL 接收线程、应用线程或复制工作线程报告异常"
				}
				addCandidate(item.Kind, title, "high", supports,
					[]string{"对照发送、落盘和回放位置，再核对同窗主机磁盘 I/O 和存储事件"},
					"数据库原生视图提供复制状态或日志位点，根因还需核对同窗证据。", nil, item.ObservedAt, nil)
			case "archive":
				addCandidate(item.Kind, label+" 已报告归档异常", "high", supports,
					[]string{"核对归档状态、容量和归档错误日志"}, "数据库原生归档状态或错误时间给出异常证据。", nil, item.ObservedAt, nil)
			case "entry":
				addCandidate(item.Kind, "MySQL 写入口探测异常", "medium", supports,
					[]string{"核对入口后端 UUID、全局只读状态和代理配置"},
					"证据来自采集器通过配置入口建立的只读会话。", object(payload["route"]), item.ObservedAt, nil)
			}
			continue
		}
		if item.Kind == etcdLeaderGroup || item.Kind == etcdHealthGroup || item.Kind == etcdDiskGroup {
			var payload map[string]any
			if json.Unmarshal(item.Payload, &payload) != nil {
				continue
			}
			supports := []string{item.ID}
			if id := stringValue(payload["sampleId"]); id != "" {
				supports = append(supports, "sample:"+id)
			}
			switch item.Kind {
			case etcdLeaderGroup:
				addCandidate("etcd_leader_observed", "etcd 观测到 Leader 中断或切换", "high", supports,
					[]string{"核对同一集群的 Leader ID 和 Raft 任期"}, "原始采样包含成员报告的 Leader 状态。", nil, item.ObservedAt, nil)
			case etcdHealthGroup:
				addCandidate("etcd_health_observed", "etcd 端点报告健康检查失败", "high", supports,
					[]string{"核对该成员及其他成员的健康与磁盘指标"}, "成员的 /health 返回失败。", nil, item.ObservedAt, nil)
			case etcdDiskGroup:
				addCandidate("etcd_disk_observed", "etcd 的 WAL 或后端提交延迟升高", "high", supports,
					[]string{"核对同窗磁盘延迟、积压提案和 API Server 请求耗时"}, "同一成员的累计分布在采样间报告更长的平均耗时。", nil, item.ObservedAt, nil)
			}
			continue
		}
		if item.Kind == redisEvictionGroup || item.Kind == redisSentinelGroup || item.Kind == redisSlotsGroup {
			var payload map[string]any
			if json.Unmarshal(item.Payload, &payload) != nil {
				continue
			}
			supports := []string{item.ID}
			if id := stringValue(payload["sampleId"]); id != "" {
				supports = append(supports, "sample:"+id)
			}
			switch item.Kind {
			case redisEvictionGroup:
				if count := number(payload["value"]); count > 0 {
					addCandidate("redis_eviction_confirmed", fmt.Sprintf("Redis 在采样间淘汰 %.0f 个键", count), "high", supports,
						[]string{"核对同窗内存上限、内存用量和命中率"}, "同一 Redis 进程的原生淘汰计数增加。", nil, item.ObservedAt, nil)
				}
			case redisSentinelGroup:
				addCandidate("redis_sentinel_master_observed", "Sentinel 已观测到主节点异常或切换", "high", supports,
					[]string{"对照故障和恢复采样里的主节点身份"}, "Sentinel 返回主节点和组状态。", nil, item.ObservedAt, nil)
			case redisSlotsGroup:
				addCandidate("redis_slots_observed", "Redis Cluster 报告槽位异常", "high", supports,
					[]string{"核对故障槽位和分片节点状态"}, "目标节点返回的 CLUSTER INFO 显示集群状态异常。", nil, item.ObservedAt, nil)
			}
			continue
		}
		if item.Kind == kingbaseLockGroup {
			var payload map[string]any
			if json.Unmarshal(item.Payload, &payload) == nil && len(list(payload["waits"])) > 0 {
				wait := object(list(payload["waits"])[0])
				title := "已观测到金仓 V9 锁阻塞"
				holder, waiter := stringValue(wait["blockingSession"]), stringValue(wait["waitingSession"])
				if holder == "0" {
					title = fmt.Sprintf("预备事务持锁，阻塞金仓会话 %s", waiter)
				} else if holder != "" && waiter != "" {
					title = fmt.Sprintf("金仓会话 %s 持锁，阻塞会话 %s", holder, waiter)
				}
				supports := []string{item.ID}
				if id := stringValue(payload["sampleId"]); id != "" {
					supports = append(supports, "sample:"+id)
				}
				addCandidate("kingbase_session_lock", title, "high", supports,
					[]string{"核对持锁事务的开始时间和提交路径"}, "金仓锁视图及阻塞会话函数确认了采样瞬间的阻塞关系。", nil, item.ObservedAt, nil)
			}
			continue
		}
		if item.Kind == dm8LockGroup {
			var payload map[string]any
			if json.Unmarshal(item.Payload, &payload) == nil && len(list(payload["waits"])) > 0 {
				wait := object(list(payload["waits"])[0])
				title := "已观测到达梦 DM8 事务锁阻塞"
				if holder, waiter := stringValue(wait["blockingSession"]), stringValue(wait["waitingSession"]); holder != "" && waiter != "" {
					title = fmt.Sprintf("达梦会话 %s 持锁，阻塞会话 %s", holder, waiter)
				}
				supports := []string{item.ID}
				if id := stringValue(payload["sampleId"]); id != "" {
					supports = append(supports, "sample:"+id)
				}
				addCandidate("dm8_transaction_lock", title, "high", supports,
					[]string{"核对持锁事务的开始时间和提交路径"}, "事务等待视图确认了采样瞬间的阻塞关系。", nil, item.ObservedAt, nil)
			}
			continue
		}
		if item.Kind == mysqlLockGroup {
			var payload map[string]any
			if json.Unmarshal(item.Payload, &payload) == nil && len(list(payload["waits"])) > 0 {
				wait := object(list(payload["waits"])[0])
				title := "已观测到 InnoDB 持锁阻塞关系"
				if holder, waiter := number(wait["blockingThread"]), number(wait["waitingThread"]); holder > 0 && waiter > 0 {
					title = fmt.Sprintf("InnoDB 线程 %.0f 持锁，阻塞线程 %.0f", holder, waiter)
				}
				supports := []string{item.ID}
				if sampleID := stringValue(payload["sampleId"]); sampleID != "" {
					supports = append(supports, "sample:"+sampleID)
				}
				addCandidate("mysql_innodb_lock_wait", title, "high", supports,
					[]string{"核对持锁事务的开始时间和提交路径"}, "证据确认采样时的直接阻塞关系。", nil, item.ObservedAt, nil)
			}
			continue
		}
		if item.Kind != "root_cause_candidate" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(item.Payload, &payload) != nil {
			continue
		}
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
	}
	for _, item := range event.Evidence {
		if item.Kind != "k8s_state" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(item.Payload, &payload) != nil {
			continue
		}
		sampleID := stringValue(payload["sampleId"])
		sample := samples[sampleID]
		if stringValue(object(sample.Metrics["collector"])["pgStatus"]) != "availability" {
			continue
		}
		k8s := object(sample.Metrics["k8s"])
		pod := object(k8s["pod"])
		if stringValue(k8s["status"]) != "ok" || stringValue(pod["uid"]) == "" {
			continue
		}
		for _, raw := range list(pod["conditions"]) {
			state := object(raw)
			if state["type"] == "Ready" && state["status"] == "False" {
				addCandidate("k8s_pod_not_ready", "PostgreSQL 连通异常时观测到 Pod 未就绪", "low",
					[]string{item.ID, "sample:" + sampleID}, []string{"缺少可验证探针失败与连通异常先后顺序的记录"},
					"Pod 未就绪和数据库异常由同一次采样记录。", map[string]any{"pod": pod["name"], "uid": pod["uid"], "reason": state["reason"]},
					sample.At, map[string]any{"metric": "Pod Ready", "observed": "False", "sampleId": sampleID})
				break
			}
		}
	}
	type storageSignal struct {
		evidenceID, sampleID, metric string
		at                           time.Time
		value                        float64
		baselineID                   string
	}
	var growthAlerts, pressureAlerts []storageSignal
	for _, item := range event.Evidence {
		var payload map[string]any
		if json.Unmarshal(item.Payload, &payload) != nil {
			continue
		}
		if item.Kind != "alert_firing" && item.Kind != "alert_unsuppressed" && item.Kind != "routing_observed" {
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
		if code, containerMetric := strings.CutPrefix(metric, "container."); containerMetric {
			if title, known := storageFindings[code]; known && number(payload["value"]) > 0 {
				detail := object(payload["detail"])
				if id := stringValue(detail["containerId"]); id != "" {
					supports = append(supports, "container:"+id)
				}
				addCandidate(metric, title, "medium", supports,
					[]string{"在历史分析复核 PostgreSQL 等待、容器缓存与设备 I/O 的先后顺序", "验证恢复后相同信号是否一并回落"},
					"跨层候选来自同一采样窗口。PVC 挂载关联仅定位对象，卷级归因以实际指标为准。", detail, item.ObservedAt, condition)
			}
			continue
		}
		switch metric {
		case "pg.database_growth_bytes_per_day":
			growth := object(sample.Metrics["databaseGrowth"])
			if sample.Status == "ok" && stringValue(growth["status"]) == "ok" &&
				stringValue(growth["baselineSampleId"]) != "" && number(payload["value"]) > 0 {
				growthAlerts = append(growthAlerts, storageSignal{evidenceID: item.ID,
					sampleID: sampleID, metric: metric, at: item.ObservedAt,
					value: number(payload["value"]), baselineID: stringValue(growth["baselineSampleId"])})
			}
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
		case "pg.replication_lag_bytes":
			lag, valid := numeric(sample.Metrics["replication_lag_bytes"])
			if stringValue(sample.Metrics["role"]) == "primary" && valid && lag >= 64*1024*1024 {
				addCandidate("replication_lag", "PostgreSQL 复制回放延迟与告警同时出现", "medium", supports,
					[]string{"复制端资源与 WAL 传输原因未验证"},
					"延迟现象已确认，造成延迟的上游原因尚未定位。",
					map[string]any{"lagBytes": lag}, item.ObservedAt, condition)
			}
		case "pg.logical_slot_retained_bytes":
			signal := readGoSignal(metric, sample.Metrics, item.ObservedAt)
			slot := object(signal.Detail)
			if signal.Available && signal.Value > 0 && signal.Value == number(payload["value"]) && stringValue(slot["name"]) != "" {
				addCandidate("logical_slot_wal_retention", "逻辑复制槽保留 WAL", "medium", supports,
					[]string{"核对订阅状态、应用位置与上游连接"},
					"主库采样已定位保留 WAL 的槽。订阅停用、同步积压或网络异常需要订阅侧证据。",
					slot, item.ObservedAt, condition)
			}
		case "pg.wal_receiver_down":
			if stringValue(sample.Metrics["role"]) == "standby" &&
				sample.Metrics["replication_observed"] == true &&
				stringValue(sample.Metrics["wal_receiver"]) != "streaming" {
				addCandidate("wal_receiver_down", "备库 WAL 接收器中断", "medium", supports,
					[]string{"主库发送端和网络状态未关联"},
					"备库已观测接收中断，不能单凭备库侧信号判断主库或网络根因。",
					map[string]any{"receiver": sample.Metrics["wal_receiver"]}, item.ObservedAt, condition)
			}
		case "patroni.leader_count_invalid":
			cluster := object(sample.Metrics["patroni"])
			leaders, ok := numeric(cluster["leaderCount"])
			if stringValue(cluster["status"]) == "ok" && ok && leaders != 1 {
				addCandidate("patroni_leader_anomaly", "Patroni 集群 leader 数量异常", "medium", supports,
					[]string{"DCS 仲裁状态和各成员本地 Patroni 状态尚未核对"},
					"集群拓扑异常已由只读 API 观察到；导致异常的 DCS、节点或网络原因尚未证实。",
					map[string]any{"leaderCount": leaders, "members": cluster["members"]}, item.ObservedAt, condition)
			}
		case "pg.archive_fail_delta":
			archiver := object(object(sample.Metrics["diagnostics"])["archiver"])
			if stringValue(archiver["status"]) == "ok" && number(payload["value"]) > 0 {
				report := object(sample.Metrics["backup"])
				gaps := []any{}
				for _, entry := range list(report["walChecks"]) {
					if number(object(entry)["missingCount"]) > 0 {
						gaps = append(gaps, entry)
					}
				}
				addCandidate("wal_archive_failures", "WAL 归档失败增加", "medium", supports,
					[]string{"核对归档命令错误日志和缺失段的后续入库记录"},
					"失败计数增加提示恢复风险。同一采样中的仓库缺口可逐段回查。",
					map[string]any{"failureDelta": payload["value"], "stanza": report["stanza"], "walGaps": gaps}, item.ObservedAt, condition)
			}
		case "pg.autovacuum_disabled":
			if number(payload["value"]) == 1 {
				backlog := 0
				vacuum := object(object(sample.Metrics["diagnostics"])["vacuum"])
				if stringValue(vacuum["status"]) == "ok" {
					for _, raw := range list(vacuum["data"]) {
						if number(object(raw)["deadRowsEstimate"]) >= 10000 {
							backlog++
						}
					}
				}
				confidence := "low"
				if backlog > 0 {
					confidence = "medium"
				}
				addCandidate("autovacuum_disabled", "autovacuum 关闭，表维护可能积压", confidence, supports,
					[]string{"未验证参数关闭与业务变慢的先后顺序"},
					"参数关闭是风险因素，只有结合表积压和时间线才能分析业务影响。",
					map[string]any{"backlogTables": backlog}, item.ObservedAt, condition)
			}
		case "pg.vacuum_backlog_percent":
			if number(payload["value"]) >= 20 {
				table := object(payload["detail"])
				addCandidate("vacuum_backlog", "PostgreSQL 表死元组积压", "medium", supports,
					[]string{"核对同一对象的维护阈值、维护前后采样和查询影响"},
					"死元组统计给出了积压对象。对象工作台提供维护阈值和前后观测。",
					map[string]any{"percent": payload["value"], "table": table["table"], "schema": table["schema"],
						"tableOid": table["tableOid"], "databaseOid": table["databaseOid"]}, item.ObservedAt, condition)
			}
		case "linux.cpu_used_percent":
			if stringValue(object(sample.Metrics["linux"])["status"]) == "ok" && number(payload["value"]) >= 90 {
				addCandidate("linux_cpu_pressure", "Linux 主机 CPU 使用率高", "low", supports,
					[]string{"尚未证明 PostgreSQL 进程是 CPU 压力来源"},
					"主机 CPU 压力与事件同期出现，不能单独解释数据库慢请求。",
					map[string]any{"usedPercent": payload["value"]}, item.ObservedAt, condition)
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
				pressureAlerts = append(pressureAlerts, storageSignal{evidenceID: item.ID,
					sampleID: sampleID, metric: metric, at: item.ObservedAt, value: number(payload["value"])})
				addCandidate("linux_disk_pressure", "Linux 文件系统空间压力", "medium", supports,
					[]string{"PG 数据/WAL 目录与该挂载点映射未验证"},
					"空间压力与事件同期出现，尚未证明 PostgreSQL 写入已经失败。",
					map[string]any{"usedPercent": payload["value"],
						"mountpoint": object(payload["detail"])["mountpoint"]}, item.ObservedAt, condition)
			}
		case "pgbouncer.waiting_clients":
			if stringValue(object(sample.Metrics["pgbouncer"])["status"]) == "ok" && number(payload["value"]) > 0 {
				route := object(sample.Metrics["routing"])
				if route["status"] == "pool_exhausted" {
					addCandidate("pooler_exhausted", "PgBouncer 监控账号的连接池全部占用，客户端正在排队", "medium", supports,
						[]string{"需核对占用连接的事务和池容量"},
						"监控账号的池状态反映该用户和数据库的连接池。", route, item.ObservedAt, condition)
					continue
				}
				addCandidate("pooler_queue", "PgBouncer 客户端连接等待", "low", supports,
					[]string{"连接池等待与 PostgreSQL 锁/连接限制的因果关系未确认"},
					"连接池排队是延迟现象，需要与数据库会话和应用请求联合分析。",
					map[string]any{"waitingClients": payload["value"]}, item.ObservedAt, condition)
			}
		case "routing.entry_fault":
			route := object(sample.Metrics["routing"])
			switch route["status"] {
			case "misrouted":
				addCandidate("route_backend_mismatch", "代理入口返回的后端与配置预期不符", "medium", supports,
					[]string{"核对代理的后端和角色配置"}, "结论对应本次入口只读连接和资产配置。", route, item.ObservedAt, condition)
			case "ready_pod_entry_unreachable":
				addCandidate("service_entry_unreachable", "PostgreSQL Pod 已就绪，Service 入口从采集器不可达", "medium", supports,
					[]string{"核对 Service targetPort、EndpointSlice 和采集器所在网络"},
					"Service 入口与 Pod 直连是同次采样的独立观测。", route, item.ObservedAt, condition)
			case "unreachable", "dns_error":
				addCandidate("route_entry_unreachable", "入口探测未能建立 PostgreSQL 连接", "low", supports,
					[]string{"核对入口 DNS、监听端口及采集器所在网络"},
					"探测结果对应 DbSphere 采集器的网络视角。", route, item.ObservedAt, condition)
			}
		case "k8s.pvc_used_percent":
			if stringValue(object(sample.Metrics["k8s"])["status"]) == "ok" && number(payload["value"]) >= 85 {
				pressureAlerts = append(pressureAlerts, storageSignal{evidenceID: item.ID,
					sampleID: sampleID, metric: metric, at: item.ObservedAt, value: number(payload["value"])})
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
		case "k8s.oom_recent", "k8s.restart_delta":
			k8s := object(sample.Metrics["k8s"])
			if stringValue(k8s["status"]) != "ok" || number(payload["value"]) <= 0 {
				continue
			}
			if metric == "k8s.restart_delta" {
				addCandidate("k8s_pod_restart", "PostgreSQL Pod 容器发生重启", "low", supports,
					[]string{"容器终止原因与 PG 服务退化未核对"},
					"重启与事件同期出现，不能断定其为唯一故障原因。",
					map[string]any{"pod": object(k8s["pod"])["name"], "restartDelta": payload["value"]},
					item.ObservedAt, condition)
			}
			for _, raw := range list(k8s["containers"]) {
				container := object(raw)
				at, err := time.Parse(time.RFC3339Nano, stringValue(container["lastTerminatedAt"]))
				if err == nil && stringValue(container["terminatedReason"]) == "OOMKilled" &&
					!at.After(item.ObservedAt) && item.ObservedAt.Sub(at) <= 10*time.Minute {
					if seen["pod_oom_correlated"] {
						appendCandidateEvidence("pod_oom_correlated", supports, condition)
					} else {
						addCandidate("k8s_oom_killed", "PostgreSQL Pod 容器近期 OOMKilled", "medium", supports,
							[]string{"容器内存限制与数据库内存负载未核对"},
							"OOM 发生在事件时间窗内，仍需排除同时发生的其他故障。",
							map[string]any{"pod": object(k8s["pod"])["name"], "container": container["name"], "at": at},
							at, condition)
					}
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
	for i := len(growthAlerts) - 1; i >= 0 && !seen["storage_growth_pressure"]; i-- {
		growth := growthAlerts[i]
		for j := len(pressureAlerts) - 1; j >= 0; j-- {
			pressure := pressureAlerts[j]
			gap := growth.at.Sub(pressure.at)
			if gap < 0 {
				gap = -gap
			}
			if gap > 24*time.Hour {
				continue
			}
			supports := []string{growth.evidenceID, "sample:" + growth.sampleID,
				"sample:" + growth.baselineID, pressure.evidenceID}
			if pressure.sampleID != growth.sampleID {
				supports = append(supports, "sample:"+pressure.sampleID)
			}
			first := growth.at
			if pressure.at.Before(first) {
				first = pressure.at
			}
			addCandidate("storage_growth_pressure", "数据库快速增长与存储空间压力同期出现", "low",
				supports, []string{"PGDATA/WAL 目录与压力挂载点或 PVC 的映射尚未验证",
					"尚未证明增长造成 PostgreSQL 写入失败"},
				"两类信号在同一事件的 24 小时窗口出现；时间相关不能单独证明因果。",
				map[string]any{"growthBytesPerDay": growth.value, "pressureUsedPercent": pressure.value,
					"pressureMetric": pressure.metric, "baselineSampleId": growth.baselineID,
					"gapHours": gap.Hours()}, first,
				map[string]any{"metric": "pg.database_growth_bytes_per_day + " + pressure.metric,
					"observed": fmt.Sprintf("%.2f GiB/日 + %.1f%%", growth.value/(1024*1024*1024), pressure.value),
					"operator": "同窗不超过", "threshold": "24 小时",
					"growthEvidenceId":   growth.evidenceID,
					"pressureEvidenceId": pressure.evidenceID, "withinHours": 24})
			break
		}
	}
	return candidates
}
