const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const finite = value => typeof value === 'number' && Number.isFinite(value);
const sampleLink = (assetId, sampleId) => `/api/assets/${assetId}/samples/${sampleId}`;
import { sqlPerformanceSummary } from './sql-trends.js';

export function eventSampleIds(event) {
  return [...new Set(event.evidence.map(item => item.payload?.sampleId)
    .filter(id => typeof id === 'string' && uuid.test(id)))].slice(0, 200);
}

export function analyzeEvent(event, samples, context = {}) {
  const eventNode = `event:${event.id}`;
  const assetNode = `asset:${event.assetId}`;
  const nodes = [
    { id: eventNode, type: 'event', label: event.title, at: event.startedAt,
      source: `/api/events/${event.id}` },
    { id: assetNode, type: 'postgres', label: event.assetName,
      source: `/api/assets/${event.assetId}` }
  ];
  const edges = [{ from: eventNode, to: assetNode, relation: 'affects' }];
  const byId = new Map(samples.map(sample => [sample.id, sample]));
  const evidence = [...event.evidence].sort((a, b) =>
    String(a.observedAt).localeCompare(String(b.observedAt)));
  const missingSignals = new Set();
  const candidates = [];
  const addNode = (id, type, label, at = null, source = null) => {
    if (!nodes.some(node => node.id === id)) nodes.push({ id, type, label, at, source });
  };
  for (const item of evidence) {
    addNode(`evidence:${item.id}`, 'evidence', item.kind, item.observedAt,
      `/api/events/${event.id}`);
    edges.push({ from: eventNode, to: `evidence:${item.id}`, relation: 'contains' });
    const sampleId = item.payload?.sampleId;
    if (!sampleId || !uuid.test(sampleId)) continue;
    const sample = byId.get(sampleId);
    if (!sample) { missingSignals.add(`样本 ${sampleId} 未保留或未加载`); continue; }
    const sampleNode = `sample:${sample.id}`;
    addNode(sampleNode, 'sample', sample.status, sample.collectedAt, sampleLink(event.assetId, sample.id));
    edges.push({ from: `evidence:${item.id}`, to: sampleNode, relation: 'observed_in' });
    edges.push({ from: sampleNode, to: assetNode, relation: 'collected_from' });
    const k8s = sample.metrics?.k8s;
    if (k8s?.status === 'ok' && k8s.pod?.uid) {
      const podNode = `pod:${k8s.pod.uid}`;
      addNode(podNode, 'pod', k8s.pod.name, sample.collectedAt, sampleLink(event.assetId, sample.id));
      edges.push({ from: sampleNode, to: podNode, relation: 'collected_with' });
      if (k8s.node?.name) {
        const nodeId = `node:${k8s.node.name}`;
        addNode(nodeId, 'k8s_node', k8s.node.name, sample.collectedAt, sampleLink(event.assetId, sample.id));
        edges.push({ from: podNode, to: nodeId, relation: 'scheduled_on' });
      }
      for (const pvc of k8s.pvcs ?? []) {
        const pvcNode = `pvc:${pvc.namespace ?? k8s.pod.namespace ?? ''}:${pvc.name}`;
        addNode(pvcNode, 'pvc', pvc.name, sample.collectedAt, sampleLink(event.assetId, sample.id));
        edges.push({ from: sampleNode, to: pvcNode, relation: 'collected_with' });
      }
      for (const [index, incident] of (k8s.events ?? []).entries()) {
        const k8sEvent = `k8s_event:${sample.id}:${index}`;
        addNode(k8sEvent, 'k8s_event', incident.reason ?? 'Event', incident.at,
          sampleLink(event.assetId, sample.id));
        edges.push({ from: podNode, to: k8sEvent, relation: 'observed_event' });
      }
    }
    if (sample.metrics?.linux?.status === 'ok') {
      const hostNode = `linux:${event.assetId}`;
      addNode(hostNode, 'linux', 'Linux 主机', sample.collectedAt, sampleLink(event.assetId, sample.id));
      edges.push({ from: sampleNode, to: hostNode, relation: 'collected_with' });
    }
  }

  const sqlByQueryId = new Map();
  for (const sample of samples) {
    if (!context.nearbySamples?.some(nearby => nearby.id === sample.id)) continue;
    const sampleNode = `sample:${sample.id}`;
    addNode(sampleNode, 'sample', sample.status, sample.collectedAt, sampleLink(event.assetId, sample.id));
    edges.push({ from: eventNode, to: sampleNode, relation: 'time_window' });
    edges.push({ from: sampleNode, to: assetNode, relation: 'collected_from' });
  }
  for (const sample of samples) {
    const sampleNode = `sample:${sample.id}`;
    if (!nodes.some(node => node.id === sampleNode)) continue;
    const diagnostics = sample.metrics?.diagnostics ?? {};
    const source = sampleLink(event.assetId, sample.id);
    for (const row of sample.status === 'ok' && diagnostics.activity?.status === 'ok' ? diagnostics.activity.data : []) {
      const waitNode = `wait:${sample.id}:${row.state}:${row.waitType}`;
      addNode(waitNode, 'wait', `${row.state} / ${row.waitType} / ${row.sessions}`, sample.collectedAt, source);
      edges.push({ from: sampleNode, to: waitNode, relation: 'observed_wait', source });
    }
    for (const row of sample.status === 'ok' && diagnostics.lockChain?.status === 'ok' ? diagnostics.lockChain.data : []) {
      const waiter = `session:${sample.id}:${row.waitingPid}`;
      const blocker = `session:${sample.id}:${row.blockerPid}`;
      addNode(waiter, 'session', `PID ${row.waitingPid}`, sample.collectedAt, source);
      addNode(blocker, 'session', `PID ${row.blockerPid}`, sample.collectedAt, source);
      edges.push({ from: sampleNode, to: waiter, relation: 'observed_session', source });
      edges.push({ from: waiter, to: blocker, relation: 'blocked_by', source });
    }
    if (sample.status === 'ok' && diagnostics.wal?.status === 'ok') {
      const walNode = `wal:${sample.id}`;
      addNode(walNode, 'wal', `WAL ${diagnostics.wal.data.bytes ?? '未知'} bytes`, sample.collectedAt, source);
      edges.push({ from: sampleNode, to: walNode, relation: 'observed_wal', source });
    }
    for (const row of sample.status === 'ok' && diagnostics.vacuum?.status === 'ok'
      ? diagnostics.vacuum.data.slice(0, 10) : []) {
      const vacuumNode = `vacuum:${sample.id}:${row.schema}:${row.table}`;
      addNode(vacuumNode, 'vacuum', `${row.schema}.${row.table} / dead ${row.deadRowsEstimate}`,
        sample.collectedAt, source);
      edges.push({ from: sampleNode, to: vacuumNode, relation: 'observed_vacuum', source });
    }
    if (sample.status === 'ok' && sample.metrics?.replication_observed) {
      const replicationNode = `replication:${sample.id}`;
      addNode(replicationNode, 'replication',
        `角色 ${sample.metrics.role} / 延迟 ${sample.metrics.replication_lag_bytes ?? '未知'} bytes`, sample.collectedAt, source);
      edges.push({ from: sampleNode, to: replicationNode, relation: 'observed_replication', source });
    }
    const statements = sample.status === 'ok' && sample.metrics?.sqlPerformance?.status === 'ok'
      ? sample.metrics.sqlPerformance.statements.slice(0, 20) : [];
    for (const statement of statements) {
      const sqlNode = `sql:${event.assetId}:${statement.fingerprint}`;
      addNode(sqlNode, 'sql_fingerprint', `Query ID ${statement.queryId}`, sample.collectedAt, source);
      edges.push({ from: sampleNode, to: sqlNode, relation: 'observed_sql', source });
      const linked = sqlByQueryId.get(String(statement.queryId)) ?? new Set();
      linked.add(sqlNode);
      sqlByQueryId.set(String(statement.queryId), linked);
    }
  }
  for (const log of context.slowLogs ?? []) {
    const logNode = `slow_log:${log.id}`;
    addNode(logNode, 'slow_log', `${log.durationMs} ms`, log.observedAt,
      `/api/assets/${event.assetId}/slow-logs/${log.id}`);
    edges.push({ from: eventNode, to: logNode, relation: 'time_window' });
    for (const sqlNode of sqlByQueryId.get(String(log.queryId)) ?? []) {
      edges.push({ from: logNode, to: sqlNode, relation: 'query_id_matches' });
    }
  }
  for (const plan of context.plans ?? []) {
    const planNode = `plan:${plan.id}`;
    addNode(planNode, 'plan', `${plan.summary.nodeCount} nodes`, plan.observedAt,
      `/api/assets/${event.assetId}/sql-plans/${plan.id}`);
    edges.push({ from: eventNode, to: planNode, relation: 'time_window' });
    for (const sqlNode of sqlByQueryId.get(String(plan.queryId)) ?? []) {
      edges.push({ from: planNode, to: sqlNode, relation: 'query_id_matches' });
    }
  }

  const traceSpans = context.traceSpans ?? [];
  const spanIds = new Set(traceSpans.map(span => `${span.traceId}:${span.spanId}`));
  for (const span of traceSpans) {
    const spanNode = `trace_span:${span.traceId}:${span.spanId}`;
    const serviceNode = `service:${span.service}`;
    const source = `/api/assets/${event.assetId}/traces/${span.traceId}`;
    addNode(serviceNode, 'service', span.service, span.start, source);
    addNode(spanNode, span.kind === 'postgresql_client' ? 'db_client_span' : 'service_span',
      `${span.service} · ${Math.max(0, Date.parse(span.end) - Date.parse(span.start))} ms · ${span.status}`,
      span.start, source);
    edges.push({ from: serviceNode, to: spanNode, relation: 'emitted', source });
    if (span.kind === 'postgresql_client') {
      edges.push({ from: spanNode, to: assetNode, relation: 'calls', source });
      edges.push({ from: eventNode, to: spanNode, relation: 'overlaps_db_call', source });
    }
    if (span.parentSpanId && spanIds.has(`${span.traceId}:${span.parentSpanId}`)) {
      edges.push({ from: `trace_span:${span.traceId}:${span.parentSpanId}`,
        to: spanNode, relation: 'parent_of', source });
    }
  }

  for (const item of evidence) {
    if (item.kind !== 'root_cause_candidate') continue;
    candidates.push({ code: 'pod_oom_correlated', title: item.payload.hypothesis,
      confidence: item.payload.confidence, supports: item.payload.supports ?? [],
      against: [], missing: ['容器日志或探针与故障先后顺序未验证'],
      causeObservedAt: item.payload.causeObservedAt ?? null,
      limitation: item.payload.limitation, state: 'hypothesis' });
  }

  for (const item of evidence) {
    if (item.kind !== 'alert_firing' && item.kind !== 'alert_unsuppressed') continue;
    const sample = byId.get(item.payload?.sampleId);
    if (!sample) continue;
    const metrics = sample.metrics ?? {};
    const pgObserved = sample.status === 'ok';
    const supports = [item.id, `sample:${sample.id}`];
    if (pgObserved && item.payload.metric === 'pg.blocked_sessions') {
      const chain = metrics.diagnostics?.lockChain;
      if (chain?.status === 'ok' && chain.data?.length) {
        candidates.push({ code: 'lock_chain', title: 'PostgreSQL 锁阻塞链与延迟告警同时出现',
          confidence: 'medium', supports, against: [], missing: ['阻塞会话所执行 SQL 与业务请求未关联'],
          detail: { pairs: chain.data.map(row => ({ waitingPid: row.waitingPid,
            blockerPid: row.blockerPid, waitingSince: row.waitingSince })).slice(0, 20) },
          limitation: '锁链解释等待关系；尚不能证明业务慢请求的唯一根因。', state: 'hypothesis' });
      } else missingSignals.add('锁链诊断不可用或当前采样已无阻塞');
    }
    if (pgObserved && item.payload.metric === 'pg.connection_utilization' &&
      finite(metrics.connections) && finite(metrics.max_connections) &&
      metrics.max_connections > 0 && metrics.connections / metrics.max_connections >= 0.85) {
      candidates.push({ code: 'connection_pressure', title: 'PostgreSQL 连接数接近上限',
        confidence: 'medium', supports, against: [], missing: ['连接来源与池使用率未关联'],
        detail: { connections: metrics.connections, maxConnections: metrics.max_connections },
        limitation: '高使用率是容量风险，尚未证明已发生连接拒绝。', state: 'hypothesis' });
    }
    if (pgObserved && item.payload.metric === 'pg.replication_lag_bytes' &&
      finite(metrics.replication_lag_bytes) && metrics.replication_lag_bytes >= 64 * 1024 * 1024) {
      candidates.push({ code: 'replication_lag', title: 'PostgreSQL 复制回放延迟与告警同时出现',
        confidence: 'medium', supports, against: [], missing: ['复制端资源与 WAL 传输原因未验证'],
        detail: { lagBytes: metrics.replication_lag_bytes },
        limitation: '确认延迟现象，尚未定位造成延迟的上游原因。', state: 'hypothesis' });
    }
    if (pgObserved && item.payload.metric === 'pg.wal_receiver_down' && metrics.role === 'standby' &&
      metrics.replication_observed && metrics.wal_receiver !== 'streaming') {
      candidates.push({ code: 'wal_receiver_down', title: '备库 WAL 接收器中断',
        confidence: 'medium', supports, against: [], missing: ['主库发送端和网络状态未关联'],
        detail: { receiver: metrics.wal_receiver ?? 'unknown' },
        limitation: '接收中断已观测，不能单凭备库侧信号判断主库或网络根因。', state: 'hypothesis' });
    }
    if (pgObserved && item.payload.metric === 'pg.archive_fail_delta' && finite(item.payload.value) &&
      item.payload.value > 0 && metrics.diagnostics?.archiver?.status === 'ok') {
      candidates.push({ code: 'wal_archive_failures', title: 'WAL 归档失败增加',
        confidence: 'medium', supports, against: [], missing: ['归档命令错误日志和仓库状态未关联'],
        detail: { failureDelta: item.payload.value },
        limitation: '归档失败计数增加是恢复风险，尚未确认失败命令原因。', state: 'hypothesis' });
    }
    if (pgObserved && item.payload.metric === 'pg.autovacuum_disabled' && item.payload.value === 1) {
      const backlog = metrics.diagnostics?.vacuum?.status === 'ok'
        ? metrics.diagnostics.vacuum.data.filter(row => row.deadRowsEstimate >= 10000) : [];
      candidates.push({ code: 'autovacuum_disabled', title: 'autovacuum 关闭，表维护可能积压',
        confidence: backlog.length ? 'medium' : 'low', supports,
        against: backlog.length ? [] : ['本次样本未发现超过一万条死元组的表'],
        missing: ['未验证该设置关闭的时间与业务变慢的先后关系'],
        detail: { backlogTables: backlog.length },
        limitation: '参数关闭是风险因素；只有结合表积压和时间线才能分析业务影响。', state: 'hypothesis' });
    }
    if (pgObserved && item.payload.metric === 'pg.vacuum_backlog_percent' &&
      finite(item.payload.value) && item.payload.value >= 20) {
      candidates.push({ code: 'vacuum_backlog', title: 'PostgreSQL 表死元组积压',
        confidence: 'medium', supports, against: [],
        missing: ['未取得表膨胀和实际查询计划的直接影响证据'],
        detail: { percent: item.payload.value, table: item.payload.detail?.table,
          schema: item.payload.detail?.schema },
        limitation: 'pg_stat_user_tables 是估计值；积压不自动证明查询退化。', state: 'hypothesis' });
    }
    if (item.payload.metric === 'linux.disk_used_percent' && finite(item.payload.value) && item.payload.value >= 90) {
      candidates.push({ code: 'linux_disk_pressure', title: 'Linux 文件系统空间压力',
        confidence: 'medium', supports, against: [], missing: ['PG 数据/WAL 目录与该挂载点映射未验证'],
        detail: { usedPercent: item.payload.value, mountpoint: item.payload.detail?.mountpoint },
        limitation: '空间压力与事件同期出现，未证明 PostgreSQL 写入已经失败。', state: 'hypothesis' });
    }
    if (item.payload.metric === 'k8s.pvc_used_percent' && finite(item.payload.value) && item.payload.value >= 85) {
      candidates.push({ code: 'pvc_pressure', title: 'Kubernetes PVC 容量压力',
        confidence: 'medium', supports, against: [], missing: ['该 PVC 与 PG 数据/WAL 目录映射未验证'],
        detail: { usedPercent: item.payload.value, pvc: item.payload.detail?.pvc },
        limitation: 'PVC 容量压力并不等同于 PostgreSQL 磁盘写入故障。', state: 'hypothesis' });
    }
    if (item.payload.metric === 'k8s.node_pressure' && item.payload.value === 1 &&
      metrics.k8s?.status === 'ok') {
      const pressures = metrics.k8s.node?.conditions?.filter(condition =>
        ['MemoryPressure', 'DiskPressure', 'PIDPressure'].includes(condition.type) &&
        condition.status === 'True') ?? [];
      if (pressures.length) candidates.push({ code: 'k8s_node_pressure',
        title: '承载 PostgreSQL Pod 的 Node 发生资源压力', confidence: 'low', supports,
        against: [], missing: ['Node 压力与 PG 性能退化的先后关系未证实'],
        detail: { node: metrics.k8s.node.name, conditions: pressures.map(row => row.type) },
        limitation: '确认同 Node 压力，不能据此直接归因数据库故障。', state: 'hypothesis' });
    }
    if (item.payload.metric === 'k8s.restart_delta' && finite(item.payload.value) &&
      item.payload.value > 0 && metrics.k8s?.status === 'ok') {
      candidates.push({ code: 'k8s_pod_restart', title: 'PostgreSQL Pod 容器发生重启',
        confidence: 'low', supports, against: [], missing: ['容器终止原因与 PG 服务退化未核对'],
        detail: { pod: metrics.k8s.pod?.name, restartDelta: item.payload.value },
        limitation: '重启与事件同期，不自动推断重启是唯一故障原因。', state: 'hypothesis' });
    }
    if (item.payload.metric === 'k8s.pod_not_ready' && item.payload.value === 1 &&
      metrics.k8s?.pod?.conditions?.some(condition => condition.type === 'Ready' && condition.status === 'False')) {
      candidates.push({ code: 'k8s_pod_not_ready', title: 'PostgreSQL Pod 未就绪',
        confidence: 'low', supports, against: [], missing: ['探针失败、容器状态或数据库连接原因未确认'],
        detail: { pod: metrics.k8s.pod.name },
        limitation: 'Ready=False 是现象，尚不能说明根因。', state: 'hypothesis' });
    }
    if (item.payload.metric === 'k8s.pod_unscheduled' && item.payload.value === 1) {
      const scheduled = metrics.k8s?.pod?.conditions?.find(condition => condition.type === 'PodScheduled');
      if (scheduled?.status === 'False') candidates.push({ code: 'k8s_scheduling',
        title: 'PostgreSQL Pod 未能调度到 Node', confidence: event.eventGroup === 'availability' ? 'medium' : 'low',
        supports, against: [], missing: ['需要核对调度器事件和数据库连接失败的先后顺序'],
        detail: { pod: metrics.k8s.pod.name, reason: scheduled.reason ?? null },
        limitation: 'PodScheduled=False 与可用性事件相关，尚需确认其为连接失败的直接原因。',
        state: 'hypothesis' });
    }
    if (item.payload.metric === 'k8s.scheduling_failure_recent' &&
      finite(item.payload.value) && item.payload.value > 0) {
      const recent = metrics.k8s?.events?.filter(k8sEvent =>
        /FailedScheduling|FailedMount|FailedAttachVolume/i.test(k8sEvent.reason ?? '') &&
        k8sEvent.at && Date.parse(k8sEvent.at) <= Date.parse(item.observedAt) &&
        Date.parse(k8sEvent.at) >= Date.parse(item.observedAt) - 15 * 60_000) ?? [];
      if (recent.length) candidates.push({ code: 'k8s_scheduling_event',
        title: '同 Pod 近期调度或卷挂载失败', confidence: 'low', supports,
        against: [], missing: ['Pod 调度状态与 PG 服务退化需进一步关联'],
        detail: { pod: metrics.k8s.pod?.name ?? null, reasons: recent.map(row => row.reason) },
        limitation: 'K8S Event 证明调度或挂载异常；不能单独证明数据库连接故障。',
        state: 'hypothesis' });
    }
  }
  if (event.eventGroup === 'latency' && Array.isArray(context.nearbySamples)) {
    const nearby = context.nearbySamples;
    for (let i = 0; i + 2 < nearby.length; i++) {
      const summary = sqlPerformanceSummary(nearby.slice(i, i + 3));
      const regressed = summary.statements?.filter(statement => statement.regression) ?? [];
      if (!regressed.length) continue;
      const top = regressed[0];
      const matchedLogs = (context.slowLogs ?? []).filter(log => String(log.queryId) === String(top.queryId));
      const matchedPlans = (context.plans ?? []).filter(plan => String(plan.queryId) === String(top.queryId));
      const supports = [`sample:${nearby[i].id}`, `sample:${nearby[i + 1].id}`,
        `sample:${nearby[i + 2].id}`,
        ...matchedLogs.slice(0, 3).map(log => `slow_log:${log.id}`),
        ...matchedPlans.slice(0, 3).map(plan => `plan:${plan.id}`)];
      candidates.push({ code: 'sql_regression', title: 'SQL 指纹平均执行时间显著退化',
        confidence: matchedLogs.length || matchedPlans.length ? 'medium' : 'low', supports,
        against: [], missing: matchedLogs.length ? [] : ['该指纹没有同时间窗慢日志记录'],
        detail: { queryId: top.queryId, previousAverageExecMs: top.regression.previousAverageExecMs,
          currentAverageExecMs: top.regression.currentAverageExecMs,
          ratio: top.regression.ratio },
        limitation: 'SQL 退化与事件同窗；锁、IO、计划变化或业务负载仍需区分。', state: 'hypothesis' });
      break;
    }
  }
  if (event.eventGroup === 'latency' && Array.isArray(context.plans)) {
    const plans = [...context.plans].sort((a, b) => Date.parse(b.observedAt) - Date.parse(a.observedAt));
    const changed = plans.find((plan, index) => plans.slice(index + 1).some(prior =>
      prior.queryId === plan.queryId && prior.shapeHash !== plan.shapeHash));
    if (changed) {
      const prior = plans.find(plan => plan.queryId === changed.queryId &&
        Date.parse(plan.observedAt) < Date.parse(changed.observedAt) && plan.shapeHash !== changed.shapeHash);
      if (prior) candidates.push({ code: 'plan_change', title: '同一 Query ID 的执行计划结构变化',
        confidence: 'low', supports: [`plan:${prior.id}`, `plan:${changed.id}`],
        against: [], missing: ['计划快照为人工导入，未确认与告警执行时刻一致'],
        detail: { queryId: changed.queryId, before: prior.shapeHash, after: changed.shapeHash },
        limitation: '计划结构变化可能解释性能差异，但未建立对业务请求的因果关系。', state: 'hypothesis' });
    }
  }
  if (!samples.length) missingSignals.add('事件引用的 PostgreSQL 样本不可用');
  const uniqueCandidates = [...new Map(candidates.map(candidate => [candidate.code, candidate])).values()];
  const reviews = new Map(evidence.filter(item => item.kind === 'rca_decision')
    .map(item => [item.payload.code, { decision: item.payload.decision,
      actor: item.payload.actor, reason: item.payload.reason, at: item.observedAt, evidenceId: item.id }]));
  for (const candidate of uniqueCandidates) {
    candidate.review = reviews.get(candidate.code) ?? null;
    candidate.matchedConditions = candidate.supports.filter(support => !support.includes(':'))
      .map(support => evidence.find(item => item.id === support))
      .filter(item => item?.kind?.startsWith('alert_') && item.payload?.metric)
      .map(item => ({ metric: item.payload.metric, observed: item.payload.value,
        operator: item.payload.operator, threshold: item.payload.threshold,
        at: item.observedAt, evidenceId: item.id }));
    if (candidate.code === 'pod_oom_correlated' && candidate.causeObservedAt) candidate.matchedConditions.push({
      metric: 'k8s.OOMKilled_before_pg_failure', observed: candidate.causeObservedAt,
      operator: '<=', threshold: event.startedAt, at: candidate.causeObservedAt });
    if (candidate.code === 'sql_regression') candidate.matchedConditions.push({
      metric: 'sql.average_exec_ms_ratio', observed: candidate.detail.ratio,
      operator: '>=', threshold: 2, at: nodes.find(node =>
        node.id === candidate.supports[0])?.at ?? null });
    if (candidate.code === 'plan_change') candidate.matchedConditions.push({
      metric: 'sql.plan_shape_changed', observed: true, operator: '=', threshold: true,
      at: nodes.find(node => node.id === candidate.supports[1])?.at ?? null });
    const supportTimes = candidate.supports.map(support => {
      const id = /^(sample|plan|slow_log|sql):/.test(support) ? support : `evidence:${support}`;
      return Date.parse(nodes.find(node => node.id === id)?.at);
    }).filter(Number.isFinite);
    if (candidate.causeObservedAt && Number.isFinite(Date.parse(candidate.causeObservedAt))) {
      supportTimes.push(Date.parse(candidate.causeObservedAt));
    }
    const first = supportTimes.length ? Math.min(...supportTimes) : null;
    candidate.timeOrder = { firstObservedAt: first === null ? null : new Date(first).toISOString(),
      eventStartedAt: event.startedAt,
      relation: first === null ? 'unknown' : first < Date.parse(event.startedAt)
        ? 'before_event' : first <= Date.parse(event.startedAt) + 5 * 60_000
          ? 'same_window' : 'after_event' };
    const candidateNode = `candidate:${candidate.code}`;
    addNode(candidateNode, 'candidate', candidate.title, null, `/api/events/${event.id}`);
    edges.push({ from: eventNode, to: candidateNode, relation: 'hypothesizes' });
    for (const support of candidate.supports) {
      const supportNode = /^(sample|plan|slow_log|sql):/.test(support) ? support : `evidence:${support}`;
      if (nodes.some(node => node.id === supportNode)) {
        edges.push({ from: candidateNode, to: supportNode, relation: 'supported_by' });
      }
    }
    if (candidate.review) edges.push({ from: candidateNode,
      to: `evidence:${candidate.review.evidenceId}`, relation: 'reviewed_by' });
  }
  return { eventId: event.id, status: uniqueCandidates.length ? 'candidates' : 'unlocated',
    candidates: uniqueCandidates, missingSignals: [...missingSignals], graph: { nodes, edges },
    traceCoverage: { spans: traceSpans.length,
      databaseSpans: traceSpans.filter(span => span.kind === 'postgresql_client').length,
      services: [...new Set(traceSpans.map(span => span.service))],
      samplingRate: 'unknown', clockOffset: 'unknown',
      gaps: traceSpans.length ? [] : ['事件时间窗内无应用 Trace'] },
    timeline: evidence.map(item => ({ evidenceId: item.id, kind: item.kind, at: item.observedAt })) };
}
