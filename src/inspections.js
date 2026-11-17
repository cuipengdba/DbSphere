import { readSignal } from './alert-rules.js';

const check = (id, title, metric, threshold, severity = 'warning') =>
  Object.freeze({ id, title, metric, operator: '>=', threshold, severity });

export const DEFAULT_INSPECTION_TEMPLATES = Object.freeze([
  {
    id: 'pg_daily', title: 'PostgreSQL 每日基础巡检', scope: 'all', version: 1, catalogVersion: 2,
    checks: [
      check('unavailable', '数据库无法连接', 'pg.unavailable', 1, 'critical'),
      check('monitor_permission', '监控账号缺少 pg_monitor', 'pg.monitor_role_missing', 1),
      check('pg14_eol', 'PostgreSQL 14 已结束上游维护', 'pg.pg14_eol', 1),
      check('connections', '连接使用率超过 85%', 'pg.connection_utilization', 85),
      check('locks', '存在被锁阻塞的会话', 'pg.blocked_sessions', 1),
      check('long_transactions', '存在长事务', 'pg.long_transactions', 1),
      check('archive_failures', 'WAL 归档失败计数增加', 'pg.archive_fail_delta', 1),
      check('freeze_age', '事务冻结年龄达到阈值的 80%', 'pg.freeze_age_percent', 80, 'critical'),
      check('inactive_slots', '存在未活跃复制槽', 'pg.inactive_slots', 1),
      check('slot_retention', '复制槽保留 WAL 超过 10 GiB', 'pg.slot_retained_bytes', 10 * 1024 ** 3),
      check('lost_slots', '存在失效复制槽', 'pg.lost_slots', 1, 'critical')
    ]
  },
  {
    id: 'linux_daily', title: 'Linux 主机每日巡检', scope: 'linux', version: 1,
    checks: [
      check('collector', 'Linux 主机采集失败', 'collector.linux_failed', 1),
      check('cpu', 'CPU 使用率超过 90%', 'linux.cpu_used_percent', 90),
      check('memory', '内存使用率超过 90%', 'linux.memory_used_percent', 90),
      check('disk', '文件系统使用率超过 85%', 'linux.disk_used_percent', 85)
    ]
  },
  {
    id: 'k8s_daily', title: 'Kubernetes 工作负载每日巡检', scope: 'k8s', version: 1,
    checks: [
      check('collector', 'Kubernetes API 采集失败', 'collector.k8s_failed', 1),
      check('pod', 'Pod 未就绪', 'k8s.pod_not_ready', 1, 'critical'),
      check('oom', '容器近期 OOMKilled', 'k8s.oom_recent', 1, 'critical'),
      check('node', 'Node 存在资源压力', 'k8s.node_pressure', 1),
      check('pvc', 'PVC 使用率超过 85%', 'k8s.pvc_used_percent', 85)
    ]
  },
  {
    id: 'backup_daily', title: '备份与恢复准备巡检', scope: 'all', version: 1,
    checks: [
      check('backup_age', '最近成功备份超过 24 小时', 'backup.age_hours', 24, 'critical'),
      check('repository', '备份仓库状态异常', 'backup.repository_problem', 1, 'critical'),
      check('last_attempt', '最近备份尝试失败', 'backup.last_attempt_failed', 1, 'critical')
    ]
  }
].map(template => Object.freeze({ ...template, checks: Object.freeze(template.checks) })));

function signal(check, sample, previous) {
  const metrics = sample?.metrics ?? {};
  if (check.metric.startsWith('backup.')) {
    const backup = metrics.backup;
    const missing = reason => ({ status: 'unavailable', source: 'pgBackRest info', reason });
    if (backup?.status !== 'ok') return missing(backup?.reason ?? '备份适配器未接入');
    if (check.metric === 'backup.repository_problem') return typeof backup.repositoryHealthy === 'boolean'
      ? { status: 'ok', value: backup.repositoryHealthy ? 0 : 1, source: backup.source }
      : missing('备份仓库健康状态未采集');
    if (check.metric === 'backup.last_attempt_failed') return backup.lastAttemptStatus
      ? { status: 'ok', value: backup.lastAttemptStatus === 'failed' ? 1 : 0, source: backup.source }
      : missing('最近备份尝试状态未提供');
    const age = backup.lastSuccessAt ? (new Date(sample.collectedAt).getTime() - new Date(backup.lastSuccessAt).getTime()) / 3600000 : NaN;
    return Number.isFinite(age) && age >= 0
      ? { status: 'ok', value: Number(age.toFixed(1)), source: backup.source }
      : missing('最近成功备份时间未提供');
  }
  if (check.metric === 'pg.freeze_age_percent') {
    const section = sample?.status === 'ok' ? metrics.diagnostics?.freeze : null;
    return section?.status === 'ok' && Number.isFinite(section.data?.usedPercent)
      ? { status: 'ok', value: section.data.usedPercent, source: 'PostgreSQL pg_database' }
      : { status: 'unavailable', reason: section?.reason ?? '事务冻结年龄未采集', source: 'PostgreSQL' };
  }
  if (['pg.inactive_slots', 'pg.slot_retained_bytes', 'pg.lost_slots'].includes(check.metric)) {
    const section = sample?.status === 'ok' ? metrics.diagnostics?.slotDetails : null;
    if (section?.status !== 'ok' || !Array.isArray(section.data)) {
      return { status: 'unavailable', reason: section?.reason ?? '复制槽明细未采集', source: 'PostgreSQL' };
    }
    const slots = section.data;
    if (check.metric === 'pg.inactive_slots') return { status: 'ok', value: slots.filter(item => !item.active).length,
      source: 'PostgreSQL pg_replication_slots' };
    if (check.metric === 'pg.lost_slots') return { status: 'ok', value: slots.filter(item => item.walStatus === 'lost').length,
      source: 'PostgreSQL pg_replication_slots' };
    const values = slots.map(item => item.retainedBytes).filter(Number.isFinite);
    return !slots.length || values.length ? { status: 'ok', value: Math.max(0, ...values),
      source: 'PostgreSQL pg_replication_slots' }
      : { status: 'unavailable', reason: '复制槽保留量在当前角色上不可用', source: 'PostgreSQL' };
  }
  if (check.metric === 'pg.monitor_role_missing') return sample?.status === 'ok' && typeof metrics.monitor_role === 'boolean'
    ? { status: 'ok', value: metrics.monitor_role ? 0 : 1, source: 'PostgreSQL' }
    : { status: 'unavailable', reason: '监控角色权限未采集', source: 'PostgreSQL' };
  if (check.metric === 'pg.pg14_eol') return sample?.status === 'ok' && Number.isInteger(metrics.version_num)
    ? { status: 'ok', value: Math.floor(metrics.version_num / 10000) === 14 ? 1 : 0, source: 'PostgreSQL' }
    : { status: 'unavailable', reason: 'PostgreSQL 版本未识别', source: 'PostgreSQL' };
  return readSignal(check.metric, metrics, previous?.metrics, sample?.error ?? null,
    new Date(sample?.collectedAt ?? Date.now()).getTime());
}

export function evaluateInspection(template, asset, sample, previous, priorRun = null) {
  if (!template || !asset || !sample || (template.scope !== 'all' && template.scope !== asset.runtime)) {
    throw new Error('Inspection template, asset or sample is unavailable');
  }
  const prior = new Map(priorRun?.findings?.map(item => [item.id, item.status]) ?? []);
  const findings = template.checks.map(item => {
    const observed = signal(item, sample, previous);
    const status = observed.status !== 'ok' ? 'unknown'
      : observed.value >= item.threshold ? 'risk' : 'pass';
    return { id: item.id, title: item.title, status, severity: item.severity,
      metric: item.metric, observed: observed.status === 'ok' ? observed.value : null,
      threshold: item.threshold, source: observed.source, reason: observed.reason ?? null,
      detail: observed.detail ?? null, previousStatus: prior.get(item.id) ?? null };
  });
  return { findings, summary: { risk: findings.filter(f => f.status === 'risk').length,
    pass: findings.filter(f => f.status === 'pass').length,
    unknown: findings.filter(f => f.status === 'unknown').length,
    changed: findings.filter(f => f.previousStatus && f.previousStatus !== f.status).length } };
}

export function validateInspectionTemplateUpdate(template, input) {
  if (!template || !input || typeof input !== 'object' || Array.isArray(input) ||
    Object.keys(input).length !== 1 || !input.thresholds || typeof input.thresholds !== 'object' ||
    Array.isArray(input.thresholds)) return null;
  const ids = new Set(template.checks.map(item => item.id));
  const entries = Object.entries(input.thresholds);
  if (!entries.length || entries.some(([id, threshold]) => !ids.has(id) ||
    typeof threshold !== 'number' || !Number.isFinite(threshold) || threshold < 0 || threshold > 1e12)) return null;
  return { ...template, version: template.version + 1,
    checks: template.checks.map(item => ({ ...item,
      threshold: Object.hasOwn(input.thresholds, item.id) ? input.thresholds[item.id] : item.threshold })) };
}

export function upgradeInspectionTemplate(current, builtin) {
  if (!current || (current.catalogVersion ?? 1) >= (builtin.catalogVersion ?? 1)) return null;
  const oldChecks = new Map(current.checks.map(item => [item.id, item]));
  return { ...builtin, version: current.version + 1,
    checks: builtin.checks.map(item => ({ ...item,
      threshold: oldChecks.get(item.id)?.threshold ?? item.threshold })) };
}
