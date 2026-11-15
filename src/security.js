import { createCipheriv, createDecipheriv, createHash, createHmac, randomBytes, scryptSync, timingSafeEqual } from 'node:crypto';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

export function loadMasterKey(dataDir) {
  if (process.env.DBSPHERE_MASTER_KEY) {
    const key = Buffer.from(process.env.DBSPHERE_MASTER_KEY, 'base64');
    if (key.length !== 32) throw new Error('DBSPHERE_MASTER_KEY must be 32 bytes in base64');
    return key;
  }
  const path = join(dataDir, 'master.key');
  if (!existsSync(path)) writeFileSync(path, randomBytes(32), { mode: 0o600, flag: 'wx' });
  const key = readFileSync(path);
  if (key.length !== 32) throw new Error('Invalid master key file');
  return key;
}

export function encrypt(key, value) {
  const iv = randomBytes(12);
  const cipher = createCipheriv('aes-256-gcm', key, iv);
  const ciphertext = Buffer.concat([cipher.update(value, 'utf8'), cipher.final()]);
  return Buffer.concat([iv, cipher.getAuthTag(), ciphertext]).toString('base64');
}

export function decrypt(key, encoded) {
  const data = Buffer.from(encoded, 'base64');
  const decipher = createDecipheriv('aes-256-gcm', key, data.subarray(0, 12));
  decipher.setAuthTag(data.subarray(12, 28));
  return Buffer.concat([decipher.update(data.subarray(28)), decipher.final()]).toString('utf8');
}

export function hashPassword(password) {
  const salt = randomBytes(16);
  return `${salt.toString('hex')}:${scryptSync(password, salt, 32).toString('hex')}`;
}

export function verifyPassword(password, stored) {
  const [saltHex, hashHex] = stored.split(':');
  const expected = Buffer.from(hashHex, 'hex');
  const actual = scryptSync(password, Buffer.from(saltHex, 'hex'), expected.length);
  return timingSafeEqual(actual, expected);
}

function signature(key, payload) {
  return createHmac('sha256', key).update(payload).digest('base64url');
}

export function createSession(key) {
  const payload = Buffer.from(JSON.stringify({ exp: Date.now() + 12 * 60 * 60 * 1000, nonce: randomBytes(16).toString('hex') })).toString('base64url');
  return `${payload}.${signature(key, payload)}`;
}

export function verifySession(key, token) {
  if (!token || typeof token !== 'string') return false;
  const [payload, mac, extra] = token.split('.');
  if (!payload || !mac || extra) return false;
  const expected = Buffer.from(signature(key, payload));
  const actual = Buffer.from(mac);
  if (expected.length !== actual.length || !timingSafeEqual(expected, actual)) return false;
  try { return JSON.parse(Buffer.from(payload, 'base64url').toString()).exp > Date.now(); }
  catch { return false; }
}

export function csrfToken(key, token) {
  return createHash('sha256').update(signature(key, `csrf:${token}`)).digest('hex');
}
