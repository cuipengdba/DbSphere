const traceIdPattern = /^[0-9a-f]{32}$/i;
const spanIdPattern = /^[0-9a-f]{16}$/i;

function attributes(values) {
  const result = {};
  for (const item of Array.isArray(values) ? values : []) {
    if (typeof item?.key !== 'string' || !item.value) continue;
    const value = item.value.stringValue ?? item.value.intValue;
    if (typeof value === 'string' || typeof value === 'number') result[item.key] = String(value);
  }
  return result;
}

function isoNanos(value) {
  try {
    const nanos = BigInt(value);
    if (nanos <= 0n) return null;
    const millis = Number(nanos / 1000000n);
    return Number.isFinite(millis) && millis >= 0 && millis <= 8640000000000000
      ? new Date(millis).toISOString() : null;
  } catch { return null; }
}

function clientKind(kind) { return kind === 3 || kind === '3' || kind === 'SPAN_KIND_CLIENT'; }

// Keep a deliberately small allowlist. Span names, SQL, parameters, URLs and events can contain secrets.
export function parseOtlpTraces(input, assets, now = Date.now()) {
  if (!input || !Array.isArray(input.resourceSpans) || input.resourceSpans.length > 32) {
    throw Object.assign(new Error('Invalid OTLP resourceSpans'), { status: 400 });
  }
  const parsed = [];
  let total = 0;
  for (const resource of input.resourceSpans) {
    if (!resource || typeof resource !== 'object') continue;
    const resourceAttrs = attributes(resource.resource?.attributes);
    const service = resourceAttrs['service.name'];
    const scopes = Array.isArray(resource.scopeSpans) ? resource.scopeSpans : [];
    if (scopes.length > 32) throw Object.assign(new Error('Too many scopes'), { status: 413 });
    for (const scope of scopes) {
      const spans = Array.isArray(scope?.spans) ? scope.spans : [];
      if (spans.length > 256) throw Object.assign(new Error('Too many spans'), { status: 413 });
      for (const span of spans) {
        total++;
        if (total > 256) throw Object.assign(new Error('Too many spans'), { status: 413 });
        if (typeof service !== 'string' || !service || service.length > 128) continue;
        if (!span || typeof span !== 'object') continue;
        if (typeof span.traceId !== 'string' || typeof span.spanId !== 'string' ||
          !traceIdPattern.test(span.traceId) || !spanIdPattern.test(span.spanId) ||
          (span.parentSpanId && (typeof span.parentSpanId !== 'string' ||
            !spanIdPattern.test(span.parentSpanId)))) continue;
        const start = isoNanos(span.startTimeUnixNano);
        const end = isoNanos(span.endTimeUnixNano);
        if (!start || !end || Date.parse(end) < Date.parse(start) ||
          Math.abs(Date.parse(start) - now) > 7 * 86400000) continue;
        const attrs = attributes(span.attributes);
        const db = clientKind(span.kind) && attrs['db.system.name'] === 'postgresql';
        parsed.push({ traceId: span.traceId.toLowerCase(), spanId: span.spanId.toLowerCase(),
          parentSpanId: span.parentSpanId?.toLowerCase() ?? null,
          service, kind: db ? 'postgresql_client' : 'service', start, end,
          address: db ? attrs['server.address'] ?? null : null,
          port: db ? Number(attrs['server.port'] ?? 5432) : null,
          namespace: db ? attrs['db.namespace']?.split('|')[0] ?? null : null,
          status: span.status?.code === 2 || span.status?.code === '2' ||
            span.status?.code === 'STATUS_CODE_ERROR' ? 'error' : 'ok' });
      }
    }
  }
  const byTrace = new Map();
  for (const span of parsed) {
    const group = byTrace.get(span.traceId) ?? [];
    group.push(span);
    byTrace.set(span.traceId, group);
  }
  const selected = [];
  const accepted = new Set();
  for (const group of byTrace.values()) {
    const matched = new Set();
    const dbAssets = new Map();
    for (const span of group) {
      if (span.kind !== 'postgresql_client' || !span.address ||
        !Number.isInteger(span.port) || span.port < 1 || span.port > 65535) continue;
      const matches = assets.filter(asset => asset.host.toLowerCase() === span.address.toLowerCase() &&
        Number(asset.port) === span.port && (!span.namespace || asset.database === span.namespace));
      if (matches.length === 1) {
        matched.add(matches[0].id);
        dbAssets.set(span.spanId, matches[0].id);
      }
    }
    for (const assetId of matched) {
      for (const span of group) {
        if (span.kind === 'postgresql_client' && dbAssets.get(span.spanId) !== assetId) continue;
        selected.push({ assetId, traceId: span.traceId, spanId: span.spanId,
        parentSpanId: span.parentSpanId, service: span.service, kind: span.kind,
        start: span.start, end: span.end, status: span.status });
        accepted.add(`${span.traceId}:${span.spanId}`);
      }
    }
  }
  return { spans: selected, received: total, rejected: total - accepted.size };
}
