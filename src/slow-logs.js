import { createHash, randomUUID } from 'node:crypto';

const digest = raw => createHash('sha256').update(raw).digest('hex');
const duration = message => {
  const match = /^duration: ([0-9]+(?:\.[0-9]+)?) ms(?:\s|$)/.exec(String(message || ''));
  const value = match ? Number(match[1]) : null;
  return value !== null && Number.isFinite(value) && value >= 0 && value <= 1e9 ? value : null;
};
const queryId = raw => /^-?\d{1,20}$/.test(String(raw || '')) && String(raw) !== '0' ? String(raw) : null;

function csvRecords(content) {
  const records = [];
  let fields = [], field = '', quoted = false, start = 0;
  for (let i = 0; i < content.length; i++) {
    const char = content[i];
    if (char === '"') {
      if (quoted && content[i + 1] === '"') { field += '"'; i++; }
      else quoted = !quoted;
    } else if (char === ',' && !quoted) { fields.push(field); field = ''; }
    else if ((char === '\n' || char === '\r') && !quoted) {
      if (char === '\r' && content[i + 1] === '\n') i++;
      fields.push(field);
      if (fields.some(value => value !== '')) records.push({ fields, raw: content.slice(start, i + 1) });
      fields = []; field = ''; start = i + 1;
    } else field += char;
  }
  if (quoted) throw Object.assign(new Error('Incomplete CSV log record'), { status: 400 });
  if (field || fields.length) {
    fields.push(field); records.push({ fields, raw: content.slice(start) });
  }
  return records;
}

export function parseSlowLogs(format, content, database) {
  if (!['jsonlog', 'csvlog'].includes(format) || typeof content !== 'string' ||
    Buffer.byteLength(content, 'utf8') > 60000 || !database) {
    throw Object.assign(new Error('Invalid slow log import'), { status: 400 });
  }
  const records = format === 'csvlog' ? csvRecords(content)
    : content.split(/\r?\n/).filter(Boolean).map(raw => {
      let record;
      try { record = JSON.parse(raw); }
      catch { throw Object.assign(new Error('Invalid JSON log record'), { status: 400 }); }
      return { record, raw };
    });
  if (records.length > 200) throw Object.assign(new Error('Too many log records'), { status: 400 });
  const entries = [];
  for (const item of records) {
    const log = format === 'csvlog' ? {
      timestamp: item.fields[0], dbname: item.fields[2], message: item.fields[13],
      query_id: item.fields[25]
    } : item.record;
    if (log.dbname !== database || typeof log.timestamp !== 'string') continue;
    const elapsedMs = duration(log.message);
    const observedAt = new Date(log.timestamp);
    if (elapsedMs === null || !Number.isFinite(observedAt.getTime())) continue;
    const exactQueryId = format === 'jsonlog'
      ? /(?<!\\)"query_id"\s*:\s*"?(-?\d{1,20})"?/.exec(item.raw)?.[1] : log.query_id;
    entries.push({ id: randomUUID(), sourceKey: digest(item.raw),
      observedAt: observedAt.toISOString(), durationMs: elapsedMs,
      queryId: queryId(exactQueryId), sourceFormat: format });
  }
  return { scanned: records.length, ignored: records.length - entries.length, entries };
}
