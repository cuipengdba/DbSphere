import test from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { collectLinux, deriveLinuxRates } from '../src/linux.js';
import { createApp } from '../src/server.js';

const exposition = `# Linux host resource sample
node_cpu_seconds_total{cpu="0",mode="idle"} 400
node_cpu_seconds_total{cpu="0",mode="user"} 100
node_cpu_seconds_total{cpu="1",mode="idle"} 390
node_cpu_seconds_total{cpu="1",mode="system"} 110
node_load1 1.25
node_memory_MemTotal_bytes 1000
node_memory_MemAvailable_bytes 250
node_filesystem_size_bytes{device="/dev/sda1",fstype="ext4",mountpoint="/"} 10000
node_filesystem_avail_bytes{device="/dev/sda1",fstype="ext4",mountpoint="/"} 3000
node_filesystem_size_bytes{device="tmpfs",fstype="tmpfs",mountpoint="/run"} 5000
node_disk_read_bytes_total{device="sda"} 9000
node_disk_written_bytes_total{device="sda"} 4000
node_network_receive_bytes_total{device="eth0"} 8000
node_network_transmit_bytes_total{device="eth0"} 6000
node_network_receive_bytes_total{device="lo"} 900000
`;

test('Linux node_exporter metrics are bounded and mapped to host resources', async t => {
  const server = createServer((_req, res) => {
    res.writeHead(200, { 'content-type': 'text/plain' });
    res.end(exposition);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const result = await collectLinux({ linux_host: '127.0.0.1',
    linux_exporter_port: server.address().port });
  assert.equal(result.status, 'ok');
  assert.equal(result.source, 'node_exporter');
  assert.equal(result.cpu.totalSeconds, 1000);
  assert.equal(result.cpu.idleSeconds, 790);
  assert.equal(result.memory.usedPercent, 75);
  assert.equal(result.filesystems.length, 1);
  assert.equal(result.filesystems[0].usedPercent, 70);
  assert.equal(result.disk.readBytesTotal, 9000);
  assert.equal(result.network.receiveBytesTotal, 8000);
  assert.equal((await collectLinux({ linux_host: 'host' })).status, 'not_configured');
  const previous = { ...result, observedAt: '2026-11-15T00:00:00Z',
    cpu: { totalSeconds: 900, idleSeconds: 730 },
    disk: { readBytesTotal: 5000, writtenBytesTotal: 2000 },
    network: { receiveBytesTotal: 4000, transmitBytesTotal: 3000 } };
  const next = deriveLinuxRates({ ...result, observedAt: '2026-11-15T00:01:00Z' }, previous);
  assert.equal(next.cpu.usedPercent, 40);
  assert.equal(next.disk.readBytesPerSecond, 67);
  assert.equal(next.network.receiveBytesPerSecond, 67);
});

test('Linux host evidence stays in the same PostgreSQL failure event', async t => {
  const base = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(base, { recursive: true });
  const dir = mkdtempSync(join(base, 'dbsphere-linux-event-'));
  const previous = process.env.DBSPHERE_ADMIN_PASSWORD;
  process.env.DBSPHERE_ADMIN_PASSWORD = 'linux-event-test-password';
  const app = await createApp({ dataDir: dir,
    collectFn: async () => { throw new Error('connection refused'); },
    linuxFn: async () => ({ status: 'ok', source: 'node_exporter',
      memory: { usedPercent: 92 }, cpu: { load1: 8 }, filesystems: [],
      disk: { readBytesTotal: 0, writtenBytesTotal: 0 },
      network: { receiveBytesTotal: 0, transmitBytesTotal: 0 } }) });
  t.after(() => {
    app.stop();
    if (previous === undefined) delete process.env.DBSPHERE_ADMIN_PASSWORD;
    else process.env.DBSPHERE_ADMIN_PASSWORD = previous;
    if (dir.startsWith(base + sep)) rmSync(dir, { recursive: true, force: true });
  });
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const baseUrl = `http://127.0.0.1:${app.server.address().port}`;
  const login = await fetch(`${baseUrl}/api/login`, { method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: 'linux-event-test-password' }) });
  const csrf = (await login.json()).csrf;
  const cookie = login.headers.get('set-cookie').split(';')[0];
  const headers = { cookie, 'x-csrf-token': csrf, 'content-type': 'application/json' };
  const created = await fetch(`${baseUrl}/api/assets`, { method: 'POST', headers,
    body: JSON.stringify({ name: 'Linux PG', host: '127.0.0.1', port: 5432,
      database: 'postgres', username: 'monitor', password: 'test-secret',
      sslMode: 'disable', runtime: 'linux', linuxHost: 'db-node-01', linuxExporterPort: 9100 }) });
  assert.equal(created.status, 201);
  const id = (await created.json()).asset.id;
  const check = await fetch(`${baseUrl}/api/assets/${id}/check`, { method: 'POST', headers });
  assert.equal((await check.json()).result.status, 'error');
  const events = await (await fetch(`${baseUrl}/api/events`, { headers: { cookie } })).json();
  assert.equal(events.events.length, 1);
  const event = await (await fetch(`${baseUrl}/api/events/${events.events[0].id}`, {
    headers: { cookie }
  })).json();
  assert.equal(event.event.evidence.filter(e => e.kind === 'collection_error').length, 1);
  assert.equal(event.event.evidence.filter(e => e.kind === 'linux_state').length, 1);
  assert.equal(event.event.evidence.find(e => e.kind === 'linux_state').payload.memory.usedPercent, 92);
});
