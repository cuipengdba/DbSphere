#!/usr/bin/env node

// Development acceptance only. Credentials are supplied separately as a Secret.
import { readFileSync, writeFileSync } from 'node:fs';
const namespace = 'dbsphere-matrix';
const cluster = process.argv[2] || 'agentsql-v350';
const outputPath = process.argv[3];
if (!/^[a-z0-9][a-z0-9-]{0,40}$/.test(cluster)) {
  console.error('usage: node scripts/kind-matrix-manifest.mjs [KIND_CLUSTER_NAME] [OUTPUT_PATH]');
  process.exit(2);
}
const controlPlane = `${cluster}-control-plane`;
const workers = [`${cluster}-worker`, `${cluster}-worker2`, `${cluster}-worker3`];
const credentials = 'dbsphere-matrix-credentials';
const collectorConfig = readFileSync(new URL('../examples/trace-lock-app/otel-collector.yaml', import.meta.url), 'utf8');
const secret = key => ({ name: key, valueFrom: { secretKeyRef: { name: credentials, key } } });
const podField = (name, fieldPath) => ({ name, valueFrom: { fieldRef: { fieldPath } } });
const metadata = name => ({ name, namespace });
const selector = name => ({ matchLabels: { app: name } });
const service = (name, port) => ({
  apiVersion: 'v1', kind: 'Service', metadata: metadata(name),
  spec: { selector: { app: name }, ports: [{ port, targetPort: port }] }
});
const claim = name => ({
  apiVersion: 'v1', kind: 'PersistentVolumeClaim', metadata: metadata(name),
  spec: { accessModes: ['ReadWriteOnce'], resources: { requests: { storage: '1Gi' } } }
});
const placement = node => ({
  nodeSelector: { 'kubernetes.io/hostname': node },
  ...(node === controlPlane ? { tolerations: [{ key: 'node-role.kubernetes.io/control-plane', operator: 'Exists', effect: 'NoSchedule' }] } : {})
});
const postgres = (name, major, node, store = false) => {
  const port = 5432;
  const container = {
    name: 'postgres', image: `postgres:${major}-bookworm`, imagePullPolicy: 'IfNotPresent',
    ports: [{ containerPort: port }],
    env: store ? [
      { name: 'POSTGRES_USER', value: 'dbsphere' },
      { name: 'POSTGRES_DB', value: 'dbsphere' },
      { name: 'POSTGRES_PASSWORD', valueFrom: { secretKeyRef: { name: credentials, key: 'storePassword' } } }
    ] : [
      { name: 'POSTGRES_PASSWORD', valueFrom: { secretKeyRef: { name: credentials, key: 'postgresPassword' } } },
      { name: 'DBSPHERE_MONITOR_PASSWORD', valueFrom: { secretKeyRef: { name: credentials, key: 'monitorPassword' } } }
    ],
    readinessProbe: { exec: { command: ['pg_isready', '-U', store ? 'dbsphere' : 'postgres', '-d', store ? 'dbsphere' : 'postgres'] },
      periodSeconds: 5, timeoutSeconds: 2 },
    resources: { requests: { cpu: '25m', memory: '64Mi' }, limits: { memory: '512Mi' } },
    volumeMounts: store || major === 18 ? [{ name: 'data', mountPath: '/var/lib/postgresql' }] : []
  };
  if (!store) container.volumeMounts.push({ name: 'monitor-init', mountPath: '/docker-entrypoint-initdb.d', readOnly: true });
  const volumes = [
    ...(store || major === 18 ? [{ name: 'data', persistentVolumeClaim: { claimName: `${name}-data` } }] : []),
    ...(!store ? [{ name: 'monitor-init', configMap: { name: 'dbsphere-monitor-init', defaultMode: 0o755 } }] : [])
  ];
  return [{
    apiVersion: 'apps/v1', kind: 'Deployment', metadata: metadata(name),
    spec: { replicas: 1, selector: selector(name), template: { metadata: { labels: { app: name } },
      spec: { ...placement(node), containers: [container], volumes } } }
  }, service(name, port)];
};

const items = [
  { apiVersion: 'v1', kind: 'Namespace', metadata: { name: namespace } },
  {
    apiVersion: 'v1', kind: 'ConfigMap', metadata: metadata('dbsphere-monitor-init'),
    data: { '01-monitor.sh': `#!/bin/sh
set -eu
psql -v ON_ERROR_STOP=1 -v monitor_password="$DBSPHERE_MONITOR_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE ROLE dbsphere_monitor LOGIN PASSWORD :'monitor_password';
GRANT pg_monitor TO dbsphere_monitor;
SQL
` }
  },
  { apiVersion: 'v1', kind: 'ConfigMap', metadata: metadata('dbsphere-otel-collector'),
    data: { 'config.yaml': collectorConfig } },
  { apiVersion: 'v1', kind: 'ServiceAccount', metadata: metadata('dbsphere-reader') },
  {
    apiVersion: 'rbac.authorization.k8s.io/v1', kind: 'Role', metadata: metadata('dbsphere-reader'),
    rules: [
      { apiGroups: [''], resources: ['pods', 'persistentvolumeclaims'], verbs: ['get', 'list'] },
      { apiGroups: [''], resources: ['events', 'services'], verbs: ['list'] },
      { apiGroups: ['discovery.k8s.io'], resources: ['endpointslices'], verbs: ['list'] },
      { apiGroups: ['apps'], resources: ['deployments', 'replicasets', 'statefulsets'], verbs: ['list'] },
      { apiGroups: ['metrics.k8s.io'], resources: ['pods'], verbs: ['get', 'list'] }
    ]
  },
  {
    apiVersion: 'rbac.authorization.k8s.io/v1', kind: 'RoleBinding', metadata: metadata('dbsphere-reader'),
    roleRef: { apiGroup: 'rbac.authorization.k8s.io', kind: 'Role', name: 'dbsphere-reader' },
    subjects: [{ kind: 'ServiceAccount', name: 'dbsphere-reader', namespace }]
  },
  {
    apiVersion: 'rbac.authorization.k8s.io/v1', kind: 'ClusterRole', metadata: { name: 'dbsphere-matrix-node-reader' },
    rules: [
      { apiGroups: [''], resources: ['nodes', 'persistentvolumes'], verbs: ['get'] },
      { apiGroups: ['metrics.k8s.io'], resources: ['nodes'], verbs: ['get'] }
    ]
  },
  {
    apiVersion: 'rbac.authorization.k8s.io/v1', kind: 'ClusterRoleBinding', metadata: { name: 'dbsphere-matrix-node-reader' },
    roleRef: { apiGroup: 'rbac.authorization.k8s.io', kind: 'ClusterRole', name: 'dbsphere-matrix-node-reader' },
    subjects: [{ kind: 'ServiceAccount', name: 'dbsphere-reader', namespace }]
  },
  claim('dbsphere-store-data'),
  claim('pg18-data'),
  ...postgres('dbsphere-store', 18, workers[1], true),
  ...postgres('pg14', 14, workers[0]),
  ...postgres('pg15', 15, workers[0]),
  ...postgres('pg16', 16, workers[2]),
  ...postgres('pg17', 17, workers[2]),
  ...postgres('pg18', 18, workers[2]),
  ...postgres('pg19', 19, workers[1]),
  {
    apiVersion: 'apps/v1', kind: 'Deployment', metadata: metadata('dbsphere-go'),
    spec: { replicas: 1, selector: selector('dbsphere-go'), template: { metadata: { labels: { app: 'dbsphere-go' } },
      spec: { ...placement(controlPlane), serviceAccountName: 'dbsphere-reader', containers: [{
        name: 'dbsphere', image: 'dbsphere/kind-matrix:local', imagePullPolicy: 'Never',
        ports: [{ containerPort: 8080 }],
        env: [secret('databaseURL'), secret('adminPassword'), secret('masterKey'), secret('traceToken')].map((value, index) => ({
          ...value, name: ['DBSPHERE_DATABASE_URL', 'DBSPHERE_ADMIN_PASSWORD', 'DBSPHERE_MASTER_KEY', 'DBSPHERE_OTLP_TOKEN'][index]
        })).concat([
          { name: 'DBSPHERE_HOST', value: '0.0.0.0' }, { name: 'DBSPHERE_PORT', value: '8080' },
          { name: 'DBSPHERE_ENABLE_GO_COLLECTOR', value: 'false' }
        ]),
        readinessProbe: { httpGet: { path: '/readyz', port: 8080 }, periodSeconds: 5 },
        resources: { requests: { cpu: '50m', memory: '64Mi' }, limits: { memory: '512Mi' } }
      }] } } }
  },
  service('dbsphere-go', 8080),
  {
    apiVersion: 'apps/v1', kind: 'Deployment', metadata: metadata('dbsphere-oom-app'),
    spec: { replicas: 1, selector: selector('dbsphere-oom-app'), template: {
      metadata: { labels: { app: 'dbsphere-oom-app' } }, spec: {
        ...placement(workers[0]), containers: [{
          name: 'app', image: 'dbsphere/trace-lock-app:kind-oom', imagePullPolicy: 'Never',
          ports: [{ containerPort: 18092 }],
          env: [
            { ...secret('demoPGURL'), name: 'DBSPHERE_DEMO_PG_URL' },
            { ...secret('traceToken'), name: 'DBSPHERE_DEMO_OTLP_TOKEN' },
            { name: 'DBSPHERE_DEMO_OTLP_ENDPOINT', value: 'http://dbsphere-go:8080/v1/traces' },
            { name: 'DBSPHERE_DEMO_LISTEN', value: '0.0.0.0:18092' }
          ],
          readinessProbe: { httpGet: { path: '/healthz', port: 18092 }, periodSeconds: 5 },
          resources: { requests: { cpu: '25m', memory: '16Mi' }, limits: { memory: '128Mi' } }
        }]
      }
    } }
  },
  service('dbsphere-oom-app', 18092),
  {
    apiVersion: 'apps/v1', kind: 'Deployment', metadata: metadata('dbsphere-otel-collector'),
    spec: { replicas: 1, selector: selector('dbsphere-otel-collector'), template: {
      metadata: { labels: { app: 'dbsphere-otel-collector' } }, spec: {
        ...placement(workers[0]), containers: [{
          name: 'collector', image: 'otel/opentelemetry-collector:0.165.0', imagePullPolicy: 'IfNotPresent',
          args: ['--config=/etc/otelcol/config.yaml'],
          ports: [{ containerPort: 4318 }],
          env: [
            { name: 'DBSPHERE_OTLP_ENDPOINT', value: 'http://dbsphere-go:8080/v1/traces' },
            { ...secret('traceToken'), name: 'DBSPHERE_OTLP_TOKEN' }
          ],
          volumeMounts: [{ name: 'config', mountPath: '/etc/otelcol/config.yaml', subPath: 'config.yaml', readOnly: true }],
          readinessProbe: { tcpSocket: { port: 4318 }, periodSeconds: 5 },
          resources: { requests: { cpu: '25m', memory: '32Mi' }, limits: { memory: '128Mi' } }
        }],
        volumes: [{ name: 'config', configMap: { name: 'dbsphere-otel-collector' } }]
      }
    } }
  },
  service('dbsphere-otel-collector', 4318),
  {
    apiVersion: 'apps/v1', kind: 'Deployment', metadata: metadata('dbsphere-sdk-oom-app'),
    spec: { replicas: 1, selector: selector('dbsphere-sdk-oom-app'), template: {
      metadata: { labels: { app: 'dbsphere-sdk-oom-app' } }, spec: {
        ...placement(workers[0]), containers: [{
          name: 'app', image: 'dbsphere/otel-sdk-probe:kind-oom', imagePullPolicy: 'Never',
          ports: [{ containerPort: 18093 }],
          env: [
            { ...secret('demoPGURL'), name: 'DBSPHERE_SDK_PROBE_PG_URL' },
            { name: 'DBSPHERE_SDK_PROBE_OTLP_ENDPOINT', value: 'http://dbsphere-otel-collector:4318/v1/traces' },
            { name: 'DBSPHERE_SDK_PROBE_ASSET_HOST', value: 'pg18' },
            { name: 'DBSPHERE_SDK_PROBE_ASSET_PORT', value: '5432' },
            { name: 'DBSPHERE_SDK_PROBE_ASSET_DATABASE', value: 'postgres' },
            { name: 'DBSPHERE_SDK_PROBE_SERVICE', value: 'dbsphere-sdk-oom-app' },
            { name: 'DBSPHERE_SDK_PROBE_SAMPLING_PROBABILITY', value: '1.0' },
            { name: 'DBSPHERE_SDK_PROBE_LISTEN', value: '0.0.0.0:18093' },
            podField('DBSPHERE_SDK_PROBE_K8S_NAMESPACE', 'metadata.namespace'),
            podField('DBSPHERE_SDK_PROBE_K8S_POD', 'metadata.name'),
            podField('DBSPHERE_SDK_PROBE_K8S_POD_UID', 'metadata.uid')
          ],
          readinessProbe: { httpGet: { path: '/healthz', port: 18093 }, periodSeconds: 5 },
          resources: { requests: { cpu: '25m', memory: '32Mi' }, limits: { memory: '128Mi' } }
        }]
      }
    } }
  },
  service('dbsphere-sdk-oom-app', 18093)
];

const manifest = JSON.stringify({ apiVersion: 'v1', kind: 'List', items });
if (outputPath) {
  writeFileSync(outputPath, manifest, 'utf8');
  console.log(`Wrote kind matrix manifest to ${outputPath}`);
} else {
  console.log(manifest);
}
