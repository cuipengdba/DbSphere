#!/usr/bin/env node

// Generate disposable credentials for the local multi-node acceptance cluster.
// Paths are explicit so a caller can keep secrets outside the source tree.
import { randomBytes } from 'node:crypto';
import { writeFileSync } from 'node:fs';

const [manifestPath, credentialsPath] = process.argv.slice(2);
if (!manifestPath || !credentialsPath) {
  console.error('usage: node scripts/kind-matrix-secrets.mjs SECRET_MANIFEST_PATH CREDENTIALS_PATH');
  process.exit(2);
}
const random = () => randomBytes(32).toString('base64url');
const credentials = {
  postgresPassword: random(), monitorPassword: random(), storePassword: random(),
  adminPassword: random(), traceToken: random(), masterKey: randomBytes(32).toString('base64')
};
credentials.databaseURL = `postgres://dbsphere:${encodeURIComponent(credentials.storePassword)}@dbsphere-store:5432/dbsphere?sslmode=disable`;
credentials.demoPGURL = `postgres://dbsphere_monitor:${encodeURIComponent(credentials.monitorPassword)}@pg18:5432/postgres?sslmode=disable`;
const secret = {
  apiVersion: 'v1', kind: 'Secret', type: 'Opaque',
  metadata: { name: 'dbsphere-matrix-credentials', namespace: 'dbsphere-matrix' },
  data: Object.fromEntries(Object.entries(credentials).map(([key, value]) =>
    [key, Buffer.from(value).toString('base64')]))
};
writeFileSync(manifestPath, JSON.stringify(secret), { mode: 0o600 });
writeFileSync(credentialsPath, JSON.stringify(credentials), { mode: 0o600 });
console.log('Wrote local matrix Secret and credentials to the requested paths.');
