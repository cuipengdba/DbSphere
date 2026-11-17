const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const ruleId = /^[a-z][a-z0-9_]{1,63}$/;

export function normalizeSilence(input, now = new Date()) {
  if (!input || typeof input !== 'object' || Array.isArray(input)) return null;
  const allowed = new Set(['kind', 'assetId', 'ruleId', 'startsAt', 'endsAt', 'reason']);
  if (Object.keys(input).some(key => !allowed.has(key))) return null;
  const kind = input.kind ?? 'silence';
  if (!['silence', 'maintenance'].includes(kind)) return null;
  const assetId = input.assetId || null;
  const selectedRule = input.ruleId || null;
  if (assetId && (typeof assetId !== 'string' || !uuid.test(assetId))) return null;
  if (selectedRule && (typeof selectedRule !== 'string' || !ruleId.test(selectedRule))) return null;
  if (typeof input.reason !== 'string' || !input.reason.trim() || input.reason.length > 300) return null;
  const startsAt = input.startsAt == null ? now.toISOString() : input.startsAt;
  const endsAt = input.endsAt;
  if (typeof startsAt !== 'string' || typeof endsAt !== 'string' ||
    !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z$/.test(startsAt) ||
    !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z$/.test(endsAt)) return null;
  const start = Date.parse(startsAt);
  const end = Date.parse(endsAt);
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start ||
    end - start > 30 * 86400 * 1000 || start < now.getTime() - 5 * 60 * 1000 ||
    start > now.getTime() + 30 * 86400 * 1000) return null;
  return { kind, assetId, ruleId: selectedRule, startsAt: new Date(start).toISOString(),
    endsAt: new Date(end).toISOString(), reason: input.reason.trim() };
}
