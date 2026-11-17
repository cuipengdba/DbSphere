import { readFile, stat } from 'node:fs/promises';

export async function readStoreBackupManifest(path = process.env.DBSPHERE_STORE_BACKUP_MANIFEST,
  now = Date.now()) {
  if (!path) return { status: 'not_configured', reason: '未配置 PG18 监控存储备份清单；备份年龄和恢复点未知' };
  try {
    if ((await stat(path)).size > 64 * 1024) throw new Error('manifest too large');
    const data = JSON.parse(await readFile(path, 'utf8'));
    if (data.status !== 'success' || typeof data.backupId !== 'string' ||
      !/^[a-zA-Z0-9_.:-]{1,100}$/.test(data.backupId)) throw new Error('invalid backup status or ID');
    const completed = Date.parse(data.completedAt);
    const restorePoint = Date.parse(data.restorePointAt);
    if (!Number.isFinite(completed) || !Number.isFinite(restorePoint) ||
      completed > now + 300000 || restorePoint > completed || restorePoint <= 0) {
      throw new Error('invalid backup timestamps');
    }
    return { status: 'ok', backupId: data.backupId,
      completedAt: new Date(completed).toISOString(), restorePointAt: new Date(restorePoint).toISOString(),
      ageHours: Number(Math.max(0, (now - completed) / 3600000).toFixed(1)) };
  } catch {
    return { status: 'unavailable', reason: 'PG18 监控存储备份清单缺失、无权限或内容无效' };
  }
}
