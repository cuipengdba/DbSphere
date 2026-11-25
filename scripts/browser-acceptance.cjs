const assert = require('node:assert/strict');
const { randomUUID } = require('node:crypto');
const { createServer } = require('node:http');
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
    env: { ...process.env, PGPASSWORD: native ? process.env.PGPASSWORD : 'dbsphere-browser-fixture' }, input: sql
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
      diagnostics: { instanceSessions: { status: 'ok', data: { waiting: index, active: index + 1, usedPercent: 42 } },
        activeWaits: { status: 'ok', data: { groups: [
          { depth: 3, waitType: 'Lock', sessions: index },
          { depth: 1, waitType: 'Lock', waitEvent: 'transactionid', sessions: index },
          { depth: 0, waitType: 'Lock', waitEvent: 'transactionid', queryId: '163', sessions: index }
        ] } },
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
  runFixtureSQL(`INSERT INTO slow_log_entries(id,asset_id,source_key,observed_at,duration_ms,query_id,source_format,imported_at,database_name)
    SELECT gen_random_uuid(), '${asset}', 'workbench-' || n, now() - interval '6 minutes', (n * 10)::float8,
      '163', 'csv', now(), 'postgres' FROM generate_series(1,20) n;`);
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
  const rawPath = await board('raw').getAttribute('href');
  const raw = await page.request.get(origin + rawPath);
  assert.equal(raw.status(), 200);
  const { sample } = await raw.json();
  await board('raw').click();
  await visible(page.locator('#sample-details'), sample.id);
  await page.locator('#sample-details summary').filter({ hasText: 'Linux 主机' }).click();
  await visible(page.locator('#sample-details'), 'qa-node:9100');
  await download(page, page.locator('#sample-download'), new RegExp(sample.id + '\\.json$'));
  await page.locator('#sample-return').click();
  await page.waitForURL(url => url.hash === pinned);
  const direct = await page.context().newPage();
  await direct.goto(origin + rawPath);
  await visible(direct.locator('#sample-details'), sample.id);
  assert.equal(new URL(direct.url()).pathname, '/');
  await direct.close();
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
  if (await live('pause').isHidden()) await navigate(page, 'live');
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

  await workbench.getByRole('button', { name: '等待分析', exact: true }).click();
  const heat = workbench.getByRole('table', { name: '每桶平均活跃会话' });
  await heat.waitFor();
  assert.notEqual(await heat.locator('.heat-4').first().evaluate(node => getComputedStyle(node).backgroundColor), 'rgba(0, 0, 0, 0)');
  const heatCell = heat.getByRole('button', { name: /合计.*3 会话/ }).first();
  await heatCell.click();
  assert.equal(await heatCell.getAttribute('aria-pressed'), 'true');
  await visible(workbench, '同窗资源趋势');
  if (process.env.DBSPHERE_BROWSER_OUTPUT) await page.screenshot({ path: join(process.env.DBSPHERE_BROWSER_OUTPUT, 'wait-heatmap.png') });
  const waitChart = workbench.locator('.monitor-focus-chart');
  const plot = waitChart.locator('.monitor-plot');
  const waitRoute = route(page);
  await plot.click();
  const enlarged = workbench.getByRole('dialog', { name: '平均活跃会话 · 放大查看' });
  await enlarged.waitFor();
  await enlarged.locator('.monitor-plot').press('ArrowLeft');
  await page.keyboard.press('Escape');
  await enlarged.waitFor({ state: 'hidden' });
  assert.equal(route(page), waitRoute);
  assert.equal(await plot.evaluate(node => node === document.activeElement), true);
  await heatCell.click();
  await workbench.getByRole('button', { name: '查选中时间桶的锁链', exact: true }).click();
  await page.waitForFunction(() => new URLSearchParams(location.hash.split('?')[1]).has('lockAt'));
  await page.goto(origin + waitRoute);
  await workbench.getByLabel('等待分组').selectOption('query');
  await workbench.getByRole('button', { name: /163/ }).first().click();
  await visible(workbench, '同窗慢日志 · 20 条 · P95 190.5 ms · P99 198.1 ms');
  proof.push('Workbench trend dialog and focus return, synchronized waits-to-locks drill, Query ID and measured percentiles, and readable direct sample links and JSON download');

  await page.goto(origin + '/#grafana?panel=dbsphere-overview');
  await visible(page.locator('#grafana-status'), '观测专题暂不可用');
  await page.locator('#grafana-retry').click();
  await visible(page.locator('#grafana-status'), '观测专题暂不可用');
  await page.locator('#grafana-back').click();
  await page.waitForFunction(() => document.body.dataset.activeView !== 'grafana');
  proof.push({ monitoring: labels, exercised: 'Overview and live scopes, metrics, fullscreens, preset and custom periods, storage mode and comparison, workbench refresh, and Grafana offline fallback' });
}

async function savedDutyView(page, asset) {
  const now = Date.now();
  const from = new Date(now - 10 * 60000).toISOString(), to = new Date(now).toISOString();
  const change = new Date(now - 4 * 60000).toISOString();
  const label = 'Browser QA on-call view';
  runFixtureSQL(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
    SELECT gen_random_uuid(),asset_id,collected_at-interval '1 day',status,
      jsonb_set(metrics_json,'{linux,cpu,usedPercent}','20'::jsonb)
    FROM samples WHERE asset_id='${asset}' AND collected_at BETWEEN '${from}' AND '${to}';
    INSERT INTO asset_changes(asset_id,kind,label,source,occurred_at)
    VALUES ('${asset}','configuration','Browser QA sampled change','operator','${change}');`);
  const board = name => page.locator(`[data-board="${name}"]`);
  const windows = new URLSearchParams({ asset, from, end: to, window: '15' });
  await page.goto(origin + '/#wallboard?' + windows);
  await visible(board('focus'), primary);
  await board('compare').selectOption('day');
  await visible(board('baseline-status'), '虚线基线');
  await visible(board('charts'), '基线 20');
  assert.equal(parameters(page).get('compare'), 'day');
  const previousRoute = route(page);
  await board('baseline-raw').click();
  await visible(page.locator('#sample-details'), asset);
  await page.locator('#sample-return').click();
  await page.waitForURL(url => url.hash === previousRoute);
  await board('layout').selectOption('host');
  await board('metric-group').selectOption('infra');
  assert.equal(await page.locator('.wallboard-chart:visible').count(), 5);
  await board('view-name').fill(label);
  await board('save-view').getByRole('button', { name: '保存视图' }).click();
  await visible(board('view-status'), '已保存 ' + label);
  await board('metric-group').selectOption('all');
  await board('compare').selectOption('');
  await page.reload();
  await board('saved-view').selectOption({label});
  await visible(board('baseline-status'), '虚线基线');
  assert.equal(await board('layout').inputValue(), 'host');
  assert.equal(await board('metric-group').inputValue(), 'infra');
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write']);
  await board('share-view').click();
  await visible(board('view-status'), '已复制视图链接');
  const shared = await page.evaluate(() => navigator.clipboard.readText());
  assert.equal(new URLSearchParams(new URL(shared).hash.split('?')[1]).get('compare'), 'day');
  const guestTab = await page.context().browser().newPage();
  await guestTab.goto(origin);
  await login(guestTab, 'admin', process.env.DBSPHERE_ADMIN_PASSWORD);
  await guestTab.locator('[data-nav=wallboard]').waitFor();
  await guestTab.locator('[data-board=saved-view]').selectOption({label});
  await visible(guestTab.locator('[data-board=baseline-status]'), '虚线基线');
  await guestTab.goto(shared);
  await visible(guestTab.locator('[data-board=baseline-status]'), '虚线基线');
  assert.equal(await guestTab.locator('[data-board=metric-group]').inputValue(), 'infra');
  await guestTab.close();
  if (process.env.DBSPHERE_BROWSER_OUTPUT) await page.screenshot({ path: join(process.env.DBSPHERE_BROWSER_OUTPUT, 'duty-baseline.png') });
  await board('delete-view').click();
  await visible(board('view-status'), '已删除 ' + label);
  const legacyName = 'Browser QA v2.30 saved view';
  await page.evaluate(async ({name, path}) => {
    const session = await fetch('/api/session').then(response => response.json());
    const identity = [session.role || 'admin', session.username || '', session.organizationId || ''];
    localStorage.setItem('dbsphere.wallboard.views.' + JSON.stringify(identity), JSON.stringify([{name, path}]));
  }, {name: legacyName, path: new URL(shared).hash.slice(1)});
  await page.reload();
  await board('import-views').click();
  await visible(board('view-status'), '已导入 1 个视图');
  await board('saved-view').selectOption({label: legacyName});
  await visible(board('baseline-status'), '虚线基线');
  await board('delete-view').click();
  await visible(board('view-status'), '已删除 ' + legacyName);
  await page.getByLabel('选择变更前后对比').selectOption(change);
  await visible(board('baseline-status'), '变更时刻');
  assert.equal(parameters(page).get('compare'), 'previous');
  assert.equal(parameters(page).get('from'), change);
  proof.push('Named duty views survive reload, share the selected layout and metrics, compare a real same-time baseline and change window, and reopen the measured raw sample');
}

async function storageInvestigation(page) {
  const asset = randomUUID(), bare = randomUUID();
  const now = Date.now();
  const from = new Date(now - 10 * 60000).toISOString(), to = new Date(now).toISOString();
  const samples = [];
  for (let i = 0; i < 4; i++) {
    const at = new Date(now - (4 - i) * 60000).toISOString();
    const rates = { reads: 100 + i, writes: 20, readSeconds: .202, writeSeconds: .08, readBytes: 120000,
      writtenBytes: 480000, ioSeconds: .31, weightedIOSeconds: 2.5 };
    const diagnostics = { wal: { data: { bytes: String(1024 + i * 60000), statsReset: fixedTime } },
      checkpoints: { data: { syncMs: i * 20, statsReset: fixedTime } }, database: { data: { tempBytes: i * 4096, statsReset: fixedTime } } };
    const pg = { postgresStartedAt: fixedTime, role: 'primary', diagnostics,
      linux: { status: 'ok', endpoint: 'lab-node:9100', deviceRates: { disk: { sda: rates } }, deviceNumbers: { sda: { number: '8:0' } } } };
    const containers = {};
    for (const [name, scale] of [['postgres', 1], ['sidecar', 10]]) {
      containers[name] = { id: `${name}-${i < 2 ? 'old' : 'current'}`, status: 'ok', observedAt: at, identityChanged: i === 2,
        memory: { active_file: (i + 1) * (1 << 20) * scale, inactive_file: (i + 1) * (1 << 19) * scale,
          file_dirty: 2048 * i, file_writeback: 40 * i }, rates: { refaultPerSecond: i * 5, reclaimsPerSecond: i * 3 },
        pressure: { io_some_avg10: i * 2, memory_some_avg10: i * 3 }, devices: { '8:0': {} }, deviceRates: { '8:0': { readIops: 3, readBps: 1000 } },
        deviceLinks: [{ device: '8:0', nodeDevice: 'sda', scope: 'node_device', to: at, queueDepth: rates.weightedIOSeconds }] };
    }
    const k8s = { ...pg, k8s: { status: 'ok' }, containerStorage: { status: 'ok', runtime: 'k8s', primary: 'postgres',
      node: 'lab-node', nodeUID: 'lab-node-uid', pod: 'pg-observability-0', podUID: 'lab-pod-uid', namespace: 'storage-lab',
      cadenceSeconds: 60, pgIOWait: i, containers, pvcs: [{ name: 'pg-data', uid: 'lab-pvc-uid', phase: 'Bound',
        usage: { status: 'ok', usedBytes: 100000 + i * 40000, capacityBytes: 10000000, observedAt: at } }],
      volumeLinks: [{ container: 'postgres', pvc: 'pg-data', status: 'claim_verified', sharedInPod: true }] } };
    for (const [id, metrics] of [[asset, k8s], [bare, pg]]) {
      samples.push(`(gen_random_uuid(), '${id}', '${at}', 'ok', $data$${JSON.stringify(metrics)}$data$::jsonb)`);
    }
  }
  runFixtureSQL(`INSERT INTO assets(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,environment,created_at)
    VALUES ('${asset}','Storage QA Kubernetes','127.0.0.1',15434,'postgres','postgres','','disable','k8s','storage-acceptance',now()),
      ('${bare}','Storage QA Linux','127.0.0.1',15434,'postgres','postgres','','disable','linux','storage-acceptance',now());
    INSERT INTO samples(id,asset_id,collected_at,status,metrics_json) VALUES ${samples.join(',')};`);
  const storage = name => page.locator(`[data-storage="${name}"]`);
  const scope = (id, topic) => '/#storage?' + new URLSearchParams({ asset: id, topic, mode: 'history', from, to });
  try {
    await page.goto(origin + scope(asset, 'containers'));
    await visible(storage('identity'), 'lab-pod-uid');
    await visible(storage('identity'), '同 Pod 共享');
    await storage('identity').getByRole('button', { name: /节点块设备 sda/ }).first().click();
    const latency = storage('charts').locator('.storage-chart').filter({ hasText: '节点设备延迟' });
    await visible(latency.locator('.storage-chart-readout'), '读 2 ms');
    await visible(storage('charts'), '队列深度 2.5');
    await visible(storage('charts'), 'WAL 1,000 B/s');
    await storage('identity').getByRole('button', { name: /持久卷声明 pg-data/ }).click();
    await visible(storage('stats'), 'PVC 容量');
    assert.equal(parameters(page).get('pvcUid'), 'lab-pvc-uid');
    await storage('cache').click();
    await storage('pin-cache').click();
    await storage('container').selectOption('sidecar');
    const cards = storage('cache-tiles').locator('.cache-card');
    await page.waitForFunction(() => document.querySelectorAll('.cache-card canvas[data-scale-max]').length === 2);
    const plots = cards.locator('canvas');
    assert.equal(await plots.nth(0).getAttribute('data-scale-max'), await plots.nth(1).getAttribute('data-scale-max'));
    await plots.first().press('ArrowLeft');
    assert(parameters(page).has('cursor'));
    await visible(cards.nth(0), '重读 15');
    await visible(cards.nth(1), '重读 15');
    const cursor = parameters(page).get('cursor');
    await cards.first().getByRole('link', { name: '查看时间桶原始采样' }).click();
    await visible(page.locator('#sample-details'), asset);
    await page.locator('#sample-return').click();
    assert.equal(parameters(page).get('cursor'), cursor);
    await cards.nth(1).waitFor();
    if (process.env.DBSPHERE_BROWSER_OUTPUT) await page.screenshot({ path: join(process.env.DBSPHERE_BROWSER_OUTPUT, 'shared-cache-scale.png') });
    await page.goto(origin + scope(bare, 'resources'));
    await storage('device').selectOption('sda');
    await visible(latency.locator('.storage-chart-readout'), '读 2 ms');
    assert.equal(parameters(page).get('nodeDevice'), 'sda');
    assert.equal(parameters(page).has('device'), false);
    proof.push('Container and bare Linux device attribution, PVC UID, WAL / disk correlation, shared cache scale and cursor, and original evidence return');
  } finally {
    runFixtureSQL(`DELETE FROM assets WHERE id IN ('${asset}','${bare}');`);
  }
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
  await routing.getByLabel('入口主机与端口', { exact: true }).fill('127.0.0.1:15434');
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

async function dutyMatrix(page) {
  const states = ['healthy', 'affected', 'maintenance', 'recovered', 'missing'];
  const assets = Object.fromEntries(states.map(state => [state, randomUUID()]));
  const opened = randomUUID(), recovered = randomUUID(), originSample = randomUUID();
  const recoverySample = randomUUID(), triggerEvidence = randomUUID(), recoveryEvidence = randomUUID();
  const rows = states.map((state, index) => `('${assets[state]}','Matrix ${state}','127.0.0.1',15434,'postgres','postgres','','disable','managed',
    'matrix-acceptance','Matrix ${index < 2 ? 'A' : 'B'}',now(),${state === 'missing' ? 'NULL' : 'now()'},
    '${state === 'missing' ? 'pending' : 'ok'}','${index === 0 || index === 3 ? 'replica' : 'primary'}')`);
  runFixtureSQL(`INSERT INTO assets(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,environment,cluster_name,created_at,last_sample_at,last_status,role)
    VALUES ${rows.join(',')};
    INSERT INTO event_keys(id,asset_id,started_at) VALUES ('${opened}','${assets.affected}',now()-interval '3 minutes'),('${recovered}','${assets.recovered}',now()-interval '5 minutes');
    INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at,event_group)
    SELECT id,asset_id,'collection_failure','Matrix ' || CASE WHEN id='${opened}' THEN 'unavailable' ELSE 'recovery' END,
      CASE WHEN id='${opened}' THEN 'open' ELSE 'resolved' END,started_at,CASE WHEN id='${recovered}' THEN now() END,'availability'
    FROM event_keys WHERE id IN ('${opened}','${recovered}');
    INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error)
    SELECT '${originSample}',asset_id,started_at,'error','{"connections":9,"collector":{"pgStatus":"error"}}','Browser capture at incident start'
    FROM event_keys WHERE id='${recovered}';
    INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
    SELECT gen_random_uuid(),asset_id,started_at-interval '1 minute','ok','{"connections":3}' FROM event_keys WHERE id='${recovered}'
    UNION ALL SELECT '${recoverySample}'::uuid,asset_id,ended_at,'ok','{"connections":4}'::jsonb FROM events WHERE id='${recovered}';
    INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
    SELECT '${triggerEvidence}',id,'collection_error',started_at,jsonb_build_object('sampleId','${originSample}'::text,'source','go_collector')
    FROM event_keys WHERE id='${recovered}';
    INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
    SELECT '${recoveryEvidence}',id,'collection_recovered',ended_at,jsonb_build_object('sampleId','${recoverySample}'::text,'source','go_collector')
    FROM events WHERE id='${recovered}';
    INSERT INTO open_event_groups(asset_id,event_group,event_id) VALUES ('${assets.affected}','availability','${opened}');
    INSERT INTO silences(id,kind,asset_id,starts_at,ends_at,reason,created_at)
    VALUES (gen_random_uuid(),'maintenance','${assets.maintenance}',now()-interval '1 minute',now()+interval '5 minutes','Browser maintenance',now());`);
  const board = name => page.locator(`[data-board="${name}"]`);
  await page.goto(origin + '/#wallboard?environment=matrix-acceptance');
  for (const state of states) await board('assets').locator(`.wallboard-tile[data-state="${state}"]`).waitFor();
  for (const [state, key] of [['affected','affectedCount'], ['maintenance','maintenanceCount'], ['recovered','recoveredCount'], ['missing','missingCount']]) {
    const filter = board('kpis').locator(`[data-kpi="${key}"]`);
    assert.equal(await filter.locator('strong').innerText(), '1');
    await filter.click();
    await visible(board('focus'), 'Matrix ' + state);
    assert.equal(parameters(page).get('state'), state);
    await filter.click();
    await board('assets').locator('.wallboard-tile[data-state=healthy]').waitFor();
  }
  await board('assets').getByRole('button', { name: '关注 Matrix healthy', exact: true }).click();
  await board('assets').getByRole('button', { name: '关注 Matrix affected', exact: true }).click();
  await visible(board('watch'), '关注 2/4');
  await board('watch').getByRole('button', { name: /Matrix healthy/ }).click();
  await visible(board('focus'), 'Matrix healthy');
  await board('layout').selectOption('host');
  await board('assets').locator('.wallboard-host').first().waitFor();
  await board('layout').selectOption('cluster');
  await board('event-list').selectOption('resolved');
  await board('events').locator(`[data-event="${recovered}"]`).click();
  await visible(page.locator('#event-detail'), recovered);
  const investigation = page.getByRole('region', { name: '事件调查工作台' });
  const scrubber = investigation.getByLabel('证据时间轴游标');
  await investigation.getByRole('button', { name: '播放事件回放' }).click();
  await page.waitForFunction(id => new URLSearchParams(location.hash.split('?')[1]).get('focusPoint') === id, recoveryEvidence);
  const pause = investigation.getByRole('button', { name: '暂停事件回放' });
  if (await pause.isVisible()) await pause.click();
  await scrubber.press('Home');
  await page.waitForFunction(id => new URLSearchParams(location.hash.split('?')[1]).get('focusPoint') === id, triggerEvidence);
  await investigation.getByLabel('对比目标阶段').selectOption('during');
  const delta = investigation.getByRole('row').filter({ hasText: '连接' });
  await visible(delta, '+6');
  await investigation.getByLabel('对比目标阶段').selectOption('after');
  await visible(delta, '+1');
  await visible(investigation.locator('.investigation-relations'), '当前证据关系');
  const evidenceRoute = route(page);
  await page.locator(`#event-detail a[href="/api/assets/${assets.recovered}/samples/${originSample}"]:visible`).first().click();
  await visible(page.locator('#sample-details'), 'Browser capture at incident start');
  await page.locator('#sample-return').click();
  await page.waitForURL(url => url.hash === evidenceRoute);
  await visible(page.locator('#event-detail'), recovered);
  assert.equal(await investigation.getByLabel('对比目标阶段').inputValue(), 'after');
  assert.equal(parameters(page).get('focusPoint'), triggerEvidence);
  await download(page, investigation.getByRole('button', { name: '下载当前证据复盘' }), /\.md$/);
  if (process.env.DBSPHERE_BROWSER_OUTPUT) await page.screenshot({ path: join(process.env.DBSPHERE_BROWSER_OUTPUT, 'incident-playback.png') });
  proof.push('Played and scrubbed one incident, compared observed phases, loaded selected relationships, reopened evidence and downloaded the restored investigation');
  await page.getByText('返回监控大屏', { exact: true }).click();
  await visible(board('focus'), 'Matrix recovered');
  assert.equal(parameters(page).get('event'), recovered);
  assert(parameters(page).has('cursor') && parameters(page).has('from'));
  await board('watch').getByRole('button', { name: '清除关注' }).click();
  for (const width of [390, 1920, 3840]) {
    await page.setViewportSize({width, height: width === 390 ? 844 : 1080});
    assert(await board('assets').isVisible());
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
  }
  await page.setViewportSize({width: 1440, height: 880});
  if (process.env.DBSPHERE_BROWSER_OUTPUT) await page.screenshot({ path: join(process.env.DBSPHERE_BROWSER_OUTPUT, 'duty-matrix.png') });
  proof.push('Two cluster groups and five status types, scoped counts and filters, multiple pinned instances, host layout, recovered event return, and 390/1920/3840 viewport widths');
  await grafanaReturn(page, assets.recovered, recovered);
}

async function grafanaReturn(page, asset, event) {
  const now = Math.floor(Date.now() / 1000) * 1000 + 123;
  const from = new Date(now - 60 * 60_000).toISOString();
  const end = new Date(now).toISOString();
  const identityURL = origin + '/grafana/api/user';
  const dashURL = url => url.pathname === '/grafana/d/dbsphere-overview';
  const link = (view, event = '') => '/grafana/dbsphere-return?' + new URLSearchParams({ view, asset, event, from, to: end });
  await page.route(identityURL, route => route.fulfill({ json: { login: 'dbsphere-local-admin', isGrafanaAdmin: false } }));
  await page.route(dashURL, route => route.fulfill({ contentType: 'text/html', body: '<!doctype html>' +
    `<a href="${link('live')}">返回 DbSphere 实时观测</a>` +
    `<a href="${link('event')}">空事件返回</a>` +
    `<a href="${link('event', event)}">回到原事件</a>` }));
  try {
    for (const [label, view] of [['返回 DbSphere 实时观测', 'live'], ['空事件返回', 'live'], ['回到原事件', 'incidents']]) {
      await page.goto(origin + '/#grafana?panel=dbsphere-overview');
      await page.frameLocator('#grafana-frame').getByRole('link', { name: label, exact: true }).click();
      await page.waitForFunction(value => document.body.dataset.activeView === value, view);
      assert.equal(parameters(page).get('asset'), asset);
      assert.equal(parameters(page).get('from'), from);
      assert.equal(parameters(page).get(view === 'live' ? 'end' : 'to'), end);
      if (view === 'incidents') {
        assert.equal(parameters(page).get('event'), event);
        await visible(page.locator('#event-detail'), event);
      }
    }
  } finally {
    await page.unroute(identityURL);
    await page.unroute(dashURL);
  }
  proof.push('Same-origin Grafana iframe links return to live, fall back from an empty event, or open the matching event with its time window');
}

async function v3Investigation(page, asset) {
  const request = (tab, path, method = 'GET', payload) => tab.evaluate(async ({path, method, payload}) => {
    const {csrf} = await fetch('/api/session').then(reply => reply.json());
    const response = await fetch(path, {method, headers: {'X-CSRF-Token': csrf, 'Content-Type': 'application/json'},
      body: payload ? JSON.stringify(payload) : undefined});
    return {status: response.status, data: await response.json()};
  }, {path, method, payload});
  const event = randomUUID();
  const evidence = randomUUID();
  runFixtureSQL(`INSERT INTO event_keys(id,asset_id,started_at) VALUES ('${event}','${asset}',now()-interval '6 minutes');
    INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
    VALUES ('${event}','${asset}','pg_lock_wait','V3 锁等待调查','open',now()-interval '6 minutes','latency');
    INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
    SELECT '${evidence}','${event}','alert_firing',collected_at,jsonb_build_object('sampleId',id,'value',42,'note','DO-NOT-SEND')
    FROM samples WHERE asset_id='${asset}' ORDER BY collected_at DESC LIMIT 1;`);
  const calls = [];
  const key = 'dbsphere-v3-model-fixture';
  const model = createServer(async (req, res) => {
    let raw = '';
    for await (const chunk of req) raw += chunk.toString();
    const input = JSON.parse(raw);
    calls.push({input, key: req.headers.authorization, url: req.url});
    const probe = input.messages.length === 1;
    const content = probe ? 'OK' : JSON.stringify({claims: [{kind: 'verification',
      text: '请核对同一时段的 PostgreSQL 阻塞采样。', evidenceIds: [evidence]}], uncertainty: '还需核对阻塞会话。'});
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({choices: [{finish_reason: 'stop', message: {content}}]}));
  });
  await new Promise(resolve => model.listen(0, '127.0.0.1', resolve));
  const team = await request(page, '/api/admin/teams', 'POST', {name: 'v3 incident duty', project: 'QA v3', assetIds: [asset]});
  assert.equal(team.status, 201, JSON.stringify(team.data));
  const teamId = team.data.team.id;
  const accounts = {};
  for (const [name, role, teamIds] of [['v3-owner', 'operator', [teamId]], ['v3-recipient', 'viewer', [teamId]], ['v3-outsider', 'viewer', []]]) {
    const result = await request(page, '/api/admin/accounts', 'POST', {username: name, role, teamIds, password: key});
    assert.equal(result.status, 201, JSON.stringify(result.data));
    accounts[name] = result.data.account.id;
  }
  const owner = await page.context().browser().newPage();
  const recipient = await page.context().browser().newPage();
  const outsider = await page.context().browser().newPage();
  const settings = '/api/admin/ai-model';
  const review = '/api/events/' + event + '/ai-evidence-review';
  const incident = origin + '/#incidents?event=' + event;
  const fetchSettings = async () => (await request(page, settings)).data;
  try {
    await page.goto(incident);
    await visible(page.locator('#event-detail'), event);
    assert.equal((await request(page, review)).data.status, 'no_model');
    await navigate(page, 'settings');
    await selectSection(page, 'settings', 'ai-model');
    const form = page.locator('#ai-model-form');
    await visible(page.locator('#ai-model-status'), '模型未启用');
    await form.locator('[name=endpoint]').fill(`http://127.0.0.1:${model.address().port}/v1/chat/completions`);
    await form.locator('[name=model]').fill('v3-fixture-model');
    await form.locator('[name=apiKey]').fill(key);
    await form.locator('[name=enabled]').check();
    await action(page, form.getByRole('button', {name: '测试连通性'}), 'POST', settings + '/test', 200);
    await visible(page.locator('#ai-model-status'), '连通性测试成功');
    assert.equal(calls[0].input.messages[0].content, 'DbSphere connection test. Reply OK.');
    await action(page, form.getByRole('button', {name: '保存配置'}), 'PUT', settings, 200);
    await visible(page.locator('#ai-model-status'), '模型已启用');
    const configured = await fetchSettings();
    assert.equal(configured.hasKey, true);
    assert(!JSON.stringify(configured).includes(key));
    await page.goto(incident);
    await page.locator('.ai-evidence-panel > summary').click();
    await page.getByText('预览送审证据 · v3-fixture-model', {exact: true}).click();
    await visible(page.locator('.ai-evidence-preview'), evidence);
    assert(!(await page.locator('.ai-evidence-preview').textContent()).includes('DO-NOT-SEND'));
    const outdated = await request(page, review, 'POST', {preview: 'outdated', question: '检查阻塞时长'});
    assert.equal(outdated.status, 409);
    assert.equal(calls.length, 1);
    await page.getByLabel('核查问题（可选）').fill('检查阻塞时长');
    await action(page, page.getByRole('button', {name: '请求模型审核'}), 'POST', review, 200);
    await visible(page.locator('.ai-evidence-panel'), '请核对同一时段的 PostgreSQL 阻塞采样。');
    assert.equal(calls[1].key, 'Bearer ' + key);
    assert(calls[1].input.messages[1].content.includes('检查阻塞时长'));
    assert(!calls[1].input.messages[1].content.includes('DO-NOT-SEND'));
    await page.locator('.ai-evidence-panel').getByRole('button', {name: '证据 ' + evidence.slice(0, 8)}).click();
    await page.locator('#event-evidence-' + evidence).waitFor();
    const disabled = await request(page, settings, 'PUT', {
      revision: configured.revision, endpoint: configured.endpoint, model: configured.model, dailyLimit: configured.dailyLimit,
      reviewSeconds: configured.reviewSeconds, enabled: false, keepKey: true
    });
    assert.equal(disabled.status, 200, JSON.stringify(disabled.data));
    assert.equal((await request(page, review)).data.status, 'no_model');
    await page.reload();
    await page.locator('.ai-evidence-panel > summary').click();
    await visible(page.locator('.ai-evidence-panel'), '规则核查 · 模型未接入');
    assert.equal(await page.getByRole('button', {name: '请求模型审核'}).count(), 0);
    for (const [tab, user] of [[owner, 'v3-owner'], [recipient, 'v3-recipient'], [outsider, 'v3-outsider']]) {
      await tab.goto(origin);
      await login(tab, user, key);
      await tab.locator('[data-nav=wallboard]').waitFor();
    }
    assert.equal((await request(recipient, settings)).status, 403);
    assert.equal((await request(outsider, '/api/duty-views', 'PUT', {name: '非法团队视图', path: 'wallboard?window=15', teamId})).status, 404);
    const board = name => owner.locator(`[data-board="${name}"]`);
    await board('view-name').fill('v3 team shift');
    await board('view-team').selectOption(teamId);
    await board('layout').selectOption('host');
    await board('save-view-button').click();
    await visible(board('view-status'), '已保存 v3 team shift');
    const saved = (await request(owner, '/api/duty-views')).data.views.find(view => view.teamId === teamId);
    assert(saved);
    const teamLink = origin + '/#wallboard?view=' + saved.id;
    await recipient.goto(teamLink);
    await visible(recipient.locator('[data-board=view-status]'), '已打开 v3 team shift');
    assert.equal(await recipient.locator('[data-board=layout]').inputValue(), 'host');
    assert(await recipient.locator('[data-board=delete-view]').isDisabled());
    await outsider.goto(teamLink);
    await visible(outsider.locator('[data-board=view-status]'), '团队视图不可用');
    await page.goto(incident);
    await page.getByRole('button', {name: '核对同窗跨层身份'}).click();
    await visible(page.locator('[data-storage=identity]'), '跨层身份与采集覆盖');
    await page.getByRole('link', {name: '返回事件证据'}).click();
    await visible(page.locator('#event-detail'), event);
    const handoff = '/api/events/' + event + '/handoff';
    assert.equal((await request(page, handoff, 'POST', {accountId: accounts['v3-owner'], note: '核对阻塞链与原始采样'})).status, 200);
    await owner.goto(incident);
    await owner.getByRole('button', {name: '处置', exact: true}).click();
    const assignment = (await request(owner, handoff)).data.assignment;
    await action(owner, owner.getByRole('button', {name: '确认接手当前事件'}), 'POST', handoff + '/accept', 200);
    await visible(owner.locator('#event-detail'), '已接手');
    assert.equal((await request(owner, handoff + '/accept', 'POST', {assignedAt: assignment.assignedAt})).status, 200);
    assert((await request(owner, handoff)).data.assignment.acceptedAt);
    const recovery = randomUUID();
    runFixtureSQL(`UPDATE events SET status='resolved',ended_at=now() WHERE id='${event}';
      INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
      VALUES ('${recovery}','${asset}',now(),'ok','{"connections":5}');
      INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
      VALUES (gen_random_uuid(),'${event}','alert_recovered',now(),jsonb_build_object('sampleId','${recovery}'::text));`);
    await owner.reload();
    await visible(owner.locator('#event-detail'), '已恢复');
    await owner.getByRole('button', {name: '处置', exact: true}).click();
    const responseForm = owner.locator('form').filter({has: owner.getByRole('heading', {name: '处置与恢复验证', exact: true})});
    await responseForm.getByLabel('记录类型').selectOption('recovery_verification');
    assert.equal(await responseForm.getByLabel('成功采样 ID').inputValue(), recovery);
    await responseForm.getByLabel('处理记录（5–500 字）').fill('恢复采样显示连接已恢复正常');
    await action(owner, responseForm.getByRole('button', {name: '保存到事件时间线'}), 'POST', '/api/events/' + event + '/response', 200);
    const report = await owner.evaluate(async eventID => fetch('/api/events/' + eventID + '/report?format=md').then(response => response.text()), event);
    assert(report.includes(event) && report.includes('v3-owner 已确认接手') && report.includes('恢复采样显示连接已恢复正常') && report.includes(recovery));
    proof.push('Configured and disabled a loopback model, verified reviewed evidence, scoped team views, followed cross-layer identity, and verified handoff and recovery in the same incident report');
  } finally {
    await Promise.all([owner.close(), recipient.close(), outsider.close()]);
    await new Promise(resolve => model.close(resolve));
  }
}

async function dm8Monitoring(page) {
  await navigate(page, 'assets');
  await page.locator('#asset-add').click();
  await formField(page, 'asset-form', 'runtime').selectOption('dm8');
  assert.equal(await formField(page, 'asset-form', 'port').inputValue(), '5236');
  await formField(page, 'asset-form', 'name').fill('Browser QA DM8');
  await formField(page, 'asset-form', 'host').fill('127.0.0.1');
  await formField(page, 'asset-form', 'username').fill('DBSPHERE_MON');
  await formField(page, 'asset-form', 'password').fill('dbsphere-dm8-fixture');
  const created = await action(page,page.locator('#asset-submit'),'POST','/api/assets',201);
  const asset = (await created.json()).asset.id;
  const sample = randomUUID(), event = randomUUID();
  runFixtureSQL(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json) VALUES
    ('${sample}','${asset}',now()-interval '15 seconds','ok',
    '{"adapter":{"signals":[{"key":"lockWaits","status":"ok"}]},"dm8":{"version":"DM Database Server x64 V8",
    "build":"qa-fixture","status":"OPEN","mode":"NORMAL","sessions":2,"activeSessions":1,"transactions":1,
    "waits":[{"waitingSession":"101","blockingSession":"102","waitingTransaction":"31","blockingTransaction":"30","waitMs":2500}],
    "signals":[{"key":"lockWaits","source":"V$TRXWAIT / V$TRX","status":"ok"}],
    "files":[{"id":"1:0","tablespace":"MAIN","allocatedBytes":104857600,"freeBytes":52428800}]}}');
    UPDATE assets SET last_status='ok',last_sample_at=(SELECT collected_at FROM samples WHERE id='${sample}'),version_num=80000 WHERE id='${asset}';
    INSERT INTO event_keys(id,asset_id,started_at) SELECT '${event}',asset_id,collected_at FROM samples WHERE id='${sample}';
    INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
    SELECT id,asset_id,'dm8_lock_wait','达梦 QA 事务锁等待','open',started_at,'dm8_lock_wait' FROM event_keys WHERE id='${event}';
    INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
    SELECT gen_random_uuid(),'${event}','dm8_lock_wait',collected_at,
    jsonb_build_object('sampleId',id,'waits',metrics_json#>'{dm8,waits}') FROM samples WHERE id='${sample}';`);
  await page.goto(origin+'/#live?engine=dm8&asset='+asset);
  await visible(page.locator('[data-live=metrics]'), '已观测锁等待');
  await page.locator('[data-live=metrics]').getByRole('button',{name:'已观测锁等待'}).click();
  await page.goto(origin+'/#incidents?event='+event);
  await visible(page.locator('#event-detail'), '达梦会话 102 持锁，阻塞会话 101');
  const raw = page.locator(`#event-detail a[href="/api/assets/${asset}/samples/${sample}"]:visible`).first();
  await raw.click();
  await visible(page.locator('#sample-details'), sample);
  await page.locator('#sample-return').click();
  await visible(page.locator('#event-detail'), event);
  await navigate(page,'assets');
  await page.locator('#asset-runtime').selectOption('dm8');
  const listing=page.locator('#asset-list .list-item').filter({hasText:'Browser QA DM8'});
  await listing.getByRole('button',{name:'查看',exact:true}).click();
  await visible(page.locator('#detail'),'事务锁阻塞链');
  await visible(page.locator('#detail'),'MAIN');
  proof.push('DM8 admission, engine-specific real-time trends, lock evidence, readable source, and same-event return');
}

async function kingbaseMonitoring(page) {
  await navigate(page, 'assets');
  await page.locator('#asset-add').click();
  await formField(page, 'asset-form', 'runtime').selectOption('kingbase');
  assert.equal(await formField(page, 'asset-form', 'port').inputValue(), '54321');
  await formField(page, 'asset-form', 'name').fill('Browser QA Kingbase');
  await formField(page, 'asset-form', 'host').fill('127.0.0.1');
  await formField(page, 'asset-form', 'username').fill('dbsphere_monitor');
  await formField(page, 'asset-form', 'password').fill('dbsphere-kingbase-fixture');
  const created = await action(page,page.locator('#asset-submit'),'POST','/api/assets',201);
  const asset = (await created.json()).asset.id;
  const sample = randomUUID(), event = randomUUID();
  runFixtureSQL(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json) VALUES
    ('${sample}','${asset}',now()-interval '15 seconds','ok',
    '{"adapter":{"engine":"kingbase","role":"primary","transport":{"readOnly":true},"signals":[{"key":"lockWaits","status":"ok"},{"key":"topSql","source":"sys_stat_statements","status":"not_configured","reason":"SQL 统计已关闭"}]},
    "kingbase":{"version":"KingbaseES V009R001C010","mode":"pg","startedAt":"2026-01-01T00:00:00Z","sessions":2,"activeSessions":1,"transactions":2,
    "databaseBytes":104857600,"waits":[{"waitingSession":"201","blockingSession":"202","event":"transactionid"}],"waitCategories":[{"category":"Lock","count":1}],"replication":{"connected":0}}}');
    UPDATE assets SET last_status='ok',last_sample_at=(SELECT collected_at FROM samples WHERE id='${sample}'),version_num=90000 WHERE id='${asset}';
    INSERT INTO event_keys(id,asset_id,started_at) SELECT '${event}',asset_id,collected_at FROM samples WHERE id='${sample}';
    INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
    SELECT id,asset_id,'kingbase_lock_wait','金仓 QA 锁等待','open',started_at,'kingbase_lock_wait' FROM event_keys WHERE id='${event}';
    INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
    SELECT gen_random_uuid(),'${event}','kingbase_lock_wait',collected_at,
    jsonb_build_object('sampleId',id,'waits',metrics_json#>'{kingbase,waits}') FROM samples WHERE id='${sample}';`);
  await page.goto(origin+'/#live?engine=kingbase&asset='+asset);
  await page.locator('[data-live=metrics]').getByRole('button',{name:'阻塞关系',exact:true}).click();
  const sqlPanel = page.locator('[data-live=sql-panel]');
  if (await sqlPanel.getAttribute('open') === null) await sqlPanel.locator('summary').click();
  await page.getByRole('button',{name:'查看金仓 SQL 指纹与锁链',exact:true}).click();
  await visible(page.locator('#detail'),'金仓锁阻塞链');
  await visible(page.locator('#detail'),'SQL 统计已关闭');
  await page.goto(origin+'/#incidents?event='+event);
  await visible(page.locator('#event-detail'),'金仓会话 202 持锁，阻塞会话 201');
  await page.locator(`#event-detail a[href="/api/assets/${asset}/samples/${sample}"]:visible`).first().click();
  await visible(page.locator('#sample-details'),sample);
  await page.locator('#sample-return').click();
  await visible(page.locator('#event-detail'),event);
  proof.push('Kingbase V9 admission, native live trends and lock chain, missing Top SQL coverage, and same-event raw sample');
}

async function redisMonitoring(page) {
  await navigate(page,'assets');
  await page.locator('#asset-add').click();
  await formField(page,'asset-form','runtime').selectOption('redis');
  assert.equal(await formField(page,'asset-form','port').inputValue(),'6379');
  assert.equal(await formField(page,'asset-form','database').inputValue(),'0');
  await formField(page,'asset-form','name').fill('Browser QA Redis');
  await formField(page,'asset-form','host').fill('127.0.0.1');
  await formField(page,'asset-form','username').fill('dbsphere');
  await formField(page,'asset-form','password').fill('dbsphere-redis-fixture');
  const created = await action(page,page.locator('#asset-submit'),'POST','/api/assets',201);
  const asset=(await created.json()).asset.id;
  const sample=randomUUID(), event=randomUUID();
  runFixtureSQL(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json) VALUES
    ('${sample}','${asset}',now()-interval '15 seconds','ok',
    '{"adapter":{"engine":"redis","role":"primary","transport":{"readOnly":true},"signals":[{"key":"redisCounters","status":"ok"}]},
    "redis":{"version":"7.2.17","mode":"standalone","role":"primary","ops":12,"clients":3,"memoryBytes":4178484,"maxMemoryBytes":4194304,
    "latencyMs":2,"slowCommands":[{"id":4,"at":"2026-01-01T00:00:00Z","command":"SET","durationUs":3}]},
    "redisInterval":{"hitPercent":50,"evicted":62,"rejected":0,"seconds":15}}');
    UPDATE assets SET last_status='ok',last_sample_at=(SELECT collected_at FROM samples WHERE id='${sample}'),version_num=70217 WHERE id='${asset}';
    INSERT INTO event_keys(id,asset_id,started_at) SELECT '${event}',asset_id,collected_at FROM samples WHERE id='${sample}';
    INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
    SELECT id,asset_id,'redis_eviction','Redis QA 缓存淘汰','open',started_at,'redis_eviction' FROM event_keys WHERE id='${event}';
    INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
    SELECT gen_random_uuid(),'${event}','redis_eviction',collected_at,jsonb_build_object('sampleId',id,'value',62)
    FROM samples WHERE id='${sample}';`);
  await page.goto(origin+'/#live?engine=redis&asset='+asset);
  await page.locator('[data-live=metrics]').getByRole('button',{name:'淘汰键数',exact:true}).click();
  const sqlPanel=page.locator('[data-live=sql-panel]');
  if (await sqlPanel.getAttribute('open')===null) await sqlPanel.locator('summary').click();
  await page.getByRole('button',{name:'查看 Redis 慢命令和拓扑',exact:true}).click();
  await page.locator('#detail').getByRole('heading',{name:'慢命令',exact:true}).waitFor();
  await visible(page.locator('#detail'),'SET');
  await page.goto(origin+'/#incidents?event='+event);
  await visible(page.locator('#event-detail'),'Redis 在采样间淘汰 62 个键');
  await page.locator(`#event-detail a[href="/api/assets/${asset}/samples/${sample}"]:visible`).first().click();
  await visible(page.locator('#sample-details'),sample);
  await page.locator('#sample-return').click();
  await visible(page.locator('#event-detail'),event);
  proof.push('Redis admission, eviction trend, sanitized slow command, and same-event original sample');
}

(async () => {
  const browser = await chromium.launch();
  const context = await browser.newContext({ viewport: { width: 1440, height: 880 }, hasTouch: true });
  const page = await context.newPage();
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
    await savedDutyView(page, asset);
    await monitoring(page, asset);
    await storageInvestigation(page);
    await operations(page, asset);
    await pagination(page, asset);
    await dutyMatrix(page);
    await v3Investigation(page, asset);
    await dm8Monitoring(page);
    await kingbaseMonitoring(page);
    await redisMonitoring(page);
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
