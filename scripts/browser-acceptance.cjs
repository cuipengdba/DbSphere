const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const { join } = require('node:path');
const { chromium } = require('playwright');

const origin = 'http://127.0.0.1:18080';
const primary = 'Browser lab PostgreSQL';
const fixedTime = '2024-01-01T00:00:00Z';
const proof = [];
const formField = (page, form, name) => page.locator(`#${form} [name="${name}"]`);
const visible = (scope, text) => scope.getByText(text, { exact: false }).first().waitFor({ state: 'visible' });
const route = page => new URL(page.url()).hash;
const parameters = page => new URLSearchParams(route(page).split('?')[1] ?? '');

async function login(page, name, password) {
  await formField(page, 'login-form', 'username').fill(name);
  await formField(page, 'login-form', 'password').fill(password);
  await page.locator('#login-form button[type=submit]').click();
}

async function navigate(page, view) {
  await page.locator(`[data-nav="${view}"]`).click();
  await page.waitForFunction(name => document.body.dataset.activeView === name, view);
}

async function selectSection(page, view, section) {
  const tab = page.locator(`[data-view="${view}"] [data-section="${section}"]`);
  await tab.click();
  assert.equal(await tab.getAttribute('aria-pressed'), 'true');
  await page.locator(`[data-section-panel="${section}"]:visible`).first().waitFor();
}

async function action(page, locator, method, path, status) {
  const pending = page.waitForResponse(response => response.request().method() === method &&
    new URL(response.url()).pathname === path);
  await locator.click();
  const response = await pending;
  assert.equal(response.status(), status, method + ' ' + path + ' ' + await response.text());
  return response;
}

function seedSamples(asset) {
  assert.match(asset, /^[a-f0-9-]{36}$/);
  const statements = [];
  for (let index = 0; index < 5; index++) {
    const rows = {
      databaseName: 'postgres', postgresStartedAt: fixedTime, postgresEndpoint: { address: '127.0.0.1', port: 15434 },
      role: 'primary', connections: index + 8, max_connections: 20, xact_commit: 100 + 120 * index, xact_rollback: 0,
      replicas: 1, replication_observed: true, replication_lag_bytes: index * 2097152,
      linux: {
        status: 'ok', endpoint: 'qa-node:9100', bootTimeSeconds: 30,
        cpu: { usedPercent: 40 + index }, memory: { usedPercent: 60 + index },
        interval: { diskReadBps: 1048576 * index, diskWriteBps: 2097152 * index,
          networkReceiveBps: 1048576 * index, networkTransmitBps: 1048576 * index },
        devices: { disk: { sda: { reads: 60 * index, writes: 100 * index, readSeconds: 0.12 * index, writeSeconds: 0.4 * index } } },
        deviceNumbers: { sda: { number: '8:0' } }
      },
      pgbouncer: { status: 'ok', pools: [{ database: 'postgres', clientWaiting: index }] },
      containerStorage: { primary: 'postgres', containers: { postgres: {
        id: 'qa-container', status: 'ok', memory: { active_file: 3145728 * index, inactive_file: 6291456 * index }
      } } },
      diagnostics: { instanceSessions: { status: 'ok', data: { waiting: index } },
        locks: { status: 'ok', data: { blockedSessions: index } },
        transactions: { status: 'ok', data: { longTransactions: index, idleInTransaction: index } } },
      sqlPerformance: { status: 'ok', resetAt: fixedTime, statements: [{
        fingerprint: 'qa-query', queryId: '163', query: 'select 1', calls: index * 10, totalExecMs: index * 25, rows: index * 10,
        sharedBlocksHit: 10, sharedBlocksRead: 0, sharedBlocksWritten: 0, tempBlocksRead: 0, tempBlocksWritten: 0, walBytes: 0
      }] }
    };
    statements.push(`(gen_random_uuid(), '${asset}', date_trunc('minute', now()) - interval '${6 - index} minutes',
      'ok', $sample$${JSON.stringify(rows)}$sample$::jsonb)`);
  }
  execFileSync('docker', ['exec', '-i', process.env.POSTGRES_CONTAINER, 'psql', '-X', '-U', 'postgres',
    '-v', 'ON_ERROR_STOP=1', '-q'], {
    input: `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json) VALUES ${statements.join(',')};`
  });
}

async function createAsset(page) {
  await navigate(page, 'assets');
  await page.locator('#asset-add').click();
  await page.locator('#asset-dialog').waitFor({ state: 'visible' });
  await formField(page, 'asset-form', 'runtime').selectOption('k8s');
  await page.locator('#k8s-fields').waitFor();
  await formField(page, 'asset-form', 'runtime').selectOption('managed');
  await page.locator('#k8s-fields').waitFor({ state: 'hidden' });
  await page.locator('#asset-integrations > summary').click();
  await formField(page, 'asset-form', 'poolerUrl').waitFor();
  for (const [name, value] of Object.entries({
    name: primary, clusterName: 'Browser QA', environment: 'acceptance', host: '127.0.0.1', port: '15434',
    username: 'postgres', password: 'dbsphere-browser-fixture'
  })) await formField(page, 'asset-form', name).fill(value);
  await formField(page, 'asset-form', 'sslMode').selectOption('disable');
  const response = await action(page, page.locator('#asset-submit'), 'POST', '/api/assets', 201);
  const { asset } = await response.json();
  await page.locator('#asset-dialog').waitFor({ state: 'hidden' });
  await visible(page.locator('#asset-list'), primary);
  const search = page.locator('#asset-search');
  await search.fill('no-such-asset');
  await page.waitForFunction(name => !document.getElementById('asset-list').textContent.includes(name), primary);
  await search.fill(primary);
  await page.locator('#asset-runtime').selectOption('managed');
  await visible(page.locator('#asset-list'), primary);
  await page.locator('#asset-import-panel > summary').click();
  const file = await Promise.all([page.waitForEvent('download'), page.locator('#asset-import-export').click()]);
  assert.match(file[0].suggestedFilename(), /\.ya?ml$/i);
  proof.push('Asset creation, runtime-dependent fields, search, runtime filter, and YAML export');
  return asset.id;
}

async function wallboard(page, version) {
  const board = name => page.locator(`[data-board="${name}"]`);
  await navigate(page, 'wallboard');
  await visible(board('version'), 'v' + version);
  const dialog = board('chart-dialog');
  const mini = page.locator('[data-board=charts] button[aria-haspopup=dialog]');
  assert.equal(await mini.count(), 12);

  // The initial setup has no assets, but every plot can still be opened and closed.
  await page.waitForFunction(() => document.querySelector('[data-board=pulse]').dataset.state === 'ok');
  await mini.first().click();
  await visible(dialog, '请选择实例');
  await page.keyboard.press('Escape');
  await dialog.waitFor({ state: 'hidden' });
  assert(await mini.first().evaluate(element => document.activeElement === element));

  const asset = await createAsset(page);
  seedSamples(asset);
  await navigate(page, 'wallboard');
  await page.waitForFunction(() => document.querySelectorAll('[data-board=charts] [data-available=true]').length === 12);
  const initial = route(page);
  for (let index = 0; index < 12; index++) {
    const plot = mini.nth(index);
    const title = (await plot.getAttribute('aria-label')).slice(2);
    const original = await plot.boundingBox();
    await plot.click();
    await dialog.getByRole('heading', { name: title, exact: true }).waitFor();
    await visible(board('chart-context'), primary);
    const large = await board('chart-plot').boundingBox();
    assert(large.width > original.width && large.height > original.height, title + ' did not grow');
    assert.equal(await dialog.getAttribute('data-available'), 'true');
    assert.equal(await board('chart-values').innerText(),
      await plot.locator('..').locator('.wallboard-chart-values').innerText());
    await board('chart-close').click();
    await dialog.waitFor({ state: 'hidden' });
    assert(await plot.evaluate(element => document.activeElement === element));
    assert.equal(route(page), initial);
  }

  await mini.first().focus();
  await page.keyboard.press('Enter');
  await board('chart-plot').press('ArrowLeft');
  await board('chart-plot').press('ArrowLeft');
  assert(parameters(page).get('cursor') && parameters(page).get('from') && parameters(page).get('end'));
  const pinned = route(page);
  await page.keyboard.press('Escape');
  await dialog.waitFor({ state: 'hidden' });
  assert.equal(route(page), pinned);
  await board('raw').waitFor();
  const raw = await page.request.get(origin + await board('raw').getAttribute('href'));
  assert.equal(raw.status(), 200);
  await board('clear').click();
  assert.equal(parameters(page).has('cursor'), false);

  await board('pause').click();
  assert.equal(await board('pause').getAttribute('aria-pressed'), 'true');
  await board('fullscreen').click();
  await page.waitForFunction(() => document.fullscreenElement?.id === 'wallboard-workspace');
  await mini.first().click();
  await dialog.waitFor();
  await board('chart-close').click();
  await board('fullscreen').click();
  await page.waitForFunction(() => !document.fullscreenElement);
  await board('pause').click();
  assert.equal(await board('pause').getAttribute('aria-pressed'), 'false');

  const theme = page.locator('#theme-select');
  await theme.selectOption('light');
  await page.waitForFunction(() => document.documentElement.dataset.theme === 'light');
  await page.setViewportSize({ width: 390, height: 844 });
  await mini.nth(11).tap();
  await dialog.waitFor();
  const bounds = await dialog.boundingBox();
  assert(bounds.x >= 0 && bounds.x + bounds.width <= 390);
  await board('chart-close').tap();
  await page.setViewportSize({ width: 1440, height: 880 });
  await theme.selectOption('dark');
  await page.waitForFunction(() => document.documentElement.dataset.theme === 'dark');
  await mini.first().click();
  if (process.env.DBSPHERE_BROWSER_OUTPUT) {
    await page.screenshot({ path: join(process.env.DBSPHERE_BROWSER_OUTPUT, 'expanded-wallboard.png') });
  }
  await board('chart-close').click();

  await board('environment').selectOption('acceptance');
  await visible(board('focus'), primary);
  await board('window').selectOption('15');
  assert.equal(parameters(page).get('window'), '15');
  await board('live').click();
  await page.waitForFunction(() => document.body.dataset.activeView === 'live');
  assert.equal(parameters(page).get('asset'), asset);
  proof.push('All 12 plots populated and enlarged, empty-state view, cursor and raw sample, focus return, themes, touch, full screen, pause, filters, and live handoff');
  return asset;
}

async function monitoring(page, asset) {
  const overview = name => page.locator(`[data-overview="${name}"]`);
  const live = name => page.locator(`[data-live="${name}"]`);
  const storage = name => page.locator(`[data-storage="${name}"]`);
  await live('pause').click();
  assert.equal(await live('pause').getAttribute('aria-pressed'), 'true');
  await live('night').click();
  assert.equal(await live('night').getAttribute('aria-pressed'), 'true');
  await live('baseline').click();
  assert.equal(await live('baseline').getAttribute('aria-pressed'), 'true');
  await live('path').locator('summary').click();
  assert(await live('path').getAttribute('open') !== null);
  await live('sql-panel').locator('summary').click();
  assert(await live('sql-panel').getAttribute('open') !== null);
  await live('storage').click();
  await page.waitForFunction(() => document.body.dataset.activeView === 'storage');
  await storage('history').click();
  await storage('range').selectOption('6');
  for (const tab of ['resources', 'containers', 'cache', 'claims']) {
    await storage(tab).click();
    assert.equal(await storage(tab).getAttribute('aria-pressed'), 'true');
  }
  await storage('live').click();
  await storage('back').click();
  await page.waitForFunction(() => document.body.dataset.activeView !== 'storage');

  await navigate(page, 'overview');
  await overview('interval').selectOption('60');
  await overview('pause').click();
  assert.equal(await overview('pause').getAttribute('aria-pressed'), 'true');
  await overview('pause').click();
  await overview('environment').selectOption('acceptance');
  await visible(overview('matrix'), primary);
  const bin = overview('timeline').getByRole('button').first();
  await bin.click();
  await overview('inspection').waitFor();
  await overview('inspection-close').click();
  await overview('inspection').waitFor({ state: 'hidden' });

  await navigate(page, 'monitoring');
  const workbench = page.locator('#monitoring-workspace');
  await workbench.getByLabel('数据库实例', { exact: true }).selectOption(asset);
  const tabs = workbench.locator('.monitor-tabs > button');
  await tabs.first().waitFor();
  const labels = await tabs.allTextContents();
  assert(labels.length >= 6, 'Instance monitoring tabs did not load');
  for (const label of labels) {
    const tab = workbench.locator('.monitor-tabs').getByRole('button', { name: label, exact: true });
    await tab.click();
    assert.equal(await tab.getAttribute('aria-pressed'), 'true');
    await page.waitForFunction(() => document.querySelector('#monitoring-workspace .monitor-content').getAttribute('aria-busy') !== 'true');
  }
  await workbench.getByRole('button', { name: '编辑时段' }).click();
  await workbench.getByRole('button', { name: '取消', exact: true }).click();

  await page.goto(origin + '/#grafana?panel=dbsphere-overview');
  await visible(page.locator('#grafana-status'), '观测专题暂不可用');
  await page.locator('#grafana-retry').click();
  await visible(page.locator('#grafana-status'), '观测专题暂不可用');
  await page.locator('#grafana-back').click();
  await page.waitForFunction(() => document.body.dataset.activeView !== 'grafana');
  proof.push({ monitoring: labels, exercised: 'Overview selection, live controls, storage tabs, and Grafana offline fallback' });
}

async function operations(page, asset) {
  await navigate(page, 'alerts');
  for (const section of ['active', 'rules', 'rca', 'delivery', 'silences']) await selectSection(page, 'alerts', section);
  const reason = 'Browser acceptance maintenance';
  await formField(page, 'silence-form', 'assetId').selectOption(asset);
  await formField(page, 'silence-form', 'kind').selectOption('maintenance');
  await formField(page, 'silence-form', 'reason').fill(reason);
  await action(page, page.locator('#silence-form button[type=submit]'), 'POST', '/api/silences', 201);
  await visible(page.locator('#silence-list'), reason);

  await navigate(page, 'inspections');
  await selectSection(page, 'inspections', 'schedules');
  await formField(page, 'inspection-form', 'assetId').selectOption(asset);
  await formField(page, 'inspection-form', 'intervalMinutes').fill('30');
  await action(page, page.locator('#inspection-form button[type=submit]'), 'PUT', '/api/inspection-schedules', 200);
  const response = await action(page, page.locator('#inspection-run'), 'POST', '/api/inspections/run', 201);
  const { run } = await response.json();
  assert(run.id && run.status !== 'failed', 'Manual inspection failed');
  await selectSection(page, 'inspections', 'history');
  const result = page.locator('#inspection-runs details').filter({ hasText: primary }).first();
  await result.locator('summary').click();
  const [download] = await Promise.all([
    page.waitForEvent('download'), result.getByRole('button', { name: '下载 Markdown' }).click()
  ]);
  assert.match(download.suggestedFilename(), /\.md$/);
  await selectSection(page, 'inspections', 'templates');
  await page.locator('#inspection-templates summary').first().click();
  await page.getByRole('button', { name: '保存模板阈值' }).first().waitFor();

  await navigate(page, 'settings');
  for (const section of ['storage', 'history-tiers', 'collector', 'platform', 'organizations', 'access', 'audit']) {
    await selectSection(page, 'settings', section);
  }
  await selectSection(page, 'settings', 'organizations');
  const organization = 'Browser QA organization';
  await formField(page, 'organization-form', 'name').fill(organization);
  await action(page, page.locator('#organization-form button'), 'POST', '/api/admin/organizations', 201);
  await visible(page.locator('#organization-list'), organization);

  await navigate(page, 'incidents');
  await page.locator('#event-status').selectOption('open');
  await page.locator('#event-search').fill('The QA event search');
  await page.waitForFunction(() => document.getElementById('event-filter-summary').textContent.length > 0);
  proof.push('Alert topics and maintenance, inspection scheduling and manual run, Markdown download, all system settings sections, organization creation, and incident filters');
}

(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 1440, height: 880 }, hasTouch: true });
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    await page.goto(origin);
    await login(page, 'admin', process.env.DBSPHERE_ADMIN_PASSWORD);
    await page.locator('[data-nav=wallboard]').waitFor();
    const asset = await wallboard(page, process.env.DBSPHERE_TEST_VERSION);
    await monitoring(page, asset);
    await operations(page, asset);
    await page.locator('#logout').click();
    await page.locator('#login-form').waitFor();
    await login(page, 'viewer', process.env.DBSPHERE_VIEWER_PASSWORD);
    await page.locator('[data-nav=wallboard]').waitFor();
    assert(await page.locator('[data-nav=logs]').isHidden());
    await navigate(page, 'assets');
    assert(await page.locator('#asset-add').isHidden());
    await navigate(page, 'settings');
    await page.locator('#storage-detail').waitFor();
    assert(await page.locator('[data-section=access]').isHidden());
    await page.locator('#logout').click();
    await page.locator('#login-form').waitFor();
    proof.push('Admin login, logout, viewer login, and read-only restrictions');
    assert.deepEqual(errors, [], 'Uncaught browser errors');
    console.log(JSON.stringify({ result: 'pass', checks: proof }, null, 2));
  } catch (error) {
    console.error('Completed steps:', JSON.stringify(proof), 'Current URL:', page.url());
    if (process.env.DBSPHERE_BROWSER_OUTPUT) {
      await page.screenshot({ path: join(process.env.DBSPHERE_BROWSER_OUTPUT, 'browser-failure.png') }).catch(() => {});
    }
    throw error;
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
