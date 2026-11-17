const fields = ['calls', 'totalExecMs', 'rows', 'sharedBlocksHit', 'sharedBlocksRead',
  'sharedBlocksWritten', 'tempBlocksRead', 'tempBlocksWritten', 'walBytes'];

function interval(newer, older) {
  if (!newer || !older || !newer.resetAt || !older.resetAt ||
    new Date(newer.resetAt).getTime() !== new Date(older.resetAt).getTime()) return null;
  const previous = new Map(older.statements.map(row => [row.fingerprint, row]));
  const deltas = new Map();
  for (const row of newer.statements) {
    const before = previous.get(row.fingerprint);
    if (!before) continue;
    const change = { fingerprint: row.fingerprint };
    if (fields.some(field => !Number.isFinite(row[field]) || !Number.isFinite(before[field]) ||
      row[field] < before[field])) continue;
    for (const field of fields) change[field] = row[field] - before[field];
    if (change.calls > 0) {
      change.averageExecMs = change.totalExecMs / change.calls;
      deltas.set(row.fingerprint, change);
    }
  }
  return deltas;
}

export function sqlPerformanceSummary(samples) {
  const snapshots = samples.filter(sample => sample.status === 'ok' &&
    sample.metrics?.sqlPerformance?.status === 'ok').slice(0, 12);
  const latest = samples[0]?.metrics?.sqlPerformance;
  if (!latest || latest.status !== 'ok') return {
    status: latest?.status ?? 'unknown', reason: latest?.reason ?? 'No SQL performance sample',
    statements: []
  };
  const current = snapshots[0]?.metrics.sqlPerformance;
  const prior = snapshots[1]?.metrics.sqlPerformance;
  const beforePrior = snapshots[2]?.metrics.sqlPerformance;
  const currentInterval = interval(current, prior);
  const priorInterval = interval(prior, beforePrior);
  const statements = current.statements.map(row => {
    const delta = currentInterval?.get(row.fingerprint) ?? null;
    const baseline = priorInterval?.get(row.fingerprint) ?? null;
    const regressed = delta && baseline && delta.calls >= 5 && baseline.calls >= 5 &&
      delta.averageExecMs >= baseline.averageExecMs * 2 &&
      delta.averageExecMs - baseline.averageExecMs >= 50;
    const trend = [];
    for (let i = snapshots.length - 2; i >= 0; i--) {
      const point = interval(snapshots[i].metrics.sqlPerformance,
        snapshots[i + 1].metrics.sqlPerformance)?.get(row.fingerprint);
      if (!point) continue;
      const metrics = snapshots[i].metrics;
      trend.push({ sampleId: snapshots[i].id, at: snapshots[i].collectedAt,
        calls: point.calls, averageExecMs: point.averageExecMs,
        linuxCpuPercent: metrics.linux?.cpu?.usedPercent ?? null,
        linuxMemoryPercent: metrics.linux?.memory?.usedPercent ?? null,
        podPhase: metrics.k8s?.pod?.phase ?? null,
        podRestarts: metrics.k8s?.containers?.reduce((total, item) =>
          total + (Number(item.restartCount) || 0), 0) ?? null });
    }
    return { ...row, delta, trend,
      regression: regressed ? { previousAverageExecMs: baseline.averageExecMs,
        currentAverageExecMs: delta.averageExecMs,
        ratio: delta.averageExecMs / baseline.averageExecMs } : null };
  }).sort((a, b) => (b.delta?.totalExecMs ?? b.totalExecMs) -
    (a.delta?.totalExecMs ?? a.totalExecMs)).slice(0, 20);
  return { status: 'ok', source: current.source, collectedAt: snapshots[0]?.collectedAt,
    sampleId: snapshots[0]?.id, resetAt: current.resetAt,
    comparison: currentInterval ? 'interval' : 'cumulative',
    truncated: current.truncated, hiddenQueryCount: current.hiddenQueryCount,
    statements };
}
