const runAcceptance = async (page) => {
  const root = 'http://127.0.0.1:18080';
  const base = '/api/log-analysis/';
  const results = [];
  const loc = name => page.locator('[data-logs="' + name + '"]');
  const assert = (condition, message) => { if (!condition) throw new Error(message); };
  const visible = (scope, text) => scope.getByText(text, { exact: false }).first().waitFor({ state: 'visible' });
  const dialogButton = (scope, name) => scope.getByRole('button', { name, exact: true });
  page.setDefaultTimeout(12000);

  await page.locator('#login-form [name=username]').fill('admin');
  await page.locator('#login-form [name=password]').fill('dbsphere-browser-admin-fixture');
  await page.locator('#login-form').getByRole('button', { name: '登录', exact: true }).click();
  await page.locator('[data-nav=logs]').waitFor({ state: 'visible' });
  const session = await (await page.request.get(root + '/api/session')).json();
  assert(session.csrf, 'Browser login did not produce a session');
  results.push('authenticated browser');

  async function api(path, method = 'GET', data) {
    const binary = Buffer.isBuffer(data);
    const response = await page.request.fetch(root + path, {
      method, data, headers: { 'X-CSRF-Token': session.csrf, ...(binary ? { 'Content-Type': 'application/octet-stream' } : {}) }
    });
    assert(response.ok(), method + ' ' + path + ' HTTP ' + response.status() + ' ' + await response.text());
    return response.json();
  }

  const org = (await api('/api/admin/organizations')).organizations[0];
  const asset = await api('/api/assets', 'POST', {
    organizationId: org.id, name: 'Browser PostgreSQL 18', host: '127.0.0.1',
    port: 15434, database: 'postgres', username: 'postgres', password: 'dbsphere-browser-fixture',
    sslMode: 'disable', runtime: 'managed'
  });
  const project = await api(base + 'projects', 'POST', { organizationId: org.id, name: 'Browser acceptance' });
  const instance = await api(base + 'instances', 'POST', {
    projectId: project.id, assetId: asset.id, environment: 'Acceptance', name: 'PostgreSQL 18',
    engine: 'postgres', version: '18'
  });
  const createBatch = name => api(base + 'batches', 'POST', {
    instanceId: instance.id, name, coverage: 'full', coverageNote: 'Browser acceptance controlled generator'
  });
  const before = await createBatch('Baseline');
  const after = await createBatch('Retest');
  const now = Date.now() - 600000;
  const line = (at, duration, table) => new Date(at).toISOString().replace('T', ' ').replace('Z', ' UTC') +
    ' [123] app=orders-api LOG: duration: ' + duration + ' ms statement: select id from ' + table +
    " where code='dbsphere-fixture-literal'";
  function fixture(start, duration, includeInventory) {
    const lines = Array.from({length: 25}, (_, index) => line(start + index * 5000, duration + index, 'qa_orders'));
    if (includeInventory) lines.push(line(start + 2000, 2600, 'qa_inventory'));
    return Buffer.from(lines.join('\n') + '\n');
  }
  await page.goto(root + '/#logs?batch=' + before.id);
  await loc('report').waitFor({ state: 'visible' });
  async function upload(name, data) {
    await loc('upload').click();
    await loc('upload-dialog').locator('input[type=file]').setInputFiles({ name, mimeType: 'text/plain', buffer: data });
    await loc('upload-dialog').locator('select[name=format]').selectOption('text');
    const submitted = page.waitForResponse(response => response.url().endsWith('/queue') && response.request().method() === 'POST');
    await loc('submit-upload').click();
    const queued = await submitted;
    assert(queued.status() === 202, 'Log ingestion queue returned HTTP ' + queued.status());
    await loc('upload-dialog').waitFor({ state: 'hidden' });
  }
  await upload('baseline.log', fixture(now - 86400000, 1900, false));
  await page.goto(root + '/#logs?batch=' + after.id);
  await visible(loc('report'), '全景分析');
  await upload('retest.log', fixture(now, 1200, true));

  async function waitRecords(batch, count) {
    for (let attempt = 0; attempt < 40; attempt++) {
      const { records } = await api(base + 'batches/' + batch.id + '/records');
      if (records.length === count) return records;
      await new Promise(resolve => setTimeout(resolve, 400));
    }
    throw new Error('Did not parse ' + count + ' records from ' + batch.name);
  }
  const baselineRecords = await waitRecords(before, 25);
  const currentRecords = await waitRecords(after, 26);
  results.push('browser file picker, PostgreSQL 18 storage, and async parsing');

  const pending = await api(base + 'batches/' + after.id + '/files', 'POST', {
    name: 'pending-acceptance.log', size: 1048577, format: 'text'
  });
  await api(base + 'files/' + pending.id + '/blocks?part=0', 'PUT', Buffer.alloc(1048576, 65));
  await loc('open-tasks').click();
  page.once('dialog', confirmation => confirmation.accept());
  const pendingRow = loc('tasks').getByRole('row').filter({ hasText: 'pending-acceptance.log' });
  await dialogButton(pendingRow, '取消上传').click();
  await visible(loc('tasks-dialog'), '上传已取消');
  const missing = await page.request.get(root + base + 'files/' + pending.id + '/status');
  assert(missing.status() === 404, 'Cancelled upload remains available HTTP ' + missing.status());
  await page.keyboard.press('Escape');
  await loc('tasks-dialog').waitFor({ state: 'hidden' });
  results.push('v2.20 cancelled a real upload and removed its stored block');

  await loc('refresh').click();
  const sqlRow = loc('report').getByRole('row').filter({ hasText: 'qa_orders' }).filter({
    has: page.getByRole('button', { name: '分析 SQL', exact: true })
  });
  await dialogButton(sqlRow.first(), '分析 SQL').click();
  await visible(loc('report'), 'SQL 延迟分布');
  await visible(loc('report'), '代表 SQL 记录');
  assert(!(await loc('report').innerText()).includes('dbsphere-fixture-literal'), 'Unmasked literal in SQL report');
  results.push('v2.22 SQL latency distribution and representative records');

  const currentSource = currentRecords.find(row => row.sql.includes('qa_orders'));
  const inventory = currentRecords.find(row => row.sql.includes('qa_inventory'));
  assert(currentSource && inventory, 'Unable to locate both SQL families');
  const investigation = await api(base + 'records/' + currentSource.id + '/investigate', 'POST', {});
  assert(investigation.eventId, 'Monitored log did not create a traceable event');
  await api(base + 'investigations/' + investigation.id + '/records', 'POST', {
    recordId: baselineRecords[0].id
  });
  await page.goto(root + '/#logs?batch=' + after.id + '&case=' + investigation.id);
  const study = loc('investigation-dialog');
  await study.getByLabel('基线批次', { exact: true }).selectOption(before.id);
  await study.getByLabel('复测批次', { exact: true }).selectOption(after.id);
  await study.getByLabel('目标 P95（毫秒）').fill('1500');
  await dialogButton(study, '保存复测结果').click();
  await visible(study, '已达标');
  const [download] = await Promise.all([
    page.waitForEvent('download'),
    dialogButton(study, '下载复测报告').click()
  ]);
  assert(download.suggestedFilename().startsWith('dbsphere-retest-'), 'Unexpected retest report filename');
  const stream = await download.createReadStream();
  const chunks = [];
  for await (const chunk of stream) chunks.push(chunk);
  const report = Buffer.concat(chunks).toString('utf8');
  assert(report.includes(investigation.eventId) && report.includes(currentSource.id) &&
    report.includes(baselineRecords[0].id) && !report.includes('dbsphere-fixture-literal'),
    'Retest report lost the event/evidence trail or exposed a source literal');
  results.push('v2.23 saved a passing retest and verified downloaded report evidence');

  await study.getByRole('link', { name: '打开事件及 AI 证据问答' }).click();
  const event = page.locator('#event-detail');
  await visible(event, investigation.eventId);
  const matchPanel = event.locator('.event-log-search');
  if (!(await matchPanel.evaluate(element => element.open))) await visible(matchPanel, '查找同窗日志');
  await matchPanel.locator('summary').click();
  const match = matchPanel.locator('.evidence').filter({ hasText: 'qa_inventory' });
  await dialogButton(match, '采纳到当前事件').click();
  if (!(await matchPanel.evaluate(element => element.open))) await matchPanel.locator('summary').click();
  await visible(match, '已采纳');
  assert(page.url().includes(investigation.eventId), 'Adoption moved the user off the original incident');
  await match.getByRole('link', { name: '查看原文' }).click();
  await visible(loc('raw-dialog'), '原始日志上下文');
  const raw = await loc('raw-text').innerText();
  assert(raw.includes('qa_inventory') && raw.includes('dbsphere-fixture-literal'), 'Wrong raw evidence returned');
  await page.keyboard.press('Escape');
  await loc('return-incident').click();
  await visible(event, investigation.eventId);
  assert(page.url().includes(investigation.eventId), 'Raw evidence did not lead back to the same incident');
  results.push('v2.21 adopted same-window evidence, opened original text, and returned to the same event');

  await page.goto(root + '/#logs?batch=' + after.id);
  await loc('open-archive').click();
  const archive = loc('archive-dialog');
  const days = archive.getByRole('spinbutton', { name: org.name + '原文保留天数' });
  const row = archive.getByRole('row').filter({ has: days });
  await days.fill('7');
  const save = dialogButton(row, '保存');
  assert(await save.isDisabled(), 'Archive policy can be saved before preview');
  await dialogButton(row, '预览影响').click();
  await visible(row, '调查保护');
  page.once('dialog', confirmation => confirmation.accept());
  await save.click();
  await visible(archive, '保留期限已保存');
  assert(await days.inputValue() === '7', 'Archive retention policy was not persisted');
  await dialogButton(row, org.name).click();
  const files = loc('archive-files-dialog');
  await visible(files, '调查保护');
  results.push('v2.24 previewed and saved retention, and listed investigation-protected originals');

  return { result: 'V224_BROWSER_PASS', event: investigation.eventId, adoptedRecord: inventory.id, checks: results };
};

(async () => {
  const { chromium } = require('playwright');
  const browser = await chromium.launch();
  try {
    const page = await browser.newPage();
    await page.goto('http://127.0.0.1:18080');
    const results = await runAcceptance(page);
    console.log(JSON.stringify(results, null, 2));
  } finally {
    await browser.close();
  }
})().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
