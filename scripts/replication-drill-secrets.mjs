#!/usr/bin/env node

// Disposable local credentials; keep both output files outside the repository.
import { randomBytes } from 'node:crypto';
import { writeFileSync } from 'node:fs';

const [envPath, credentialsPath] = process.argv.slice(2);
if (!envPath || !credentialsPath) {
  console.error('usage: node scripts/replication-drill-secrets.mjs ENV_PATH CREDENTIALS_PATH');
  process.exit(2);
}
const random = () => randomBytes(32).toString('base64url');
const credentials = {
  pgPassword: random(), replPassword: random(), monitorPassword: random(),
  storePassword: random(), adminPassword: random(), masterKey: randomBytes(32).toString('base64')
};
const values = {
  DBSPHERE_DRILL_PG_PASSWORD: credentials.pgPassword,
  DBSPHERE_DRILL_REPL_PASSWORD: credentials.replPassword,
  DBSPHERE_DRILL_MONITOR_PASSWORD: credentials.monitorPassword,
  DBSPHERE_DRILL_STORE_PASSWORD: credentials.storePassword,
  DBSPHERE_DRILL_ADMIN_PASSWORD: credentials.adminPassword,
  DBSPHERE_DRILL_MASTER_KEY: credentials.masterKey
};
writeFileSync(envPath, Object.entries(values).map(([key, value]) => `${key}=${value}`).join('\n') + '\n',
  { mode: 0o600 });
writeFileSync(credentialsPath, JSON.stringify(credentials), { mode: 0o600 });
console.log('Wrote disposable replication drill credentials to the requested paths.');
