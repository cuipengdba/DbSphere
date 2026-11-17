export const RULE_PACK_VERSION = 1;

export const DEFAULT_RULES = Object.freeze([
  { id: 'pg_unavailable', title: 'PostgreSQL 无法连接', metric: 'pg.unavailable', source: 'PostgreSQL', group: 'availability', severity: 'critical', operator: '>=', threshold: 1, durationSeconds: 0 },
  { id: 'pg_collection_failed', title: 'PostgreSQL 采集失败', metric: 'collector.pg_failed', source: 'collector', group: 'telemetry', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 0 },
  { id: 'collector_stale', title: '采样数据陈旧', metric: 'collector.missed_intervals', source: 'collector', group: 'telemetry', severity: 'warning', operator: '>=', threshold: 2, durationSeconds: 0 },
  { id: 'k8s_collection_failed', title: 'Kubernetes 采集失败', metric: 'collector.k8s_failed', source: 'Kubernetes API', group: 'telemetry', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 0 },
  { id: 'linux_collection_failed', title: 'Linux 主机采集失败', metric: 'collector.linux_failed', source: 'node_exporter', group: 'telemetry', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 0 },
  { id: 'pg_connections_high', title: 'PostgreSQL 连接使用率高', metric: 'pg.connection_utilization', source: 'PostgreSQL', group: 'capacity', severity: 'warning', operator: '>=', threshold: 85, durationSeconds: 120 },
  { id: 'pg_lock_wait', title: 'PostgreSQL 会话被锁阻塞', metric: 'pg.blocked_sessions', source: 'PostgreSQL', group: 'latency', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 60 },
  { id: 'pg_long_transaction', title: 'PostgreSQL 长事务', metric: 'pg.long_transactions', source: 'PostgreSQL', group: 'latency', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 300 },
  { id: 'pg_replication_lag', title: 'PostgreSQL 复制延迟', metric: 'pg.replication_lag_bytes', source: 'PostgreSQL', group: 'replication', severity: 'warning', operator: '>=', threshold: 64 * 1024 * 1024, durationSeconds: 120 },
  { id: 'pg_wal_receiver_down', title: 'PostgreSQL WAL 接收中断', metric: 'pg.wal_receiver_down', source: 'PostgreSQL', group: 'replication', severity: 'critical', operator: '>=', threshold: 1, durationSeconds: 60 },
  { id: 'pg_archive_failures', title: 'PostgreSQL WAL 归档失败增加', metric: 'pg.archive_fail_delta', source: 'PostgreSQL', group: 'storage', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 0 },
  { id: 'linux_cpu_high', title: 'Linux CPU 使用率高', metric: 'linux.cpu_used_percent', source: 'node_exporter', group: 'capacity', severity: 'warning', operator: '>=', threshold: 90, durationSeconds: 300 },
  { id: 'linux_memory_high', title: 'Linux 内存使用率高', metric: 'linux.memory_used_percent', source: 'node_exporter', group: 'capacity', severity: 'warning', operator: '>=', threshold: 90, durationSeconds: 300 },
  { id: 'linux_disk_high', title: 'Linux 文件系统使用率高', metric: 'linux.disk_used_percent', source: 'node_exporter', group: 'storage', severity: 'warning', operator: '>=', threshold: 90, durationSeconds: 300 },
  { id: 'k8s_pod_not_ready', title: 'Kubernetes Pod 未就绪', metric: 'k8s.pod_not_ready', source: 'Kubernetes API', group: 'availability', severity: 'critical', operator: '>=', threshold: 1, durationSeconds: 60 },
  { id: 'k8s_pod_restarted', title: 'Kubernetes 容器重启', metric: 'k8s.restart_delta', source: 'Kubernetes API', group: 'availability', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 0 },
  { id: 'k8s_oom_killed', title: 'Kubernetes 容器发生 OOMKilled', metric: 'k8s.oom_recent', source: 'Kubernetes API', group: 'availability', severity: 'critical', operator: '>=', threshold: 1, durationSeconds: 0 },
  { id: 'k8s_node_pressure', title: 'Kubernetes Node 资源压力', metric: 'k8s.node_pressure', source: 'Kubernetes API', group: 'capacity', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 60 },
  { id: 'k8s_pvc_high', title: 'Kubernetes PVC 使用率高', metric: 'k8s.pvc_used_percent', source: 'Prometheus kubelet', group: 'storage', severity: 'warning', operator: '>=', threshold: 85, durationSeconds: 300 },
  { id: 'pgbouncer_waiting', title: 'PgBouncer 客户端等待', metric: 'pgbouncer.waiting_clients', source: 'PgBouncer', group: 'latency', severity: 'warning', operator: '>=', threshold: 1, durationSeconds: 60 }
].map(rule => Object.freeze({ ...rule, version: RULE_PACK_VERSION, enabled: true })));

const observed = (value, source, detail = null) => ({ status: 'ok', value, source, detail });
const unavailable = (source, reason) => ({ status: 'unavailable', value: null, source, reason });
const finite = value => typeof value === 'number' && Number.isFinite(value);
const connectionFailure = error => /connection|connect |ECONN|ETIMEDOUT|timeout|ENOTFOUND|EHOSTUNREACH/i.test(error) &&
  !/authentication|password|permission denied|unsupported|outside the supported/i.test(error);

function quantityBytes(value) {
  if (typeof value !== 'string') return null;
  const match = value.match(/^(\d+(?:\.\d+)?)(Ki|Mi|Gi|Ti|k|M|G|T)?$/);
  if (!match) return null;
  const factors = { Ki: 1024, Mi: 1024 ** 2, Gi: 1024 ** 3, Ti: 1024 ** 4,
    k: 1000, M: 1000 ** 2, G: 1000 ** 3, T: 1000 ** 4 };
  return Number(match[1]) * (factors[match[2]] ?? 1);
}

export function readSignal(metric, result, previous = null, error = null, now = Date.now()) {
  const pg = result ?? {};
  const linux = pg.linux;
  const k8s = pg.k8s;
  const pooler = pg.pgbouncer;
  switch (metric) {
    case 'collector.pg_failed': return observed(error && !connectionFailure(error) ? 1 : 0, 'collector');
    case 'collector.missed_intervals': return observed(finite(pg.collectorMissedIntervals) ? pg.collectorMissedIntervals : 0, 'collector');
    case 'collector.k8s_failed': return k8s?.status === 'ok' || k8s?.status === 'error'
      ? observed(k8s.status === 'error' ? 1 : 0, 'Kubernetes API', { reason: k8s.reason ?? null })
      : unavailable('Kubernetes API', 'Kubernetes 未配置或尚未采集');
    case 'collector.linux_failed': return linux?.status === 'ok' || linux?.status === 'error'
      ? observed(linux.status === 'error' ? 1 : 0, 'node_exporter', { reason: linux.reason ?? null })
      : unavailable('node_exporter', 'Linux 主机指标未配置或尚未采集');
    case 'pg.unavailable': {
      if (!error) return observed(0, 'PostgreSQL');
      if (/authentication|password|permission denied|unsupported|outside the supported/i.test(error)) {
        return unavailable('PostgreSQL', '采集权限、凭据或版本错误，不能据此判断实例不可用');
      }
      return connectionFailure(error)
        ? observed(1, 'PostgreSQL') : unavailable('PostgreSQL', '采集失败原因不足以证明实例不可用');
    }
    case 'pg.connection_utilization': return !error && finite(pg.connections) && finite(pg.max_connections) && pg.max_connections > 0
      ? observed(Number((pg.connections / pg.max_connections * 100).toFixed(1)), 'PostgreSQL')
      : unavailable('PostgreSQL', '连接数或 max_connections 未采集');
    case 'pg.blocked_sessions': return !error && pg.diagnostics?.locks?.status === 'ok' && finite(pg.diagnostics.locks.data?.blockedSessions)
      ? observed(pg.diagnostics.locks.data.blockedSessions, 'PostgreSQL')
      : unavailable('PostgreSQL', '锁等待诊断未采集');
    case 'pg.long_transactions': return !error && pg.diagnostics?.transactions?.status === 'ok' && finite(pg.diagnostics.transactions.data?.longTransactions)
      ? observed(pg.diagnostics.transactions.data.longTransactions, 'PostgreSQL')
      : unavailable('PostgreSQL', '长事务诊断未采集');
    case 'pg.replication_lag_bytes': return !error && pg.role === 'primary' && pg.replication_observed && pg.replicas > 0 && finite(pg.replication_lag_bytes)
      ? observed(pg.replication_lag_bytes, 'PostgreSQL')
      : unavailable('PostgreSQL', '复制回放 LSN 或复制连接未采集');
    case 'pg.wal_receiver_down': return !error && pg.role === 'standby' && pg.replication_observed
      ? observed(pg.wal_receiver === 'streaming' ? 0 : 1, 'PostgreSQL')
      : unavailable('PostgreSQL', 'WAL 接收器状态未采集或不适用');
    case 'pg.archive_fail_delta': {
      const current = pg.diagnostics?.archiver?.status === 'ok' ? pg.diagnostics.archiver.data?.failedCount : null;
      const before = previous?.diagnostics?.archiver?.status === 'ok' ? previous.diagnostics.archiver.data?.failedCount : null;
      return !error && finite(current) && finite(before) && current >= before
        ? observed(current - before, 'PostgreSQL')
        : unavailable('PostgreSQL', '缺少连续 WAL 归档失败计数');
    }
    case 'linux.cpu_used_percent': return linux?.status === 'ok' && finite(linux.cpu?.usedPercent)
      ? observed(linux.cpu.usedPercent, 'node_exporter') : unavailable('node_exporter', 'Linux CPU 使用率未采集');
    case 'linux.memory_used_percent': return linux?.status === 'ok' && finite(linux.memory?.usedPercent)
      ? observed(linux.memory.usedPercent, 'node_exporter') : unavailable('node_exporter', 'Linux 内存使用率未采集');
    case 'linux.disk_used_percent': {
      const disks = linux?.status === 'ok' ? linux.filesystems?.filter(item => finite(item.usedPercent)) : null;
      if (!disks?.length) return unavailable('node_exporter', 'Linux 文件系统使用率未采集');
      const disk = disks.reduce((a, b) => a.usedPercent >= b.usedPercent ? a : b);
      return observed(disk.usedPercent, 'node_exporter', { mountpoint: disk.mountpoint });
    }
    case 'k8s.pod_not_ready': {
      const ready = k8s?.status === 'ok' && k8s.pod?.conditions?.find(item => item.type === 'Ready');
      return ready?.status === 'True' || ready?.status === 'False'
        ? observed(ready.status === 'False' ? 1 : 0, 'Kubernetes API', { pod: k8s.pod.name })
        : unavailable('Kubernetes API', 'Pod Ready 条件未采集');
    }
    case 'k8s.restart_delta': {
      if (k8s?.status !== 'ok' || previous?.k8s?.status !== 'ok' || !k8s.pod?.uid || k8s.pod.uid !== previous.k8s.pod?.uid) {
        return unavailable('Kubernetes API', '缺少同一 Pod 的前次重启计数');
      }
      const before = new Map(previous.k8s.containers?.map(item => [item.name, item.restartCount]) ?? []);
      const delta = k8s.containers?.reduce((total, item) => total + Math.max(0, item.restartCount - (before.get(item.name) ?? item.restartCount)), 0);
      return finite(delta) ? observed(delta, 'Kubernetes API', { pod: k8s.pod.name })
        : unavailable('Kubernetes API', '容器重启计数未采集');
    }
    case 'k8s.oom_recent': {
      if (k8s?.status !== 'ok' || !k8s.pod?.uid || !Array.isArray(k8s.containers)) {
        return unavailable('Kubernetes API', 'Pod UID 或容器状态未采集');
      }
      const recent = k8s.containers.some(item => item.terminatedReason === 'OOMKilled' &&
        item.lastTerminatedAt && Math.abs(now - new Date(item.lastTerminatedAt).getTime()) <= 10 * 60 * 1000);
      return observed(recent ? 1 : 0, 'Kubernetes API', { pod: k8s.pod?.name });
    }
    case 'k8s.node_pressure': {
      const conditions = k8s?.status === 'ok' ? k8s.node?.conditions : null;
      if (!conditions?.length) return unavailable('Kubernetes API', 'Node 压力条件未采集');
      return observed(conditions.some(item => ['MemoryPressure', 'DiskPressure', 'PIDPressure'].includes(item.type) && item.status === 'True') ? 1 : 0, 'Kubernetes API', { node: k8s.node.name });
    }
    case 'k8s.pvc_used_percent': {
      const pvc = k8s?.status === 'ok' ? k8s.pvcs?.filter(item => item.usage?.status === 'ok' && finite(item.usage.usedBytes))
        .map(item => ({ name: item.name, percent: item.usage.usedBytes / quantityBytes(item.capacity) * 100 }))
        .filter(item => finite(item.percent))
        .sort((a, b) => b.percent - a.percent)[0] : null;
      return pvc ? observed(Number(pvc.percent.toFixed(1)), 'Prometheus kubelet', { pvc: pvc.name })
        : unavailable('Prometheus kubelet', 'PVC 实际使用量或容量未采集');
    }
    case 'pgbouncer.waiting_clients': return pooler?.status === 'ok' && Array.isArray(pooler.pools)
      ? observed(pooler.pools.reduce((sum, item) => sum + (Number(item.clientWaiting) || 0), 0), 'PgBouncer')
      : unavailable('PgBouncer', '连接池统计未采集');
    default: throw new Error(`Unknown alert metric: ${metric}`);
  }
}

export function compareSignal(signal, rule) {
  if (signal.status !== 'ok') return 'unknown';
  if (rule.operator === '>=') return signal.value >= rule.threshold ? 'breach' : 'normal';
  if (rule.operator === '>') return signal.value > rule.threshold ? 'breach' : 'normal';
  if (rule.operator === '<=') return signal.value <= rule.threshold ? 'breach' : 'normal';
  if (rule.operator === '<') return signal.value < rule.threshold ? 'breach' : 'normal';
  throw new Error(`Unsupported alert operator: ${rule.operator}`);
}

export function validateRuleUpdate(rule, update) {
  if (!update || typeof update !== 'object' || Array.isArray(update)) return false;
  const allowed = new Set(['enabled', 'severity', 'operator', 'threshold', 'durationSeconds']);
  if (!Object.keys(update).length || Object.keys(update).some(key => !allowed.has(key))) return false;
  const next = { ...rule, ...update };
  return typeof next.enabled === 'boolean' && ['info', 'warning', 'critical'].includes(next.severity) &&
    ['>=', '>', '<=', '<'].includes(next.operator) && finite(next.threshold) &&
    next.threshold >= 0 && next.threshold <= 1e12 && Number.isInteger(next.durationSeconds) &&
    next.durationSeconds >= 0 && next.durationSeconds <= 86400;
}
