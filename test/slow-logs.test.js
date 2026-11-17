import test from 'node:test';
import assert from 'node:assert/strict';
import { parseSlowLogs } from '../src/slow-logs.js';

test('PG JSON slow logs retain exact query ID while discarding statement and parameters', () => {
  const content = '{"timestamp":"2026-11-17T08:00:00Z","dbname":"app",' +
    '"message":"duration: 418.37 ms  statement: SELECT * FROM secrets WHERE token = abc",' +
    '"statement":"SELECT * FROM secrets WHERE token = abc","query_id":9223372036854775807}';
  const parsed = parseSlowLogs('jsonlog', content, 'app');
  assert.equal(parsed.entries.length, 1);
  assert.equal(parsed.entries[0].durationMs, 418.37);
  assert.equal(parsed.entries[0].queryId, '9223372036854775807');
  assert.equal(JSON.stringify(parsed).includes('secrets'), false);
  assert.equal(JSON.stringify(parsed).includes('token = abc'), false);
  const tricky = '{"timestamp":"2026-11-17T08:00:00Z","dbname":"app",' +
    '"message":"duration: 10 ms  statement: SELECT \\"query_id\\":999",' +
    '"query_id":123}';
  assert.equal(parseSlowLogs('jsonlog', tricky, 'app').entries[0].queryId, '123');
});

test('PG CSV slow log supports quoted multiline fields and skips other databases', () => {
  const fields = Array(26).fill('');
  fields[0] = '2026-11-17 08:00:00 UTC'; fields[2] = 'app';
  fields[13] = 'duration: 260.1 ms  statement: SELECT 1';
  fields[19] = 'SELECT private\nvalue'; fields[25] = '-123456789012345678';
  const csv = fields.map(value => `"${value.replaceAll('"', '""')}"`).join(',') + '\n';
  const parsed = parseSlowLogs('csvlog', csv, 'app');
  assert.equal(parsed.entries.length, 1);
  assert.equal(parsed.entries[0].queryId, '-123456789012345678');
  assert.equal(JSON.stringify(parsed).includes('private'), false);
  assert.equal(parseSlowLogs('csvlog', csv, 'other').entries.length, 0);
});
