export function oomCandidate(result, error, collectionEvidenceId, k8sEvidenceId) {
  const now = Date.now();
  const recentOom = result?.k8s?.containers?.find(c => c.terminatedReason === 'OOMKilled' &&
    c.lastTerminatedAt && now - new Date(c.lastTerminatedAt).getTime() >= 0 &&
    now - new Date(c.lastTerminatedAt).getTime() < 10 * 60 * 1000);
  const connectivityError = /connection|connect |ECONN|ETIMEDOUT|timeout/i.test(error) &&
    !/authentication|password|permission denied/i.test(error);
  if (!recentOom || !connectivityError) return null;
  return {
    hypothesis: `Pod 容器 ${recentOom.name} 最近发生 OOMKilled，可能导致 PostgreSQL 连接失败`,
    confidence: 'medium', supports: [collectionEvidenceId, k8sEvidenceId],
    causeObservedAt: recentOom.lastTerminatedAt,
    limitation: '时间关联不等于因果；仍需核对容器日志、探针和服务端点。'
  };
}
