const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const finite = value => typeof value === 'number' && Number.isFinite(value);

export function eventSampleIds(event) {
  return [...new Set(event.evidence.map(item => item.payload?.sampleId)
    .filter(id => typeof id === 'string' && uuid.test(id)))].slice(0, 200);
}

export function analyzeEvent(event, samples) {
  const eventNode = `event:${event.id}`;
  const assetNode = `asset:${event.assetId}`;
  const nodes = [
    { id: eventNode, type: 'event', label: event.title, at: event.startedAt },
    { id: assetNode, type: 'postgres', label: event.assetName }
  ];
  const edges = [{ from: eventNode, to: assetNode, relation: 'affects' }];
  const byId = new Map(samples.map(sample => [sample.id, sample]));
  const evidence = [...event.evidence].sort((a, b) =>
    String(a.observedAt).localeCompare(String(b.observedAt)));
  const missingSignals = new Set();
  const candidates = [];
  const addNode = (id, type, label, at = null) => {
    if (!nodes.some(node => node.id === id)) nodes.push({ id, type, label, at });
  };
  for (const item of evidence) {
    addNode(`evidence:${item.id}`, 'evidence', item.kind, item.observedAt);
    edges.push({ from: eventNode, to: `evidence:${item.id}`, relation: 'contains' });
    const sampleId = item.payload?.sampleId;
    if (!sampleId || !uuid.test(sampleId)) continue;
    const sample = byId.get(sampleId);
    if (!sample) { missingSignals.add(`样本 ${sampleId} 未保留或未加载`); continue; }
    const sampleNode = `sample:${sample.id}`;
    addNode(sampleNode, 'sample', sample.status, sample.collectedAt);
    edges.push({ from: `evidence:${item.id}`, to: sampleNode, relation: 'observed_in' });
    edges.push({ from: sampleNode, to: assetNode, relation: 'collected_from' });
    const k8s = sample.metrics?.k8s;
    if (k8s?.status === 'ok' && k8s.pod?.uid) {
      const podNode = `pod:${k8s.pod.uid}`;
      addNode(podNode, 'pod', k8s.pod.name, sample.collectedAt);
      edges.push({ from: sampleNode, to: podNode, relation: 'collected_with' });
      if (k8s.node?.name) {
        const nodeId = `node:${k8s.node.name}`;
        addNode(nodeId, 'k8s_node', k8s.node.name, sample.collectedAt);
        edges.push({ from: podNode, to: nodeId, relation: 'scheduled_on' });
      }
      for (const pvc of k8s.pvcs ?? []) {
        const pvcNode = `pvc:${pvc.namespace ?? k8s.pod.namespace ?? ''}:${pvc.name}`;
        addNode(pvcNode, 'pvc', pvc.name, sample.collectedAt);
        edges.push({ from: sampleNode, to: pvcNode, relation: 'collected_with' });
      }
    }
    if (sample.metrics?.linux?.status === 'ok') {
      const hostNode = `linux:${event.assetId}`;
      addNode(hostNode, 'linux', 'Linux 主机', sample.collectedAt);
      edges.push({ from: sampleNode, to: hostNode, relation: 'collected_with' });
    }
  }

  for (const item of evidence) {
    if (item.kind !== 'root_cause_candidate') continue;
    candidates.push({ code: 'pod_oom_correlated', title: item.payload.hypothesis,
      confidence: item.payload.confidence, supports: item.payload.supports ?? [],
      against: [], missing: ['容器日志或探针与故障先后顺序未验证'],
      limitation: item.payload.limitation, state: 'hypothesis' });
  }

  for (const item of evidence) {
    if (item.kind !== 'alert_firing' && item.kind !== 'alert_unsuppressed') continue;
    const sample = byId.get(item.payload?.sampleId);
    if (!sample || sample.status !== 'ok') continue;
    const metrics = sample.metrics ?? {};
    const supports = [item.id, `sample:${sample.id}`];
    if (item.payload.metric === 'pg.blocked_sessions') {
      const chain = metrics.diagnostics?.lockChain;
      if (chain?.status === 'ok' && chain.data?.length) {
        candidates.push({ code: 'lock_chain', title: 'PostgreSQL 锁阻塞链与延迟告警同时出现',
          confidence: 'medium', supports, against: [], missing: ['阻塞会话所执行 SQL 与业务请求未关联'],
          detail: { pairs: chain.data.map(row => ({ waitingPid: row.waitingPid,
            blockerPid: row.blockerPid, waitingSince: row.waitingSince })).slice(0, 20) },
          limitation: '锁链解释等待关系；尚不能证明业务慢请求的唯一根因。', state: 'hypothesis' });
      } else missingSignals.add('锁链诊断不可用或当前采样已无阻塞');
    }
    if (item.payload.metric === 'pg.connection_utilization' &&
      finite(metrics.connections) && finite(metrics.max_connections) &&
      metrics.max_connections > 0 && metrics.connections / metrics.max_connections >= 0.85) {
      candidates.push({ code: 'connection_pressure', title: 'PostgreSQL 连接数接近上限',
        confidence: 'medium', supports, against: [], missing: ['连接来源与池使用率未关联'],
        detail: { connections: metrics.connections, maxConnections: metrics.max_connections },
        limitation: '高使用率是容量风险，尚未证明已发生连接拒绝。', state: 'hypothesis' });
    }
    if (item.payload.metric === 'pg.replication_lag_bytes' &&
      finite(metrics.replication_lag_bytes) && metrics.replication_lag_bytes >= 64 * 1024 * 1024) {
      candidates.push({ code: 'replication_lag', title: 'PostgreSQL 复制回放延迟与告警同时出现',
        confidence: 'medium', supports, against: [], missing: ['复制端资源与 WAL 传输原因未验证'],
        detail: { lagBytes: metrics.replication_lag_bytes },
        limitation: '确认延迟现象，尚未定位造成延迟的上游原因。', state: 'hypothesis' });
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
  }
  if (!samples.length) missingSignals.add('事件引用的 PostgreSQL 样本不可用');
  const uniqueCandidates = [...new Map(candidates.map(candidate => [candidate.code, candidate])).values()];
  const reviews = new Map(evidence.filter(item => item.kind === 'rca_decision')
    .map(item => [item.payload.code, { decision: item.payload.decision,
      actor: item.payload.actor, reason: item.payload.reason, at: item.observedAt, evidenceId: item.id }]));
  for (const candidate of uniqueCandidates) {
    candidate.review = reviews.get(candidate.code) ?? null;
    const candidateNode = `candidate:${candidate.code}`;
    addNode(candidateNode, 'candidate', candidate.title);
    edges.push({ from: eventNode, to: candidateNode, relation: 'hypothesizes' });
    for (const support of candidate.supports) {
      const supportNode = support.startsWith('sample:') ? support : `evidence:${support}`;
      if (nodes.some(node => node.id === supportNode)) {
        edges.push({ from: candidateNode, to: supportNode, relation: 'supported_by' });
      }
    }
    if (candidate.review) edges.push({ from: candidateNode,
      to: `evidence:${candidate.review.evidenceId}`, relation: 'reviewed_by' });
  }
  return { eventId: event.id, status: uniqueCandidates.length ? 'candidates' : 'unlocated',
    candidates: uniqueCandidates, missingSignals: [...missingSignals], graph: { nodes, edges },
    timeline: evidence.map(item => ({ evidenceId: item.id, kind: item.kind, at: item.observedAt })) };
}
