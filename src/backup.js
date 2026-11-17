import { readFile, realpath, stat } from 'node:fs/promises';
import { resolve, sep } from 'node:path';

const unavailable = reason => ({ status: 'unavailable', source: 'pgBackRest info', reason });

export function parsePgBackRestInfo(payload, now = Date.now()) {
  const info = Array.isArray(payload) ? payload : payload?.pgbackrest;
  if (!Array.isArray(info) || info.length !== 1 || !info[0] || typeof info[0] !== 'object') {
    return unavailable('每个资产需提供一个 pgBackRest stanza 的 JSON 信息');
  }
  const stanza = info[0];
  const rawCode = stanza.status?.code;
  const code = typeof rawCode === 'number' || (typeof rawCode === 'string' && /^\d+$/.test(rawCode))
    ? Number(rawCode) : NaN;
  if (!Number.isInteger(code) || code < 0 || typeof stanza.name !== 'string') {
    return unavailable('pgBackRest stanza 状态或名称缺失');
  }
  const backups = Array.isArray(stanza.backup) ? stanza.backup : [];
  const latest = backups.map(item => Number(item?.timestamp?.stop))
    .filter(value => Number.isFinite(value) && value > 0 && value * 1000 <= now + 300000)
    .sort((a, b) => b - a)[0] ?? null;
  const attempt = !Array.isArray(payload) ? payload.lastAttempt : null;
  const lastAttemptStatus = ['success', 'failed'].includes(attempt?.status) ? attempt.status : null;
  const lastAttemptAt = lastAttemptStatus && Number.isFinite(Date.parse(attempt.at))
    ? new Date(attempt.at).toISOString() : null;
  return { status: 'ok', source: 'pgBackRest info', stanza: stanza.name,
    repositoryHealthy: code === 0, repositoryCode: code,
    lastSuccessAt: latest ? new Date(latest * 1000).toISOString() : null,
    lastAttemptStatus: lastAttemptAt ? lastAttemptStatus : null, lastAttemptAt };
}

export async function collectBackup(asset, directory = process.env.DBSPHERE_BACKUP_INFO_DIR) {
  if (!directory) return { status: 'not_configured', source: 'pgBackRest info',
    reason: '未配置 DBSPHERE_BACKUP_INFO_DIR，备份状态未知' };
  if (!/^[0-9a-f-]{36}$/.test(asset?.id ?? '')) return unavailable('资产 ID 无效');
  try {
    const root = await realpath(resolve(directory));
    const file = await realpath(resolve(root, `${asset.id}.json`));
    if (!file.startsWith(root + sep)) return unavailable('备份信息文件不在配置目录内');
    if ((await stat(file)).size > 1024 * 1024) return unavailable('备份信息文件超过 1 MiB');
    return parsePgBackRestInfo(JSON.parse(await readFile(file, 'utf8')));
  } catch {
    return unavailable('备份信息文件缺失、无权限或 JSON 无效');
  }
}
