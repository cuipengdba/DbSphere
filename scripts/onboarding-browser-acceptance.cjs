const assert = require('node:assert/strict');
const { mkdir } = require('node:fs/promises');
const { join } = require('node:path');
const { chromium } = require('playwright');

const origin = process.env.DBSPHERE_TEST_ORIGIN;
const output = process.env.DBSPHERE_BROWSER_OUTPUT;
const ports = ['PG', 'MYSQL', 'REDIS'].map(engine => Number(process.env[`DBSPHERE_TEST_${engine}_PORT`]));
const password = process.env.DBSPHERE_TEST_REDIS_PASSWORD;
assert.ok(origin && output && password && ports.every(port => Number.isInteger(port) && port > 0));

async function main() {
  await mkdir(output, { recursive: true });
  const browser = await chromium.launch({ executablePath: process.env.DBSPHERE_CHROMIUM || undefined });
  const page = await browser.newPage({ viewport: { width: 1440, height: 960 } });
  page.setDefaultTimeout(30000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const panel = page.locator('#asset-import-panel');
  const form = page.locator('#asset-import-form');
  const rows = page.locator('#asset-import-results tbody tr');
  const batchRoot = '/api/assets/import/batches';
  async function action(control, method, path) {
    const response = page.waitForResponse(response => response.request().method() === method &&
      new URL(response.url()).pathname === path);
    await control.click();
    const result = await response;
    assert.equal(result.status(), 200, `${method} ${path}: ${await result.text()}`);
    return result.json();
  }
  const row = name => rows.filter({ hasText: name });
  const show = async (locator, text) => locator.getByText(text, { exact: false }).first().waitFor({ state: 'visible' });
  try {
    await page.goto(origin);
    await page.locator('#login-form [name=username]').fill('admin');
    await page.locator('#login-form [name=password]').fill(process.env.DBSPHERE_ADMIN_PASSWORD);
    await action(page.locator('#login-form button[type=submit]'), 'POST', '/api/login');
    const version = page.locator('#workspace-version');
    await version.waitFor({ state: 'visible' });
    assert.equal(await version.textContent(), 'v3.11.0');
    await page.locator('[data-nav=assets]').click();
    await panel.locator(':scope > summary').click();
    const manifest = ['PG', 'MYSQL', 'REDIS'].map((engine, i) => ({
      name: `V311 ${engine}`, runtime: ['linux', 'mysql', 'redis'][i], host: '127.0.0.1', port: ports[i],
      database: ['postgres', 'mysql', '0'][i], username: 'v311_monitor', sslMode: 'disable',
      credentialRef: `env:DBSPHERE_IMPORT_${engine}`,
    }));
    await form.locator('[name=content]').fill(JSON.stringify({ assets: manifest }));
    const plan = await action(form.getByRole('button', { name: '预览变更' }), 'POST', '/api/assets/import/preview');
    assert.equal(plan.canApply, true);
    assert.equal(plan.rows.length, 3);
    const requested = page.waitForRequest(request => request.method() === 'POST' &&
      new URL(request.url()).pathname === '/api/assets/import/apply');
    const applied = await action(page.locator('#asset-import-apply'), 'POST', '/api/assets/import/apply');
    const { batchId } = applied;
    assert.ok(batchId);
    const request = await requested;
    const replay = await page.request.post(origin + '/api/assets/import/apply', {
      data: request.postDataJSON(), headers: { 'X-CSRF-Token': request.headers()['x-csrf-token'] },
    });
    assert.equal(replay.status(), 200);
    assert.deepEqual(await replay.json(), applied);
    await show(row('V311 REDIS'), '等待首次采样');
    const first = await action(page.locator('#asset-import-probe'), 'POST', `${batchRoot}/${batchId}/check`);
    const firstPG = first.items.find(item => item.runtime === 'linux');
    const firstRedis = first.items.find(item => item.runtime === 'redis');
    const firstMySQL = first.items.find(item => item.runtime === 'mysql');
    assert.equal(firstPG.firstReport.status, 'limited');
    assert.equal(firstRedis.firstReport.status, 'failed');
    assert.ok(firstRedis.firstReport.checks.some(check => check.action === 'check_credentials'));
    assert.ok(firstMySQL.firstReport.checks.some(check => check.key === 'connection' && check.status === 'ok'));
    await show(row('V311 REDIS'), '核对目标库账号');
    await page.screenshot({ path: join(output, 'onboarding-first.png'), fullPage: true });

    await page.reload();
    await show(row('V311 REDIS'), '检查失败');
    await row('V311 REDIS').getByRole('link', { name: '首采原始采样' }).click();
    await show(page.locator('#sample-details'), firstRedis.firstReport.sampleId);
    await page.locator('#sample-return').click();
    await show(row('V311 REDIS'), '检查失败');
    assert.equal(new URLSearchParams(new URL(page.url()).hash.split('?')[1]).get('batch'), batchId);

    await row('V311 REDIS').getByRole('button', { name: '编辑原资产' }).click();
    await page.locator('#asset-form [name=password]').fill(password);
    await action(page.locator('#asset-submit'), 'PATCH', `/api/assets/${firstRedis.assetId}`);
    const fixed = await action(page.locator('#asset-import-recheck'), 'POST', `${batchRoot}/${batchId}/check`);
    const redis = fixed.items.find(item => item.assetId === firstRedis.assetId);
    assert.equal(redis.latestReport.status, 'ready');
    assert.equal(redis.firstReport.sampleId, firstRedis.firstReport.sampleId);
    assert.notEqual(redis.latestReport.sampleId, redis.firstReport.sampleId);
    await row('V311 REDIS').getByRole('link', { name: '复检原始采样' }).click();
    await show(page.locator('#sample-details'), redis.latestReport.sampleId);
    const response = await page.request.get(`${origin}/api/assets/${redis.assetId}/samples/${redis.latestReport.sampleId}`);
    assert.equal(response.status(), 200);
    const { sample } = await response.json();
    assert.match(sample.metrics.redis.version, /^6\.2\./);
    assert.equal(sample.metrics.redis.role, 'primary');
    assert.equal(typeof sample.metrics.redis.memoryBytes, 'number');
    await page.locator('#sample-return').click();
    await show(row('V311 REDIS'), '本次体检通过');
    await page.screenshot({ path: join(output, 'onboarding-recovered.png'), fullPage: true });
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({ status: 'passed', batchId, redisVersion: sample.metrics.redis.version,
      redisAsset: redis.assetId, firstSample: redis.firstReport.sampleId, retestSample: redis.latestReport.sampleId,
      postgres: firstPG.firstReport.status, mysql: firstMySQL.firstReport.status }));
  } finally {
    await browser.close();
  }
}

main().catch(error => { console.error(error); process.exitCode = 1; });
