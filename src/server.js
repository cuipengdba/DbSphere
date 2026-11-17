import { createServer } from 'node:http';
import { randomUUID } from 'node:crypto';
import { readFile, mkdir } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Store } from './store.js';
import { PgStore } from './pg-store.js';
import { collectPostgres } from './postgres.js';
import { collectK8s, discoverK8s } from './k8s.js';
import { collectLinux, deriveLinuxRates } from './linux.js';
import { collectPgBouncer } from './pgbouncer.js';
import { createSession, csrfToken, decrypt, encrypt, hashPassword, loadMasterKey, verifyPassword, verifySession } from './security.js';
import { normalizeSilence } from './silences.js';
import { createNotifier } from './notifications.js';
import { evaluateInspection } from './inspections.js';
import { collectBackup } from './backup.js';
import { forecastDatabaseGrowth } from './capacity.js';
import { inspectionReportHtml, inspectionReportMarkdown } from './reports.js';
import { readStoreBackupManifest } from './store-backup.js';

const publicDir = resolve(dirname(fileURLToPath(import.meta.url)), '../public');
const staticFiles = new Map([['/', ['index.html', 'text/html; charset=utf-8']], ['/app.js', ['app.js', 'text/javascript; charset=utf-8']], ['/styles.css', ['styles.css', 'text/css; charset=utf-8']], ['/favicon.svg', ['favicon.svg', 'image/svg+xml']]]);

function send(res, status, data, extra = {}) {
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store', ...extra });
  res.end(JSON.stringify(data));
}

function sendReport(res, content, format, id) {
  res.writeHead(200, { 'Content-Type': format === 'html' ? 'text/html; charset=utf-8' : 'text/markdown; charset=utf-8',
    'Content-Disposition': `attachment; filename="dbsphere-inspection-${id}.${format === 'html' ? 'html' : 'md'}"`,
    'Cache-Control': 'no-store' });
  res.end(content);
}

async function body(req) {
  if (!req.headers['content-type']?.startsWith('application/json')) throw Object.assign(new Error('Expected JSON body'), { status: 415 });
  let raw = '';
  for await (const chunk of req) {
    raw += chunk;
    if (raw.length > 65536) throw Object.assign(new Error('Request too large'), { status: 413 });
  }
  try { return JSON.parse(raw); }
  catch { throw Object.assign(new Error('Invalid JSON'), { status: 400 }); }
}

function validAsset(a) {
  if (!a || typeof a !== 'object') return false;
  if (typeof a.name !== 'string' || a.name.trim().length < 1 || a.name.length > 100) return false;
  if (typeof a.host !== 'string' || !/^[a-zA-Z0-9_.:-]{1,255}$/.test(a.host)) return false;
  if (!Number.isInteger(Number(a.port)) || Number(a.port) < 1 || Number(a.port) > 65535) return false;
  if (typeof a.database !== 'string' || !a.database || a.database.length > 100) return false;
  if (typeof a.username !== 'string' || !a.username || a.username.length > 100) return false;
  if (typeof a.password !== 'string' || a.password.length > 1024) return false;
  if (!['disable', 'require'].includes(a.sslMode)) return false;
  if (!['linux', 'k8s'].includes(a.runtime)) return false;
  if (a.runtime === 'k8s' && (!a.k8sCluster || !a.namespace)) return false;
  if (a.linuxExporterPort !== '' && a.linuxExporterPort != null) {
    if (a.runtime !== 'linux' || !/^[a-zA-Z0-9_.:-]{1,150}$/.test(a.linuxHost || '')) return false;
    if (!Number.isInteger(Number(a.linuxExporterPort)) || Number(a.linuxExporterPort) < 1 || Number(a.linuxExporterPort) > 65535) return false;
  }
  if (a.poolerUrl) {
    if (typeof a.poolerUrl !== 'string' || a.poolerUrl.length > 2048) return false;
    try {
      const url = new URL(a.poolerUrl);
      if (!['postgres:', 'postgresql:'].includes(url.protocol) || url.pathname !== '/pgbouncer' || !url.username) return false;
    } catch { return false; }
  }
  if (a.poolerClear != null && typeof a.poolerClear !== 'boolean') return false;
  return ['linuxHost', 'k8sCluster', 'namespace', 'workload', 'pod', 'node'].every(k => !a[k] || (typeof a[k] === 'string' && a[k].length <= 150));
}

function cookie(req) {
  const pair = (req.headers.cookie || '').split(';').map(x => x.trim()).find(x => x.startsWith('dbsphere_session='));
  return pair ? pair.slice('dbsphere_session='.length) : null;
}

export async function createApp({ dataDir, collectFn = collectPostgres, k8sFn = collectK8s,
  discoverK8sFn = discoverK8s, linuxFn = collectLinux, poolerFn = collectPgBouncer,
  backupFn = collectBackup } = {}) {
  const directory = resolve(dataDir ?? process.env.DBSPHERE_DATA_DIR ?? './data');
  await mkdir(directory, { recursive: true, mode: 0o700 });
  const key = loadMasterKey(directory);
  const store = process.env.DBSPHERE_DATABASE_URL
    ? await PgStore.create(process.env.DBSPHERE_DATABASE_URL) : new Store(directory);
  if (!await store.getSetting('admin_password_hash')) {
    const password = process.env.DBSPHERE_ADMIN_PASSWORD;
    if (!password || password.length < 12) { await store.close(); throw new Error('Set DBSPHERE_ADMIN_PASSWORD (at least 12 characters) for first start'); }
    await store.setSetting('admin_password_hash', hashPassword(password));
  }
  let notifier;
  try { notifier = createNotifier(store); }
  catch (error) { await store.close(); throw error; }
  const retentionDays = process.env.DBSPHERE_RETENTION_DAYS ? Number(process.env.DBSPHERE_RETENTION_DAYS) : null;
  if (retentionDays !== null && (!Number.isInteger(retentionDays) || retentionDays < 1 || retentionDays > 3650)) {
    await store.close(); throw new Error('DBSPHERE_RETENTION_DAYS must be between 1 and 3650');
  }
  const eventRetentionDays = process.env.DBSPHERE_EVENT_RETENTION_DAYS
    ? Number(process.env.DBSPHERE_EVENT_RETENTION_DAYS) : null;
  if (eventRetentionDays !== null && (!Number.isInteger(eventRetentionDays) ||
    eventRetentionDays < 1 || eventRetentionDays > 3650)) {
    await store.close(); throw new Error('DBSPHERE_EVENT_RETENTION_DAYS must be between 1 and 3650');
  }
  const attempts = new Map();
  const activeCollections = new Set();
  const activeInspections = new Set();

  async function collectAsset(id) {
    if (activeCollections.has(id)) return { busy: true };
    const asset = await store.getInternalAsset(id);
    if (!asset) return null;
    activeCollections.add(id);
    try {
      let secret = '';
      let metrics = null;
      let error = null;
      try {
        secret = decrypt(key, asset.password_enc);
        metrics = await collectFn(asset, secret);
      } catch (e) { error = String(e?.message ?? e).replaceAll(secret || '\u0000', '[redacted]').slice(0, 500); }
      let k8s = null;
      if (asset.runtime === 'k8s') {
        try { k8s = await k8sFn(asset); }
        catch (e) { k8s = { status: 'error', reason: String(e?.message ?? e).slice(0, 300) }; }
      }
      let linux = null;
      if (asset.runtime === 'linux') {
        try { linux = await linuxFn(asset); }
        catch (e) { linux = { status: 'error', reason: String(e?.message ?? e).slice(0, 300) }; }
        if (linux?.status === 'ok') {
          const previous = (await store.listSamples(id, 1))[0]?.metrics?.linux;
          linux = deriveLinuxRates(linux, previous);
        }
      }
      let pgbouncer = null;
      if (asset.pooler_url_enc) {
        let poolerUrl = '';
        try { poolerUrl = decrypt(key, asset.pooler_url_enc); pgbouncer = await poolerFn(asset, poolerUrl); }
        catch (e) {
          let reason = String(e?.message ?? e);
          try {
            reason = reason.replaceAll(poolerUrl, '[redacted]');
            const secret = decodeURIComponent(new URL(poolerUrl).password);
            if (secret) reason = reason.replaceAll(secret, '[redacted]');
          }
          catch { reason = 'PgBouncer credential or connection error'; }
          pgbouncer = { status: 'error', reason: reason.slice(0, 300) };
        }
      }
      let backup;
      try { backup = await backupFn(asset); }
      catch { backup = { status: 'error', source: 'pgBackRest info', reason: '备份状态采集失败' }; }
      if (!await store.getInternalAsset(id)) return null;
      const recorded = await store.recordResult(id, { ...(metrics ?? {}), ...(k8s ? { k8s } : {}), ...(linux ? { linux } : {}),
        ...(pgbouncer ? { pgbouncer } : {}), backup }, error);
      void notifier.run().catch(e => console.error('Notification dispatch failed:', e.message));
      return recorded;
    } finally { activeCollections.delete(id); }
  }

  async function runInspection(assetId, templateId, retryOf = null) {
    const key = `${assetId}:${templateId}`;
    if (activeInspections.has(key)) throw Object.assign(new Error('Inspection already running'), { status: 409 });
    const asset = await store.getAsset(assetId);
    const template = await store.getInspectionTemplate(templateId);
    if (!asset || !template) throw Object.assign(new Error('Inspection asset or template not found'), { status: 404 });
    if (template.scope !== 'all' && template.scope !== asset.runtime) {
      throw Object.assign(new Error('Inspection template does not match asset runtime'), { status: 400 });
    }
    activeInspections.add(key);
    const startedAt = new Date().toISOString();
    try {
      const collected = await collectAsset(assetId);
      if (!collected || collected.busy) throw Object.assign(new Error('Asset collection is busy'), { status: 409 });
      const [sample, previous] = await store.listSamples(assetId, 2);
      if (!sample || sample.id !== collected.sampleId) throw new Error('Inspection sample was not stored');
      const priorRun = (await store.listInspectionRuns(assetId, 50))
        .find(run => run.templateId === templateId && run.status !== 'failed') ?? null;
      const { findings, summary } = evaluateInspection(template, asset, sample, previous, priorRun);
      const eventIds = (await store.listEvents()).filter(event => event.assetId === assetId &&
        event.status === 'open').map(event => event.id);
      return store.saveInspectionRun({ id: randomUUID(), assetId, templateId,
        templateVersion: template.version, sampleId: sample.id, retryOf,
        startedAt, completedAt: new Date().toISOString(), status: 'completed', error: null,
        summary, findings, eventIds });
    } catch (error) {
      const reason = error.status === 409 ? '资产采集正忙，请重试' : '巡检执行失败，请检查服务日志后重试';
      if (error.status !== 409) console.error('Inspection failed:', error);
      return store.saveInspectionRun({ id: randomUUID(), assetId, templateId,
        templateVersion: template.version, sampleId: null, retryOf,
        startedAt, completedAt: new Date().toISOString(), status: 'failed', error: reason,
        summary: { risk: 0, pass: 0, unknown: template.checks.length,
          notApplicable: 0, changed: 0 },
        findings: [], eventIds: [] });
    } finally { activeInspections.delete(key); }
  }

  const server = createServer(async (req, res) => {
    res.setHeader('X-Content-Type-Options', 'nosniff');
    res.setHeader('X-Frame-Options', 'DENY');
    res.setHeader('Referrer-Policy', 'no-referrer');
    res.setHeader('Content-Security-Policy', "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'");
    try {
      const requestUrl = new URL(req.url, 'http://localhost');
      const path = requestUrl.pathname;
      if (req.method === 'GET' && path === '/healthz') return send(res, 200, { status: 'ok' });
      if (req.method === 'GET' && path === '/readyz') {
        try { await store.health(); return send(res, 200, { status: 'ready', storage: store.kind }); }
        catch { return send(res, 503, { status: 'unavailable', storage: store.kind }); }
      }
      if (req.method === 'GET' && staticFiles.has(path)) {
        const [file, type] = staticFiles.get(path);
        res.writeHead(200, { 'Content-Type': type, 'Cache-Control': 'no-store' });
        return res.end(await readFile(join(publicDir, file)));
      }
      if (!path.startsWith('/api/')) return send(res, 404, { error: 'Not found' });
      if (req.method !== 'GET' && req.headers.origin) {
        const origin = new URL(req.headers.origin);
        if (origin.host !== req.headers.host || !['http:', 'https:'].includes(origin.protocol)) return send(res, 403, { error: 'Origin mismatch' });
      }
      const token = cookie(req);
      const authenticated = verifySession(key, token);
      if (req.method === 'POST' && path === '/api/login') {
        const ip = req.socket.remoteAddress || 'unknown';
        const entry = attempts.get(ip) ?? { count: 0, until: 0 };
        if (entry.until > Date.now()) return send(res, 429, { error: 'Too many attempts' });
        const input = await body(req);
        if (typeof input.password !== 'string' || !verifyPassword(input.password, await store.getSetting('admin_password_hash'))) {
          entry.count += 1;
          if (entry.count >= 5) { entry.until = Date.now() + 5 * 60 * 1000; entry.count = 0; }
          attempts.set(ip, entry);
          return send(res, 401, { error: 'Invalid credentials' });
        }
        attempts.delete(ip);
        const session = createSession(key);
        const secure = process.env.DBSPHERE_COOKIE_SECURE === 'true' ? '; Secure' : '';
        return send(res, 200, { authenticated: true, csrf: csrfToken(key, session) }, { 'Set-Cookie': `dbsphere_session=${session}; HttpOnly; SameSite=Strict; Path=/; Max-Age=43200${secure}` });
      }
      if (req.method === 'GET' && path === '/api/session') return send(res, 200, { authenticated, csrf: authenticated ? csrfToken(key, token) : null });
      if (!authenticated) return send(res, 401, { error: 'Authentication required' });
      if (req.method !== 'GET' && req.headers['x-csrf-token'] !== csrfToken(key, token)) return send(res, 403, { error: 'CSRF token required' });
      if (req.method === 'POST' && path === '/api/logout') return send(res, 200, { ok: true }, { 'Set-Cookie': 'dbsphere_session=; HttpOnly; SameSite=Strict; Path=/; Max-Age=0' });
      if (req.method === 'GET' && path === '/api/k8s/discover') {
        const cluster = requestUrl.searchParams.get('cluster') ?? '';
        const namespace = requestUrl.searchParams.get('namespace') ?? '';
        if (!cluster || cluster.length > 150 || !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(namespace) || namespace.length > 63) {
          return send(res, 400, { error: 'A valid K8S context and namespace are required' });
        }
        return send(res, 200, { discovery: await discoverK8sFn({ k8s_cluster: cluster, k8s_namespace: namespace }) });
      }
      if (req.method === 'GET' && path === '/api/assets') return send(res, 200, { assets: await store.listAssets() });
      if (req.method === 'GET' && path === '/api/alert-rules') return send(res, 200, { rules: await store.getAlertRules() });
      if (req.method === 'GET' && path === '/api/alerts') return send(res, 200, { alerts: await store.listActiveAlerts() });
      if (req.method === 'GET' && path === '/api/inspection-templates') {
        return send(res, 200, { templates: await store.getInspectionTemplates() });
      }
      const inspectionTemplateMatch = path.match(/^\/api\/inspection-templates\/([a-z][a-z0-9_]{1,63})$/);
      if (req.method === 'PATCH' && inspectionTemplateMatch) {
        const template = await store.updateInspectionTemplate(inspectionTemplateMatch[1], await body(req));
        return template ? send(res, 200, { template }) : send(res, 400, { error: 'Invalid template or thresholds' });
      }
      if (req.method === 'GET' && path === '/api/inspection-schedules') {
        return send(res, 200, { schedules: await store.listInspectionSchedules() });
      }
      if (req.method === 'PUT' && path === '/api/inspection-schedules') {
        const input = await body(req);
        if (!/^[0-9a-f-]{36}$/.test(input?.assetId ?? '') || !/^[a-z][a-z0-9_]{1,63}$/.test(input?.templateId ?? '') ||
          !Number.isInteger(input?.intervalMinutes) || input.intervalMinutes < 15 || input.intervalMinutes > 10080 ||
          typeof input.enabled !== 'boolean') return send(res, 400, { error: 'Invalid inspection schedule' });
        const schedule = await store.upsertInspectionSchedule(input.assetId, input.templateId,
          input.intervalMinutes, input.enabled);
        return schedule ? send(res, 200, { schedule }) : send(res, 404, { error: 'Asset or template not found' });
      }
      if (req.method === 'GET' && path === '/api/inspection-runs') {
        const assetId = requestUrl.searchParams.get('assetId');
        if (assetId && !/^[0-9a-f-]{36}$/.test(assetId)) return send(res, 400, { error: 'Invalid asset ID' });
        return send(res, 200, { runs: await store.listInspectionRuns(assetId, 50) });
      }
      if (req.method === 'POST' && path === '/api/inspections/run') {
        const input = await body(req);
        if (!/^[0-9a-f-]{36}$/.test(input?.assetId ?? '') || !/^[a-z][a-z0-9_]{1,63}$/.test(input?.templateId ?? '')) {
          return send(res, 400, { error: 'Invalid inspection request' });
        }
        return send(res, 201, { run: await runInspection(input.assetId, input.templateId) });
      }
      const inspectionRetryMatch = path.match(/^\/api\/inspection-runs\/([0-9a-f-]{36})\/retry$/);
      if (req.method === 'POST' && inspectionRetryMatch) {
        const run = await store.getInspectionRun(inspectionRetryMatch[1]);
        return run ? send(res, 201, { run: await runInspection(run.assetId, run.templateId, run.id) })
          : send(res, 404, { error: 'Inspection run not found' });
      }
      const inspectionReportMatch = path.match(/^\/api\/inspection-runs\/([0-9a-f-]{36})\/report$/);
      if (req.method === 'GET' && inspectionReportMatch) {
        const format = requestUrl.searchParams.get('format') ?? 'md';
        if (!['md', 'html'].includes(format)) return send(res, 400, { error: 'Invalid report format' });
        const run = await store.getInspectionRun(inspectionReportMatch[1]);
        if (!run) return send(res, 404, { error: 'Inspection run not found' });
        const asset = await store.getAsset(run.assetId);
        if (!asset) return send(res, 404, { error: 'Inspection asset not found' });
        const forecast = store.kind === 'postgresql18'
          ? forecastDatabaseGrowth(await store.databaseCapacityPoints(asset.id)) : null;
        return sendReport(res, format === 'html' ? inspectionReportHtml(run, asset, forecast)
          : inspectionReportMarkdown(run, asset, forecast), format, run.id);
      }
        if (req.method === 'GET' && path === '/api/notifications') return send(res, 200, { notifications: await store.listNotificationHistory() });
        if (req.method === 'GET' && path === '/api/storage') {
          return send(res, 200, { storage: store.kind === 'postgresql18'
            ? { ...await store.storageStats(retentionDays), eventRetentionDays,
              backup: await readStoreBackupManifest() }
            : { mode: 'sqlite-preview' } });
        }
        const notificationRetry = path.match(/^\/api\/notifications\/([0-9a-f-]{36})\/retry$/);
        if (req.method === 'POST' && notificationRetry) {
          const retried = await store.retryNotification(notificationRetry[1]);
          if (!retried) return send(res, 404, { error: 'Failed notification not found' });
          void notifier.run().catch(e => console.error('Notification dispatch failed:', e.message));
          return send(res, 200, { ok: true });
        }
      if (req.method === 'GET' && path === '/api/silences') return send(res, 200, { silences: await store.listSilences() });
      if (req.method === 'POST' && path === '/api/silences') {
        const input = normalizeSilence(await body(req));
        if (!input) return send(res, 400, { error: 'Invalid silence window' });
        const silence = await store.createSilence(input);
        return silence ? send(res, 201, { silence }) : send(res, 400, { error: 'Silence asset or rule not found' });
      }
      const silenceMatch = path.match(/^\/api\/silences\/([0-9a-f-]{36})$/);
      if (req.method === 'DELETE' && silenceMatch) {
        const silence = await store.revokeSilence(silenceMatch[1]);
        return silence ? send(res, 200, { silence }) : send(res, 404, { error: 'Silence not found or already revoked' });
      }
      const ruleMatch = path.match(/^\/api\/alert-rules\/([a-z][a-z0-9_]{1,63})$/);
      if (req.method === 'PATCH' && ruleMatch) {
        const changed = await store.updateAlertRule(ruleMatch[1], await body(req));
        return changed === null ? send(res, 404, { error: 'Alert rule not found' })
          : changed === false ? send(res, 400, { error: 'Invalid alert rule update' })
            : send(res, 200, { rule: changed });
      }
      if (req.method === 'POST' && path === '/api/assets') {
        const input = await body(req);
        if (!validAsset(input)) return send(res, 400, { error: 'Invalid asset fields' });
        const asset = await store.addAsset({ ...input, port: Number(input.port),
          linuxExporterPort: input.linuxExporterPort ? Number(input.linuxExporterPort) : null },
        encrypt(key, input.password), input.poolerUrl ? encrypt(key, input.poolerUrl) : null);
        return send(res, 201, { asset });
      }
      const assetMatch = path.match(/^\/api\/assets\/([0-9a-f-]{36})(?:\/(samples|series|capacity|check))?$/);
      if (assetMatch) {
        const [, id, action] = assetMatch;
        if (!await store.getAsset(id)) return send(res, 404, { error: 'Asset not found' });
        if (req.method === 'GET' && !action) return send(res, 200, { asset: await store.getAsset(id) });
        if (req.method === 'PATCH' && !action) {
          const input = await body(req);
          if (!validAsset(input)) return send(res, 400, { error: 'Invalid asset fields' });
          const old = await store.getInternalAsset(id);
          const password = input.password ? encrypt(key, input.password) : old.password_enc;
          const poolerUrl = input.poolerClear ? null : input.poolerUrl ? encrypt(key, input.poolerUrl) : old.pooler_url_enc;
          return send(res, 200, { asset: await store.updateAsset(id, { ...input, port: Number(input.port),
            linuxExporterPort: input.linuxExporterPort ? Number(input.linuxExporterPort) : null }, password, poolerUrl) });
        }
        if (req.method === 'DELETE' && !action) { await store.deleteAsset(id); return send(res, 200, { ok: true }); }
        if (req.method === 'GET' && action === 'samples') return send(res, 200, { samples: await store.listSamples(id) });
        if (req.method === 'GET' && action === 'capacity') {
          if (store.kind !== 'postgresql18') return send(res, 501, { error: 'Capacity forecast requires PostgreSQL 18 store' });
          return send(res, 200, { forecast: forecastDatabaseGrowth(await store.databaseCapacityPoints(id)) });
        }
        if (req.method === 'GET' && action === 'series') {
          if (store.kind !== 'postgresql18') return send(res, 501, { error: 'Series requires PostgreSQL 18 store' });
          const fromValue = requestUrl.searchParams.get('from');
          const toValue = requestUrl.searchParams.get('to');
          const from = new Date(fromValue);
          const to = new Date(toValue);
          const bucketHours = Number(requestUrl.searchParams.get('bucketHours') || 1);
          if (!fromValue || !toValue || !Number.isFinite(from.getTime()) || !Number.isFinite(to.getTime()) ||
            to <= from || to - from > 90 * 86400000 ||
            !Number.isInteger(bucketHours) || bucketHours < 1 || bucketHours > 24) {
            return send(res, 400, { error: 'Invalid series range or bucket' });
          }
          return send(res, 200, { series: await store.aggregateSamples(id, from.toISOString(),
            to.toISOString(), bucketHours) });
        }
        if (req.method === 'POST' && action === 'check') return send(res, 200, { result: await collectAsset(id) });
      }
      if (req.method === 'GET' && path === '/api/events') return send(res, 200, { events: await store.listEvents() });
      const eventMatch = path.match(/^\/api\/events\/([0-9a-f-]{36})$/);
      if (req.method === 'GET' && eventMatch) {
        const event = await store.getEvent(eventMatch[1]);
        return event ? send(res, 200, { event }) : send(res, 404, { error: 'Event not found' });
      }
      const ackMatch = path.match(/^\/api\/events\/([0-9a-f-]{36})\/ack$/);
      if (req.method === 'POST' && ackMatch) {
        const event = await store.acknowledgeEvent(ackMatch[1]);
        return event ? send(res, 200, { event }) : send(res, 404, { error: 'Event not found' });
      }
      return send(res, 404, { error: 'Not found' });
    } catch (e) {
      if (e.status) return send(res, e.status, { error: e.message });
      console.error(e);
      return send(res, 500, { error: 'Internal server error' });
    }
  });

  let timer;
  let retentionTimer;
  let inspectionTimer;
  function startScheduler() {
    const interval = Math.max(15, Number(process.env.DBSPHERE_COLLECT_INTERVAL_SECONDS) || 60) * 1000;
    const escalateAfter = Number(process.env.DBSPHERE_ESCALATE_AFTER_MINUTES || 15);
    if (!Number.isInteger(escalateAfter) || escalateAfter < 1 || escalateAfter > 1440) {
      throw new Error('DBSPHERE_ESCALATE_AFTER_MINUTES must be between 1 and 1440');
    }
    timer = setInterval(() => { void (async () => {
      for (const asset of await store.listAssets()) {
        await store.checkStale(asset.id);
        void collectAsset(asset.id).catch(e => console.error('Collection failed:', e));
      }
      await store.queueEscalations(new Date().toISOString(), escalateAfter);
      void notifier.run().catch(e => console.error('Notification dispatch failed:', e.message));
    })().catch(e => console.error('Scheduler failed:', e)); }, interval);
    timer.unref();
    const inspectDue = () => void (async () => {
      for (const due of await store.claimDueInspections()) {
        try { await runInspection(due.assetId, due.templateId); }
        catch (error) { console.error('Scheduled inspection failed:', error.message); }
      }
    })().catch(error => console.error('Inspection scheduler failed:', error.message));
    inspectionTimer = setInterval(inspectDue, 60000);
    inspectionTimer.unref();
    inspectDue();
    void notifier.run().catch(e => console.error('Notification dispatch failed:', e.message));
    if (store.kind === 'postgresql18' && (retentionDays !== null || eventRetentionDays !== null)) {
      const prune = () => void (async () => {
        if (retentionDays !== null) await store.pruneSamples(retentionDays);
        if (eventRetentionDays !== null) await store.pruneResolvedEvents(eventRetentionDays);
      })().catch(e => console.error('Retention sweep failed:', e.message));
      prune();
      retentionTimer = setInterval(prune, 24 * 3600 * 1000);
      retentionTimer.unref();
    }
  }
  function stop() { clearInterval(timer); clearInterval(retentionTimer); clearInterval(inspectionTimer); server.close(); return store.close(); }
  return { server, store, notifier, collectAsset, runInspection, startScheduler, stop };
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const app = await createApp();
  const host = process.env.DBSPHERE_HOST || '127.0.0.1';
  const port = Number(process.env.DBSPHERE_PORT || 8080);
  app.server.listen(port, host, () => console.log(`DbSphere listening on http://${host}:${port}`));
  app.startScheduler();
  process.on('SIGTERM', () => { app.stop(); });
  process.on('SIGINT', () => { app.stop(); });
}
