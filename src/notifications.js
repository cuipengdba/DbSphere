import { createHmac } from 'node:crypto';
import nodemailer from 'nodemailer';

const severityRank = { info: 0, warning: 1, critical: 2 };

export function notificationConfig(env = process.env) {
  const minimumSeverity = env.DBSPHERE_NOTIFY_MIN_SEVERITY || 'warning';
  if (!(minimumSeverity in severityRank)) throw new Error('Invalid DBSPHERE_NOTIFY_MIN_SEVERITY');
  const webhookMinimumSeverity = env.DBSPHERE_WEBHOOK_MIN_SEVERITY || minimumSeverity;
  const emailMinimumSeverity = env.DBSPHERE_EMAIL_MIN_SEVERITY || minimumSeverity;
  if (!(webhookMinimumSeverity in severityRank) || !(emailMinimumSeverity in severityRank)) {
    throw new Error('Invalid notification channel severity');
  }
  let webhookUrl = null;
  if (env.DBSPHERE_WEBHOOK_URL) {
    let url;
    try { url = new URL(env.DBSPHERE_WEBHOOK_URL); }
    catch { throw new Error('Invalid DBSPHERE_WEBHOOK_URL'); }
    if (url.protocol !== 'https:' && !(url.protocol === 'http:' &&
      (['localhost', '127.0.0.1', '[::1]'].includes(url.hostname) || env.DBSPHERE_WEBHOOK_ALLOW_HTTP === 'true'))) {
      throw new Error('Webhook must use HTTPS unless DBSPHERE_WEBHOOK_ALLOW_HTTP=true');
    }
    if (url.username || url.password) throw new Error('Webhook URL must not contain credentials');
    webhookUrl = url.toString();
  }
  const smtpHost = env.DBSPHERE_SMTP_HOST || null;
  const smtpTo = env.DBSPHERE_SMTP_TO || null;
  const smtpFrom = env.DBSPHERE_SMTP_FROM || null;
  if ([smtpHost, smtpTo, smtpFrom].some(Boolean) && ![smtpHost, smtpTo, smtpFrom].every(Boolean)) {
    throw new Error('Set DBSPHERE_SMTP_HOST, DBSPHERE_SMTP_FROM and DBSPHERE_SMTP_TO together');
  }
  const smtpPort = Number(env.DBSPHERE_SMTP_PORT || 587);
  if (!Number.isInteger(smtpPort) || smtpPort < 1 || smtpPort > 65535) throw new Error('Invalid DBSPHERE_SMTP_PORT');
  if (Boolean(env.DBSPHERE_SMTP_USER) !== Boolean(env.DBSPHERE_SMTP_PASSWORD)) {
    throw new Error('Set DBSPHERE_SMTP_USER and DBSPHERE_SMTP_PASSWORD together');
  }
  return { minimumSeverity, webhookMinimumSeverity, emailMinimumSeverity,
    webhookUrl, webhookSecret: env.DBSPHERE_WEBHOOK_SECRET || null,
    smtp: smtpHost ? { host: smtpHost, port: smtpPort, secure: smtpPort === 465,
      requireTLS: smtpPort !== 465, from: smtpFrom, to: smtpTo,
      auth: env.DBSPHERE_SMTP_USER ? { user: env.DBSPHERE_SMTP_USER,
        pass: env.DBSPHERE_SMTP_PASSWORD } : undefined } : null };
}

export function createNotifier(store, config = notificationConfig(),
  { fetchFn = fetch, mailer = nodemailer } = {}) {
  let busy = false;
  let transporter = null;
  async function send(item) {
    const minimum = item.channel === 'webhook' ? config.webhookMinimumSeverity : config.emailMinimumSeverity;
    if (severityRank[item.payload.severity] < severityRank[minimum]) return 'skipped';
    const message = { schemaVersion: 1, deliveryId: item.id, eventId: item.eventId,
      eventTitle: item.eventTitle, eventStatus: item.eventStatus, assetId: item.assetId,
      assetName: item.assetName, kind: item.kind, observedAt: item.observedAt,
      ruleId: item.payload.ruleId, severity: item.payload.severity,
      value: item.payload.value, source: item.payload.source };
    if (item.channel === 'webhook') {
      if (!config.webhookUrl) return 'skipped';
      const body = JSON.stringify(message);
      const headers = { 'content-type': 'application/json', 'x-dbsphere-delivery-id': item.id };
      if (config.webhookSecret) headers['x-dbsphere-signature'] = `sha256=${createHmac('sha256', config.webhookSecret).update(body).digest('hex')}`;
      let response;
      try { response = await fetchFn(config.webhookUrl, { method: 'POST', headers, body,
        redirect: 'error', signal: AbortSignal.timeout(5000) }); }
      catch { throw new Error('Webhook request failed'); }
      if (!response.ok) throw new Error(`Webhook HTTP ${response.status}`);
      return 'delivered';
    }
    if (item.channel === 'email') {
      if (!config.smtp) return 'skipped';
      transporter ??= mailer.createTransport({ host: config.smtp.host, port: config.smtp.port,
        secure: config.smtp.secure, requireTLS: config.smtp.requireTLS,
        connectionTimeout: 5000, greetingTimeout: 5000, socketTimeout: 10000,
        auth: config.smtp.auth });
      const subject = `[DbSphere ${item.payload.severity}] ${item.assetName} · ${item.eventTitle}`;
      const text = [`事件 ID: ${item.eventId}`, `资产: ${item.assetName} (${item.assetId})`,
        `告警: ${item.payload.ruleId}`, `状态: ${item.kind}`, `级别: ${item.payload.severity}`,
        `时间: ${item.observedAt}`, `指标来源: ${item.payload.source || '未知'}`].join('\n');
      try { await transporter.sendMail({ from: config.smtp.from, to: config.smtp.to,
        subject, text, disableFileAccess: true, disableUrlAccess: true }); }
      catch { throw new Error('SMTP delivery failed'); }
      return 'delivered';
    }
    throw new Error('Unsupported notification channel');
  }
  async function run() {
    if (busy) return { busy: true };
    busy = true;
    let delivered = 0;
    let failed = 0;
    try {
      for (const item of await store.listDueNotifications()) {
        try {
          const outcome = await send(item);
          await store.completeNotification(item.id, outcome);
          if (outcome === 'delivered') delivered++;
        } catch (error) {
          await store.completeNotification(item.id, error.message);
          failed++;
        }
      }
      return { delivered, failed };
    } finally { busy = false; }
  }
  return { run };
}
