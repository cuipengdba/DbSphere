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
    (path instanceof RegExp ? path.test(new URL(response.url()).pathname) : new URL(response.url()).pathname === path));
  await locator.click();
  const response = await pending;
  assert.equal(response.status(), status, method + ' ' + path + ' ' + await response.text());
  return response;
}

async function download(page, control, suffix) {
  const [file] = await Promise.all([page.waitForEvent('download'), control.click()]);
  assert.match(file.suggestedFilename(), suffix);
  assert.equal(await file.failure(), null);
}

async function setTimeFields(page, from, to) {
  const [start, end] = await page.evaluate(() => {
    const date = new Date();
    const localEnd = date.getTime() - date.getTimezoneOffset() * 60_000;
    return [localEnd - 60 * 60_000, localEnd].map(value => new Date(value).toISOString().slice(0, 16));
  });
  await from.fill(start);
  await to.fill(end);
}

async function fullscreen(control) {
  await control.click();
  await control.and(control.page().locator('[aria-pressed=true]')).waitFor();
  await control.click();
  await control.and(control.page().locator('[aria-pressed=false]')).waitFor();
}

function runFixtureSQL(sql) {
  const args = ['-X', '-U', 'postgres', '-v', 'ON_ERROR_STOP=1', '-q'];
  const native = process.env.DBSPHERE_BROWSER_PSQL;
  execFileSync(native || 'docker', native ? ['-h', '127.0.0.1', '-p', '15434', ...args] :
    ['exec', '-i', process.env.POSTGRES_CONTAINER, 'psql', ...args], {
    env: { ...process.env, PGPASSWORD: 'dbsphere-browser-fixture' }, input: sql
  });
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
  runFixtureSQL(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json) VALUES ${statements.join(',')};`);
}

async function createAsset(page) {
  await navigate(page, 'assets');
  await page.locator('#asset-add').click();
  await page.locator('#asset-dialog').waitFor({ state: 'visible' });
  await formField(page, 'asset-form', 'runtime').selectOption('k8s');
  await page.locator('#k8s-fields').waitFor();
  await visible(page.locator('#k8s-fields'), '发现此 Namespace');
  await formField(page, 'asset-form', 'runtime').selectOption('mysql');
  await page.locator('#k8s-fields').waitFor({ state: 'hidden' });
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
  const assetRow = page.locator('#asset-list article').filter({ hasText: primary });
  await assetRow.getByRole('button', { name: '查看', exact: true }).click();
  await visible(page.locator('#detail'), primary);
  await assetRow.getByRole('button', { name: '编辑', exact: true }).click();
  assert.equal(await formField(page, 'asset-form', 'name').inputValue(), primary);
  await page.locator('#cancel-edit').click();
  await page.locator('#asset-dialog').waitFor({ state: 'hidden' });
  await page.locator('#asset-import-panel > summary').click();
  await download(page, page.locator('#asset-import-export'), /\.ya?ml$/i);
  proof.push('Asset creation, details, editing and cancellation, search, runtime filter, and YAML export');
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
    assert.equal(await board('chart-scale').locator('span').count(), 3);
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
  await board('window').selectOption('15');
  await page.waitForFunction(() => document.querySelectorAll('[data-board=charts] [data-available=true]').length === 12);
  await mini.first().click();
  if (process.env.DBSPHERE_BROWSER_OUTPUT) {
    await page.screenshot({ path: join(process.env.DBSPHERE_BROWSER_OUTPUT, 'expanded-wallboard.png') });
  }
  await board('chart-close').click();

  await board('environment').selectOption('acceptance');
  await visible(board('focus'), primary);
  await board('cluster').selectOption({ label: 'Browser QA' });
  await board('window').selectOption('fixed');
  await setTimeFields(page, board('from'), board('to'));
  await board('dates').getByRole('button', { name: '应用', exact: true }).click();
  await page.waitForFunction(() => new URLSearchParams(location.hash.split('?')[1]).has('from'));
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
  await live('engine').selectOption('postgres');
  await live('environment').selectOption('acceptance');
  await live('cluster').selectOption({ label: 'Browser QA' });
  await live('window').selectOption('15');
  await fullscreen(live('fullscreen'));
  const metrics = live('metrics').locator('button:not([disabled])');
  assert(await metrics.count() > 1, 'Missing live metric selectors');
  for (const label of await metrics.allTextContents()) {
    const selector = live('metrics').getByRole('button', { name: label, exact: true });
    await selector.click();
    assert.equal(await selector.getAttribute('aria-pressed'), 'true');
  }
  await live('path').locator('summary').click();
  assert(await live('path').getAttribute('open') !== null);
  await live('sql-panel').locator('summary').click();
  assert(await live('sql-panel').getAttribute('open') !== null);
  await live('storage').click();
  await page.waitForFunction(() => document.body.dataset.activeView === 'storage');
  await storage('history').click();
  await storage('range').selectOption('6');
  await storage('compare').check();
  assert(await storage('compare').isChecked());
  await storage('range').selectOption('custom');
  await setTimeFields(page, storage('from'), storage('to'));
  await storage('dates').getByRole('button', { name: '应用', exact: true }).click();
  await page.waitForFunction(() => new URLSearchParams(location.hash.split('?')[1]).has('from'));
  await fullscreen(storage('full'));
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
  await overview('cluster').selectOption({ label: 'Browser QA' });
  await overview('range').selectOption('custom');
  await setTimeFields(page, overview('from'), overview('to'));
  await overview('dates').getByRole('button', { name: '应用时段' }).click();
  await page.waitForFunction(() => new URLSearchParams(location.hash.split('?')[1]).has('from'));
  await fullscreen(overview('fullscreen'));
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
  await workbench.getByRole('button', { name: '编辑时段' }).click();
  const dateFields = workbench.locator('input[type=datetime-local]:visible');
  await setTimeFields(page, dateFields.nth(0), dateFields.nth(1));
  await workbench.getByRole('button', { name: '应用范围' }).click();
  await dateFields.first().waitFor({ state: 'hidden' });
  await workbench.getByRole('button', { name: '刷新', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('#monitoring-workspace .monitor-content').getAttribute('aria-busy') !== 'true');

  await page.goto(origin + '/#grafana?panel=dbsphere-overview');
  await visible(page.locator('#grafana-status'), '观测专题暂不可用');
  await page.locator('#grafana-retry').click();
  await visible(page.locator('#grafana-status'), '观测专题暂不可用');
  await page.locator('#grafana-back').click();
  await page.waitForFunction(() => document.body.dataset.activeView !== 'grafana');
  proof.push({ monitoring: labels, exercised: 'Overview and live scopes, metrics, fullscreens, preset and custom periods, storage mode and comparison, workbench refresh, and Grafana offline fallback' });
}

async function operations(page, asset) {
  await navigate(page, 'assets');
  const row = page.locator('#asset-list article').filter({ hasText: primary });
  const checked = await action(page, row.getByRole('button', { name: '采集', exact: true }),
    'POST', '/api/assets/' + asset + '/check', 200);
  assert.equal((await checked.json()).result.status, 'ok');
  await page.locator('#refresh').click();
  await page.locator('#asset-readiness-batch > summary').click();
  await row.getByRole('checkbox', { name: '体检 ' + primary }).check();
  await action(page, page.locator('#asset-run-readiness'), 'POST', '/api/assets/import/probe', 200);
  const readiness = page.locator('#asset-readiness-results details').first();
  await readiness.locator('summary').click();
  await download(page, readiness.getByRole('button', { name: '导出体检 JSON' }), /\.json$/);
  const retryReadiness = page.locator('#asset-retry-readiness');
  if (await retryReadiness.isVisible()) await action(page, retryReadiness, 'POST', '/api/assets/import/probe', 200);

  const importPanel = page.locator('#asset-import-panel');
  if (!(await importPanel.evaluate(panel => panel.open))) await importPanel.locator(':scope > summary').click();
  const manifest = page.locator('#asset-import-form');
  const importedName = 'Browser import smoke';
  await manifest.locator('[name=content]').fill(JSON.stringify({ assets: [{
    name: importedName, runtime: 'managed', host: '127.0.0.1', port: 15435, database: 'postgres',
    username: 'postgres', sslMode: 'disable', environment: 'acceptance', credentialRef: 'asset:' + asset
  }] }));
  const preview = await action(page, manifest.getByRole('button', { name: '预览变更' }), 'POST', '/api/assets/import/preview', 200);
  assert((await preview.json()).canApply, await preview.text());
  await visible(page.locator('#asset-import-status'), '可应用');
  await action(page, page.locator('#asset-import-apply'), 'POST', '/api/assets/import/apply', 200);
  await visible(page.locator('#asset-import-status'), '已应用 1 项');
  await action(page, page.locator('#asset-import-probe'), 'POST', '/api/assets/import/probe', 200);
  await page.locator('#asset-search').fill(importedName);
  const imported = page.locator('#asset-list article').filter({ hasText: importedName });
  await imported.getByRole('button', { name: '编辑', exact: true }).click();
  const renamed = importedName + ' edited';
  await formField(page, 'asset-form', 'name').fill(renamed);
  await action(page, page.locator('#asset-submit'), 'PATCH', /^\/api\/assets\/[a-f0-9-]+$/, 200);
  const edited = page.locator('#asset-list article').filter({ hasText: renamed });
  await visible(edited, renamed);
  page.once('dialog', dialog => dialog.dismiss());
  const deleteButton = edited.getByRole('button', { name: '删除' });
  await deleteButton.click();
  await deleteButton.waitFor();
  page.once('dialog', dialog => dialog.accept());
  await action(page, deleteButton, 'DELETE', /^\/api\/assets\/[a-f0-9-]+$/, 200);
  await edited.waitFor({ state: 'hidden' });
  await page.locator('#asset-search').fill(primary);
  await row.getByRole('button', { name: '查看', exact: true }).click();
  const details = page.locator('#detail');
  const assetTabs = details.getByRole('tab');
  await assetTabs.first().waitFor();
  for (const label of await assetTabs.allTextContents()) {
    const tab = details.getByRole('tab', { name: label, exact: true });
    await tab.click();
    assert.equal(await tab.getAttribute('aria-selected'), 'true');
  }
  await action(page, details.getByRole('button', { name: '7 天', exact: true }), 'GET', `/api/assets/${asset}/series`, 200);
  const historyChart = details.locator('details').filter({ has: page.getByText('分层历史趋势', { exact: true }) });
  await historyChart.locator('summary').click();
  await historyChart.locator('[name=period]').selectOption('7');
  await action(page, historyChart.getByRole('button', { name: '查看历史' }), 'GET', `/api/assets/${asset}/history-tier`, 200);
  const risk = details.locator('.risk-panel');
  await risk.locator('summary').click();
  await action(page, risk.getByRole('button', { name: '运行近 7 天回测' }), 'GET', `/api/assets/${asset}/risk/backtest`, 200);
  await risk.getByText('证据不足', { exact: true }).waitFor();

  await details.getByRole('tab', { name: '慢 SQL 日志', exact: true }).click();
  const slow = page.locator('#asset-layer-slow-logs');
  await slow.locator('select').selectOption('jsonlog');
  const log = JSON.stringify({ timestamp: new Date().toISOString(), dbname: 'postgres', query_id: 163, pid: 12,
    application_name: 'ui-acceptance', message: 'duration: 2500 ms statement: select 1' }) + '\n';
  await slow.locator('input[type=file]').setInputFiles({ name: 'ui-slow.json', mimeType: 'application/json', buffer: Buffer.from(log) });
  const importedLog = await action(page, slow.getByRole('button', { name: '增量导入' }), 'POST', `/api/assets/${asset}/slow-logs/import`, 200);
  assert.equal((await importedLog.json()).imported, 1);
  await visible(slow, 'Query ID 163');

  await details.getByRole('tab', { name: '执行计划', exact: true }).click();
  const plans = page.locator('#asset-layer-sql-plans');
  await plans.getByPlaceholder('Query ID').fill('163');
  const plan = JSON.stringify([{ 'Plan': { 'Node Type': 'Result', 'Actual Total Time': 0.02, 'Actual Rows': 1 }, 'Query Identifier': 163 }]);
  await plans.locator('input[type=file]').setInputFiles({ name: 'ui-plan.json', mimeType: 'application/json', buffer: Buffer.from(plan) });
  let resumeRefresh;
  const holdRefresh = new Promise(resolve => { resumeRefresh = resolve; });
  await page.route(origin + `/api/assets/${asset}`, async route => { await holdRefresh; await route.continue(); }, { times: 1 });
  const refreshed = page.waitForResponse(response => response.url() === origin + `/api/assets/${asset}`);
  try {
    await action(page, plans.getByRole('button', { name: '导入计划' }), 'POST', `/api/assets/${asset}/sql-plans`, 201);
    await details.getByRole('tab', { name: '代理与访问入口', exact: true }).click();
  } finally { resumeRefresh(); }
  assert((await refreshed).ok());
  const routing = page.locator('#asset-layer-routing-detail');
  await routing.getByText('配置只读入口探测', { exact: true }).click();
  await routing.getByLabel('入口类型').selectOption('haproxy');
  await routing.getByLabel('入口主机与端口').fill('127.0.0.1:15434');
  await action(page, routing.getByRole('button', { name: '保存入口配置' }), 'PATCH', `/api/assets/${asset}/routing`, 200);
  await visible(routing, '等待新配置的首次采样。');
  proof.push('Asset refresh and bulk onboarding, readiness and JSON export, edit and deletion, history and risk controls, log and plan import, and route editing across a concurrent refresh');

  await navigate(page, 'alerts');
  for (const section of ['active', 'rules', 'rca', 'delivery', 'silences']) await selectSection(page, 'alerts', section);
  await selectSection(page, 'alerts', 'rules');
  const rule = page.locator('#rule-list details').filter({ has: page.locator('form') }).first();
  await rule.locator('summary').click();
  const enabled = rule.getByRole('checkbox', { name: '启用' });
  await enabled.setChecked(!(await enabled.isChecked()));
  await action(page, rule.getByRole('button', { name: '保存规则' }), 'PATCH', /^\/api\/alert-rules\//, 200);
  await selectSection(page, 'alerts', 'silences');
  const reason = 'Browser acceptance maintenance';
  await formField(page, 'silence-form', 'assetId').selectOption(asset);
  await formField(page, 'silence-form', 'kind').selectOption('maintenance');
  await formField(page, 'silence-form', 'reason').fill(reason);
  await action(page, page.locator('#silence-form button[type=submit]'), 'POST', '/api/silences', 201);
  await visible(page.locator('#silence-list'), reason);
  const silence = page.locator('#silence-list article').filter({ hasText: reason });
  await action(page, silence.getByRole('button', { name: '撤销' }), 'DELETE', /^\/api\/silences\//, 200);
  await visible(silence, '已撤销');
  await selectSection(page, 'alerts', 'rca');
  const rca = page.locator('#rca-workbench');
  await rca.getByRole('textbox', { name: '版本变更原因' }).fill('Browser verification of rule revisions');
  const savedRule = await action(page, rca.getByRole('button', { name: '保存规则版本' }), 'PATCH', /^\/api\/rca\/rules\//, 200);
  const revision = await savedRule.json();
  await visible(rca, `当前版本 v${revision.version}`);

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
  await download(page, result.getByRole('button', { name: '下载 Markdown' }), /\.md$/);
  await download(page, result.getByRole('button', { name: '下载 HTML' }), /\.html$/);
  const repeated = await action(page, result.getByRole('button', { name: '重新巡检' }), 'POST', `/api/inspection-runs/${run.id}/retry`, 201);
  assert.equal((await repeated.json()).run.retryOf, run.id);
  await selectSection(page, 'inspections', 'templates');
  const template = page.locator('#inspection-templates details').first();
  await template.locator('summary').click();
  await action(page, template.getByRole('button', { name: '保存模板阈值' }), 'PATCH', /^\/api\/inspection-templates\//, 200);

  await navigate(page, 'settings');
  for (const section of ['storage', 'history-tiers', 'collector', 'platform', 'organizations', 'access', 'audit']) {
    await selectSection(page, 'settings', section);
  }
  await selectSection(page, 'settings', 'organizations');
  const organization = 'Browser QA organization';
  await formField(page, 'organization-form', 'name').fill(organization);
  await action(page, page.locator('#organization-form button'), 'POST', '/api/admin/organizations', 201);
  await visible(page.locator('#organization-list'), organization);
  await selectSection(page, 'settings', 'history-tiers');
  const history = page.locator('#history-policies');
  await history.locator('[name=rawDays]').fill('9');
  await action(page, history.getByRole('button', { name: '保存保留策略' }), 'PUT', '/api/admin/history-retention', 200);
  const policy = history.locator('.list-item').filter({ hasText: '原始 9 天' });
  await policy.getByRole('button', { name: '编辑' }).click();
  assert.equal(await history.locator('[name=rawDays]').inputValue(), '9');
  await selectSection(page, 'settings', 'collector');
  const collector = page.locator('#collector-status-detail');
  for (const [label, endpoint] of [
    ['查看采集队列', 'queue'], ['查看小时汇总进度', 'hourly-trends'],
    ['查看采集覆盖与耗时', 'health'], ['查看近 7 天采集覆盖', 'coverage']
  ]) {
    await action(page, collector.getByRole('button', { name: label, exact: true }), 'GET', '/api/admin/collector/' + endpoint, 200);
  }
  await selectSection(page, 'settings', 'platform');
  await page.locator('#platform-refresh').click();
  await page.locator('#platform-status-detail :is(.evidence,.hint)').first().waitFor();
  await selectSection(page, 'settings', 'audit');
  await page.locator('#admin-audit-panel summary').click();
  await page.locator('#admin-audit-list').waitFor();

  await selectSection(page, 'settings', 'access');
  const directory = page.locator('#access-directory-content');
  const team = directory.locator('div.settings-details').filter({ has: page.getByText('新建团队', { exact: true }) });
  await team.locator('[name=project]').fill('UI coverage');
  const directoryRefresh = page.waitForResponse(response => new URL(response.url()).pathname === '/api/admin/access');
  await page.locator('[data-view=settings] [data-section=access]').evaluate(button => button.click());
  assert((await directoryRefresh).ok());
  await team.locator('[name=name]').fill('QA operators');
  await team.locator('[name=assetIds]').fill(asset);
  assert.equal(await team.locator('[name=project]').inputValue(), 'UI coverage');
  const teamResponse = await action(page, team.getByRole('button', { name: '创建团队' }), 'POST', '/api/admin/teams', 201);
  const createdTeam = (await teamResponse.json()).team;
  const teamSettings = directory.locator('details').filter({ hasText: 'QA operators' });
  await teamSettings.locator('summary').click();
  await teamSettings.locator('[name=project]').fill('UI coverage updated');
  await action(page, teamSettings.getByRole('button', { name: '保存团队' }), 'PATCH', `/api/admin/teams/${createdTeam.id}`, 200);
  const account = directory.locator('div.settings-details').filter({ has: page.getByText('新建或迁移账号', { exact: true }) });
  await account.locator('[name=username]').fill('ui-cover-operator');
  await account.locator('[name=password]').fill('ui-cover-operator-fixture');
  await account.locator('[name=role]').selectOption('operator');
  await account.locator('[name=teamIds]').selectOption(createdTeam.id);
  const accountResponse = await action(page, account.getByRole('button', { name: '创建账号' }), 'POST', '/api/admin/accounts', 201);
  const createdAccount = (await accountResponse.json()).account;
  const accountSettings = directory.locator('details').filter({ has: page.getByText(/^ui-cover-operator · /) });
  await accountSettings.locator('summary').click();
  await action(page, accountSettings.getByRole('button', { name: '保存账号' }), 'PATCH', `/api/admin/accounts/${createdAccount.id}`, 200);
  const rotate = directory.locator('details').filter({ has: page.getByText('轮换凭据 · ui-cover-operator', { exact: true }) });
  await rotate.locator('summary').click();
  await rotate.locator('[name=password]').fill('ui-cover-operator-rotated');
  await action(page, rotate.getByRole('button', { name: '轮换密码' }), 'POST', `/api/admin/accounts/${createdAccount.id}/rotate`, 200);
  const scoped = await page.context().browser().newPage();
  try {
    await scoped.goto(origin);
    await login(scoped, 'ui-cover-operator', 'ui-cover-operator-rotated');
    await scoped.locator('[data-nav=wallboard]').waitFor();
    await navigate(scoped, 'assets');
    await visible(scoped.locator('#asset-list'), primary);
    await download(scoped, scoped.locator('#scoped-asset-export'), /\.json$/);
  } finally { await scoped.close(); }

  await navigate(page, 'incidents');
  await page.locator('#event-status').selectOption('open');
  await page.locator('#event-search').fill('The QA event search');
  await page.waitForFunction(() => document.getElementById('event-filter-summary').textContent.length > 0);
  proof.push('Alert and maintenance writes, inspection schedules, template revisions, immediate and repeated runs, both report formats, history policies, collection reports, organization and team creation, account rotation and scoped export, audit, and incident filters');
}

async function pagination(page, asset) {
  const session = await (await page.request.get(origin + '/api/session')).json();
  const organizations = await (await page.request.get(origin + '/api/admin/organizations')).json();
  const organizationId = organizations.organizations.find(row => row.name !== 'Browser QA organization').id;
  for (let i = 1; i <= 52; i++) {
    const response = await page.request.post(origin + '/api/assets', {
      headers: { 'X-CSRF-Token': session.csrf },
      data: { organizationId, runtime: 'managed', sslMode: 'disable', environment: 'pagination',
        name: 'Pagination asset ' + i, host: '127.0.0.1', port: 15434, database: 'postgres', username: 'postgres' }
    });
    assert.equal(response.status(), 201, await response.text());
  }
  assert.match(asset, /^[a-f0-9-]{36}$/);
  runFixtureSQL(`WITH keys AS (
    INSERT INTO event_keys (id,asset_id,started_at)
    SELECT gen_random_uuid(),'${asset}',now() - n * interval '1 second' FROM generate_series(1,53) AS n
    RETURNING id,asset_id,started_at
  ) INSERT INTO events (id,asset_id,kind,title,status,started_at,ended_at,event_group)
  SELECT id,asset_id,'collection_failed','Browser pagination event','resolved',started_at,now(),'telemetry' FROM keys;`);
  await page.goto(origin + '/#wallboard');
  const pageNumber = page.locator('[data-board=page]');
  await visible(pageNumber, '1 / 3');
  await page.locator('[data-board=next]').click();
  await visible(pageNumber, '2 / 3');
  await page.locator('[data-board=next]').click();
  await visible(pageNumber, '3 / 3');
  assert(await page.locator('[data-board=next]').isDisabled());
  await page.locator('[data-board=prev]').click();
  await visible(pageNumber, '2 / 3');
  await page.locator('[data-board=prev]').click();
  await visible(pageNumber, '1 / 3');
  assert(await page.locator('[data-board=prev]').isDisabled());
  await navigate(page, 'overview');
  for (const filter of ['organization', 'environment', 'cluster']) await page.locator(`[data-overview=${filter}]`).selectOption('');
  const overviewNext = page.locator('[data-overview=next]');
  const overviewPrev = page.locator('[data-overview=prev]');
  await overviewNext.click();
  assert(Number(parameters(page).get('offset')) > 0);
  await overviewPrev.click();
  assert(!parameters(page).has('offset'));
  await navigate(page, 'assets');
  await page.locator('#asset-search').clear();
  await page.locator('#asset-runtime').selectOption('all');
  const next = page.locator('#asset-next');
  const previous = page.locator('#asset-prev');
  await next.click();
  await visible(page.locator('#asset-filter-summary'), '第 2 页');
  await previous.click();
  await visible(page.locator('#asset-filter-summary'), '第 1 页');
  await navigate(page, 'incidents');
  await page.locator('#event-search').fill('Browser pagination event');
  await page.locator('#event-status').selectOption('resolved');
  await page.locator('#event-next').click();
  await visible(page.locator('#event-filter-summary'), '第 2 页');
  await page.locator('#event-prev').click();
  await visible(page.locator('#event-filter-summary'), '第 1 页');
  proof.push('Wallboard, overview, asset and incident pagination with 53 registered assets and 53 resolved events');
}

(async () => {
  const browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width: 1440, height: 880 }, hasTouch: true });
  page.setDefaultTimeout(15000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  try {
    await page.goto(origin);
    await login(page, 'admin', 'wrong-browser-fixture-password');
    await visible(page.locator('#login-error'), 'Invalid credentials');
    await login(page, 'admin', process.env.DBSPHERE_ADMIN_PASSWORD);
    await page.locator('[data-nav=wallboard]').waitFor();
    const asset = await wallboard(page, process.env.DBSPHERE_TEST_VERSION);
    await monitoring(page, asset);
    await operations(page, asset);
    await pagination(page, asset);
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
