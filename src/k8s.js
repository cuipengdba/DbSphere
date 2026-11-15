import * as k8s from '@kubernetes/client-node';

function loadConfig(asset) {
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
  return config;
}

export function createK8sApi(asset) {
  return loadConfig(asset)?.makeApiClient(k8s.CoreV1Api) ?? null;
}

function createK8sMetrics(asset) {
  const config = loadConfig(asset);
  return config ? new k8s.Metrics(config) : null;
}

function createK8sApps(asset) {
  return loadConfig(asset)?.makeApiClient(k8s.AppsV1Api) ?? null;
}

function errorText(error) { return String(error?.message ?? error).slice(0, 300); }

function readOptional(api, method, query) {
  return typeof api[method] === 'function' ? api[method](query) : Promise.reject(new Error(`${method} is unavailable`));
}

function namesAndStatus(node) {
  return {
    name: node.metadata?.name ?? null,
    conditions: (node.status?.conditions ?? []).filter(c => ['Ready', 'MemoryPressure', 'DiskPressure', 'PIDPressure'].includes(c.type))
      .map(c => ({ type: c.type, status: c.status, reason: c.reason ?? null, changedAt: c.lastTransitionTime ?? null })),
    allocatable: { cpu: node.status?.allocatable?.cpu ?? null, memory: node.status?.allocatable?.memory ?? null }
  };
}

export async function collectK8s(asset, api = createK8sApi(asset), metricsApi = null) {
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
      let volume = null;
      let volumeError = null;
      if (pvc.spec?.volumeName) {
        try {
          const pv = await readOptional(api, 'readPersistentVolume', { name: pvc.spec.volumeName });
          volume = { name: pv.metadata?.name ?? pvc.spec.volumeName, phase: pv.status?.phase ?? null,
            capacity: pv.spec?.capacity?.storage ?? null };
        } catch (error) { volumeError = errorText(error); }
      }
      return { name, phase: pvc.status?.phase ?? null, capacity: pvc.status?.capacity?.storage ?? null,
        requested: pvc.spec?.resources?.requests?.storage ?? null, volume, volumeError };
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
  let node = null;
  let nodeError = null;
  if (pod.spec?.nodeName) {
    try { node = namesAndStatus(await readOptional(api, 'readNode', { name: pod.spec.nodeName })); }
    catch (error) { nodeError = errorText(error); }
  }
  let services = [];
  let servicesError = null;
  try {
    const list = await readOptional(api, 'listNamespacedService', { namespace: asset.k8s_namespace, limit: 100 });
    const labels = pod.metadata?.labels ?? {};
    services = (list.items ?? []).filter(service => {
      const selector = service.spec?.selector;
      return selector && Object.keys(selector).length > 0 && Object.entries(selector).every(([key, value]) => labels[key] === value);
    }).map(service => ({ name: service.metadata?.name, ports: (service.spec?.ports ?? []).map(port => port.port) }));
    if (list.metadata?._continue) servicesError = 'Service list truncated at 100 items';
  } catch (error) { servicesError = errorText(error); }
  let resourceMetrics = null;
  let resourceMetricsError = null;
  try {
    const metrics = metricsApi ?? createK8sMetrics(asset);
    if (!metrics) throw new Error('Kubernetes metrics API is not configured');
    const podMetrics = await metrics.getPodMetrics(asset.k8s_namespace);
    let nodeMetrics = { items: [] };
    if (pod.spec?.nodeName) {
      try { nodeMetrics = await metrics.getNodeMetrics(); }
      catch (error) { nodeError ??= `Node metrics: ${errorText(error)}`; }
    }
    const currentPod = (podMetrics.items ?? []).find(item => item.metadata?.name === asset.k8s_pod);
    const currentNode = (nodeMetrics.items ?? []).find(item => item.metadata?.name === pod.spec?.nodeName);
    resourceMetrics = { source: 'metrics.k8s.io', observedAt: currentPod?.timestamp ?? currentNode?.timestamp ?? null,
      containers: (currentPod?.containers ?? []).map(container => ({ name: container.name, cpu: container.usage?.cpu ?? null, memory: container.usage?.memory ?? null })),
      node: currentNode ? { cpu: currentNode.usage?.cpu ?? null, memory: currentNode.usage?.memory ?? null } : null };
    if (!currentPod) resourceMetricsError = 'Mapped Pod has no metrics';
  } catch (error) { resourceMetricsError = errorText(error); }
  return {
    status: 'ok', cluster: asset.k8s_cluster, namespace: asset.k8s_namespace,
    pod: { name: pod.metadata?.name, uid: pod.metadata?.uid, phase: pod.status?.phase ?? null, node: pod.spec?.nodeName ?? null, owner: pod.metadata?.ownerReferences?.[0]?.name ?? null },
    containers, pvcs, events, eventsError, node, nodeError, services, servicesError,
    resourceMetrics, resourceMetricsError
  };
}

export async function discoverK8s(asset, api = createK8sApi(asset), apps = createK8sApps(asset)) {
  if (!api) return { status: 'not_configured', reason: 'Kubernetes API credentials are not configured' };
  if (!asset.k8s_namespace) return { status: 'unmapped', reason: 'Namespace is required' };
  const namespace = asset.k8s_namespace;
  const sources = [
    ['pods', api, 'listNamespacedPod'], ['services', api, 'listNamespacedService'],
    ['pvcs', api, 'listNamespacedPersistentVolumeClaim'],
    ['statefulSets', apps, 'listNamespacedStatefulSet'],
    ['deployments', apps, 'listNamespacedDeployment'],
    ['replicaSets', apps, 'listNamespacedReplicaSet']
  ];
  const results = await Promise.all(sources.map(async ([name, client, method]) => {
    try {
      const list = await readOptional(client ?? {}, method, { namespace, limit: 100 });
      return [name, { items: list.items ?? [], truncated: Boolean(list.metadata?._continue), error: null }];
    } catch (error) { return [name, { items: [], truncated: false, error: errorText(error) }]; }
  }));
  const data = Object.fromEntries(results);
  const replicaSets = new Map(data.replicaSets.items.map(item => [item.metadata?.uid, item]));
  const pods = data.pods.items.map(pod => {
    const owner = pod.metadata?.ownerReferences?.find(ref => ref.controller) ?? pod.metadata?.ownerReferences?.[0];
    const replicaSet = owner?.kind === 'ReplicaSet' ? replicaSets.get(owner.uid) : null;
    const deployment = replicaSet?.metadata?.ownerReferences?.find(ref => ref.kind === 'Deployment');
    const workload = deployment ? { kind: 'Deployment', name: deployment.name }
      : owner ? { kind: owner.kind, name: owner.name } : null;
    const labels = pod.metadata?.labels ?? {};
    const services = data.services.items.filter(service => {
      const selector = service.spec?.selector;
      return selector && Object.keys(selector).length > 0 && Object.entries(selector).every(([key, value]) => labels[key] === value);
    }).map(service => service.metadata?.name);
    const containers = (pod.spec?.containers ?? []).map(container => ({
      name: container.name, image: container.image ?? null,
      ports: (container.ports ?? []).map(port => port.containerPort)
    }));
    const postgresLikely = containers.some(container =>
      container.ports.includes(5432) || /(?:postgres|spilo|patroni|timescale|crunchy|cloudnative-pg)/i.test(container.image ?? ''));
    return { name: pod.metadata?.name, uid: pod.metadata?.uid, phase: pod.status?.phase ?? null,
      node: pod.spec?.nodeName ?? null, workload, services,
      pvcs: (pod.spec?.volumes ?? []).map(volume => volume.persistentVolumeClaim?.claimName).filter(Boolean),
      containers, postgresLikely };
  });
  return { status: data.pods.error ? 'error' : 'ok', cluster: asset.k8s_cluster, namespace,
    pods, pvcs: data.pvcs.items.map(pvc => ({ name: pvc.metadata?.name, phase: pvc.status?.phase ?? null,
      volume: pvc.spec?.volumeName ?? null })),
    workloads: {
      statefulSets: data.statefulSets.items.map(item => item.metadata?.name),
      deployments: data.deployments.items.map(item => item.metadata?.name)
    },
    errors: Object.fromEntries(results.filter(([, value]) => value.error).map(([name, value]) => [name, value.error])),
    truncated: Object.fromEntries(results.filter(([, value]) => value.truncated).map(([name]) => [name, true])) };
}
