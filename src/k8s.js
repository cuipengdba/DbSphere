import * as k8s from '@kubernetes/client-node';

export function createK8sApi(asset) {
  const config = new k8s.KubeConfig();
  const path = process.env.DBSPHERE_KUBECONFIG;
  if (path) {
    config.loadFromFile(path);
    const context = config.getContexts().find(c => c.name === asset.k8s_cluster);
    if (!context) throw new Error(`K8S context ${asset.k8s_cluster} not found`);
    config.setCurrentContext(context.name);
  } else if (process.env.KUBERNETES_SERVICE_HOST) {
    config.loadFromCluster();
  } else return null;
  // A kubeconfig must never launch local credential helper processes in this service.
  config.setExecCredentialPluginPolicy({ credentialPluginPolicy: 'DenyAll' });
  return config.makeApiClient(k8s.CoreV1Api);
}

function errorText(error) { return String(error?.message ?? error).slice(0, 300); }

export async function collectK8s(asset, api = createK8sApi(asset)) {
  if (!api) return { status: 'not_configured', reason: 'Set DBSPHERE_KUBECONFIG or run DbSphere with a read-only ServiceAccount' };
  if (!asset.k8s_namespace || !asset.k8s_pod) return { status: 'unmapped', reason: 'Namespace and Pod mapping are required' };
  const pod = await api.readNamespacedPod({ namespace: asset.k8s_namespace, name: asset.k8s_pod });
  const containers = (pod.status?.containerStatuses ?? []).map(c => ({
    name: c.name, ready: c.ready, restartCount: c.restartCount ?? 0,
    waitingReason: c.state?.waiting?.reason ?? null,
    terminatedReason: c.state?.terminated?.reason ?? c.lastState?.terminated?.reason ?? null,
    lastTerminatedAt: c.lastState?.terminated?.finishedAt ?? c.state?.terminated?.finishedAt ?? null
  }));
  const pvcNames = [...new Set((pod.spec?.volumes ?? []).map(v => v.persistentVolumeClaim?.claimName).filter(Boolean))];
  const pvcs = await Promise.all(pvcNames.map(async name => {
    try {
      const pvc = await api.readNamespacedPersistentVolumeClaim({ namespace: asset.k8s_namespace, name });
      return { name, phase: pvc.status?.phase ?? null, capacity: pvc.status?.capacity?.storage ?? null };
    } catch (error) { return { name, error: errorText(error) }; }
  }));
  let events = [];
  let eventsError = null;
  if (pod.metadata?.uid) {
    try {
      const list = await api.listNamespacedEvent({ namespace: asset.k8s_namespace, fieldSelector: `involvedObject.uid=${pod.metadata.uid}`, limit: 20 });
      events = (list.items ?? []).map(e => ({ reason: e.reason ?? null, message: e.message?.slice(0, 300) ?? null, at: e.lastTimestamp ?? e.eventTime ?? e.metadata?.creationTimestamp ?? null }));
    } catch (error) { eventsError = errorText(error); }
  } else eventsError = 'Pod UID is unavailable';
  return {
    status: 'ok', cluster: asset.k8s_cluster, namespace: asset.k8s_namespace,
    pod: { name: pod.metadata?.name, uid: pod.metadata?.uid, phase: pod.status?.phase ?? null, node: pod.spec?.nodeName ?? null, owner: pod.metadata?.ownerReferences?.[0]?.name ?? null },
    containers, pvcs, events, eventsError
  };
}
