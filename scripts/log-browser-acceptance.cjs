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
  await page.locator('#login-form [name=password]').fill(process.env.DBSPHERE_ADMIN_PASSWORD);
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

  const org = (await api('/api/admin/organizations')).organizations.find(item => item.id === '00000000-0000-4000-8000-000000000001');
  assert(org, 'Missing default acceptance organization');
  const scenario = Date.now().toString(36);
  const { asset } = await api('/api/assets', 'POST', {
    organizationId: org.id, name: 'Browser PostgreSQL 18 ' + scenario, host: '127.0.0.1',
    port: 15434, database: 'postgres', username: 'postgres', password: 'dbsphere-browser-fixture',
    sslMode: 'disable', runtime: 'managed'
  });
  assert(asset?.id, 'Asset creation did not return an asset ID');
  await page.goto(root + '/#logs');
  async function create(kind, fields) {
    await loc('new-' + kind).click();
    const dialog = loc(kind + '-dialog');
    await dialog.waitFor();
    for (const [name, value] of Object.entries(fields)) {
      const field = dialog.locator('[name="' + name + '"]');
      if (await field.evaluate(element => element.tagName === 'SELECT')) await field.selectOption(value);
      else await field.fill(value);
    }
    const path = kind === 'batch' ? 'batches' : kind + 's';
    const pending = page.waitForResponse(response => response.url() === root + base + path && response.request().method() === 'POST');
    await dialog.locator('button[type=submit]').click();
    const response = await pending;
    assert(response.status() === 201, 'Create ' + kind + ' failed ' + await response.text());
    await dialog.waitFor({ state: 'hidden' });
    return response.json();
  }
  await create('project', { organizationId: org.id, name: 'Browser acceptance ' + scenario });
  await create('instance', { assetId: asset.id, environment: 'Acceptance', name: 'PostgreSQL 18', engine: 'postgres', version: '18' });
  const createBatch = name => create('batch', { name, coverage: 'full', coverageNote: 'Browser acceptance controlled generator' });
  const before = await createBatch('Baseline');
  const after = await createBatch('Retest');
  results.push('browser forms saved the project, instance and both batches');
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
  await waitRecords(before, 25);
  const currentRecords = await waitRecords(after, 26);
  results.push('browser file picker, PostgreSQL 18 storage, and async parsing');

  await loc('open-comparison').click();
  await loc('baseline').selectOption(before.id);
  await visible(loc('compare-rows'), 'qa_orders');
  const comparisonFilter = loc('compare-filter');
  for (const state of (await comparisonFilter.locator('option').evaluateAll(options => options.map(option => option.value)))) {
    await comparisonFilter.selectOption(state);
    assert(await comparisonFilter.inputValue() === state, 'Comparison state did not change');
  }
  await comparisonFilter.selectOption('');
  const rebuilt = page.waitForResponse(response => response.request().method() === 'POST' && response.url().endsWith('/batches/' + after.id + '/rebuild'));
  await loc('rebuild').click();
  assert((await rebuilt).ok(), 'Comparison index rebuild failed');
  await loc('open-comparison').click();
  await loc('comparison').waitFor({ state: 'hidden' });
  await loc('open-health').click();
  await visible(loc('health-dialog'), '项目健康');
  await loc('health-dialog').getByRole('button', { name: '关闭' }).click();
  await loc('health-dialog').waitFor({ state: 'hidden' });
  results.push('browser compared the two complete log batches');

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
  await loc('task-state').selectOption('uploading');
  await loc('task-refresh').click();
  assert(!(await loc('tasks').innerText()).includes('pending-acceptance.log'), 'Cancelled upload is still listed');
  await loc('tasks-dialog').getByRole('button', { name: '关闭' }).click();
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

  const inventory = currentRecords.find(row => row.sql.includes('qa_inventory'));
  assert(inventory, 'Unable to locate the secondary SQL family');
  await dialogButton(loc('report'), '清除下钻').click();
  const recordFilters = loc('record-filters');
  await recordFilters.locator('[name=kind]').selectOption('statement');
  await dialogButton(recordFilters, '筛选').click();
  await visible(loc('records'), 'qa_orders');
  const ordersRecord = loc('records').getByRole('row').filter({ hasText: 'qa_orders' }).first();
  await dialogButton(ordersRecord, '查看原文').click();
  await visible(loc('raw-dialog'), 'qa_orders');
  await loc('raw-dialog').getByRole('button', { name: '关闭' }).click();
  await dialogButton(ordersRecord, '调查').click();
  const study = loc('investigation-dialog');
  await visible(study, '根因调查');
  await dialogButton(study, '关闭').click();
  await loc('open-comparison').click();
  await loc('baseline').selectOption(before.id);
  const comparedOrders = loc('compare-rows').getByRole('row').filter({ hasText: 'qa_orders' }).first();
  const linked = page.waitForResponse(response => /\/investigations\/.+\/records$/.test(response.url()) && response.request().method() === 'POST');
  await dialogButton(comparedOrders, '调查').click();
  const response = await linked;
  assert(response.ok(), 'Comparison failed to link the baseline ' + await response.text());
  const investigation = await response.json();
  assert(investigation.eventId, 'Monitored log did not create a traceable event');
  const currentSource = investigation.records.find(row => row.batch === after.id);
  const baselineSource = investigation.records.find(row => row.batch === before.id);
  assert(currentSource && baselineSource, 'Comparison investigation lost a batch source');
  await study.locator('select[name=before]').selectOption(before.id);
  await study.locator('select[name=after]').selectOption(after.id);
  await study.locator('input[name=target]').fill('1500');
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
    report.includes(baselineSource.id) && !report.includes('dbsphere-fixture-literal'),
    'Retest report lost the event/evidence trail or exposed a source literal');
  results.push('v2.23 saved a passing retest and verified downloaded report evidence');

  await study.getByRole('link', { name: '打开事件及 AI 证据问答' }).click();
  const event = page.locator('#event-detail');
  await visible(event, investigation.eventId);
  for (const label of ['处置', '时间线与报告', '根因与证据']) {
    const tab = event.locator('.incident-nav').getByRole('button', { name: label, exact: true });
    await tab.click();
    assert(await tab.getAttribute('aria-pressed') === 'true', 'Event tab did not select ' + label);
  }
  const openTab = label => dialogButton(event.locator('.incident-nav'), label).click();
  await openTab('处置');
  await dialogButton(event, '确认事件').click();
  await visible(event, '确认。');
  const note = 'Browser QA incident evidence review';
  await event.getByLabel('处理记录（5–500 字）').fill(note);
  await dialogButton(event, '保存到事件时间线').click();
  await visible(event.locator('.incident-content:visible'), note);
  const { team } = await api('/api/admin/teams', 'POST', {
    organizationId: org.id, project: 'Log browser fixture', name: 'Handoff ' + scenario, assetIds: [asset.id]
  });
  const { account } = await api('/api/admin/accounts', 'POST', {
    organizationId: org.id, username: 'handoff-' + scenario, password: 'dbsphere-handoff-fixture',
    role: 'operator', teamIds: [team.id], enabled: true
  });
  await page.reload();
  await openTab('处置');
  await event.getByLabel('接班人').selectOption(account.id);
  await event.getByLabel('交接记录').fill('Browser QA handed off the incident');
  await dialogButton(event, '交接当前事件').click();
  await visible(event.locator('.incident-content:visible'), '交接给 ' + account.username);
  await openTab('时间线与报告');
  const timeline = event.locator('.event-timeline');
  if (!(await timeline.evaluate(panel => panel.open))) await timeline.locator('summary').click();
  await visible(timeline, note);
  for (const [label, extension] of [['Markdown', '.md'], ['HTML', '.html']]) {
    const [reportFile] = await Promise.all([page.waitForEvent('download'), dialogButton(event, '下载事件报告 · ' + label).click()]);
    assert(reportFile.suggestedFilename().endsWith(extension) && await reportFile.failure() === null, 'Event report download failed');
  }
  const sharing = event.locator('details').filter({ has: page.getByText('事件分享', { exact: true }) });
  await sharing.locator('summary').click();
  await dialogButton(sharing, '创建事件链接').click();
  const shared = sharing.getByRole('link', { name: '打开分享链接' });
  await shared.waitFor();
  const sharedPage = await page.context().newPage();
  try {
    await sharedPage.goto(root + await shared.getAttribute('href'));
    await visible(sharedPage.locator('#event-detail'), investigation.eventId);
  } finally { await sharedPage.close(); }
  await dialogButton(sharing, '撤销链接').click();
  await visible(sharing, '当前没有有效的分享链接');
  if (process.env.DBSPHERE_APPROVER_PASSWORD) {
    await openTab('处置');
    await event.getByLabel('申请原因（5–300 字）').fill('Browser QA requests a read-only sample');
    await dialogButton(event, '预检与预演').click();
    const submit = dialogButton(event, '提交作业审批');
    await submit.click();
    const submittedOrder = event.locator('.incident-content:visible').getByText('待审批', { exact: false });
    await submittedOrder.first().waitFor();
    const approver = await page.context().browser().newPage();
    try {
      await approver.goto(root + '/#incidents?event=' + investigation.eventId);
      await approver.locator('#login-form [name=username]').fill('approver');
      await approver.locator('#login-form [name=password]').fill(process.env.DBSPHERE_APPROVER_PASSWORD);
      await approver.locator('#login-form').getByRole('button', { name: '登录', exact: true }).click();
      const review = approver.locator('#event-detail');
      await dialogButton(review.locator('.incident-nav'), '处置').click();
      await dialogButton(review, '批准').click();
      await visible(review.locator('.incident-content:visible'), '已批准待执行');
    } finally { await approver.close(); }
    await page.reload();
    await openTab('处置');
    await event.getByRole('button', { name: /^执行已审批的/ }).click();
    await visible(event.locator('.incident-content:visible'), '已完成');
    results.push('Read-only sample order preview, separate approval account, authorized execution and result');
  }
  await openTab('根因与证据');
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
  await page.goto(root + '/#alerts?section=rca');
  const rulePanel = page.locator('#rca-workbench');
  await rulePanel.getByLabel('回放事件 ID').fill(investigation.eventId);
  await dialogButton(rulePanel, '回放规则').click();
  await visible(rulePanel, '事件开始时规则');
  await visible(rulePanel, '当前规则');
  await rulePanel.getByRole('link', { name: '返回原事件' }).click();
  await visible(event, investigation.eventId);
  results.push('Incident acknowledgement, response, handoff, both report downloads, scoped share creation and revocation, and historical rule replay');

  await page.goto(root + '/#logs?batch=' + after.id);
  await loc('open-archive').click();
  const archive = loc('archive-dialog');
  const days = archive.getByRole('spinbutton', { name: org.name + '原文保留天数' });
  const row = archive.getByRole('row').filter({ has: page.getByRole('spinbutton', { name: org.name + '原文保留天数' }) });
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
  await visible(files.getByRole('table'), '调查保护');
  await loc('archive-file-state').selectOption('retained');
  const verify = files.getByRole('button', { name: '校验归档', exact: true }).first();
  await verify.click();
  await visible(files, '校验通过');
  await files.getByRole('button', { name: '关闭', exact: true }).click();
  await archive.getByRole('button', { name: '关闭', exact: true }).click();
  results.push('v2.24 previewed and saved retention, and listed investigation-protected originals');

  const firstProject = await loc('project').inputValue();
  const otherProject = await api(base + 'projects', 'POST', { organizationId: org.id, name: 'QA project ' + '长项目名称'.repeat(7) });
  const stage = await api(base + 'instances', 'POST', { projectId: otherProject.id, environment: 'Stage', name: 'Stage PG', engine: 'postgres', version: '18' });
  const prod = await api(base + 'instances', 'POST', { projectId: otherProject.id, environment: 'Production', name: 'Production ' + '长实例名称'.repeat(7), engine: 'postgres', version: '18' });
  const isolatedBatch = await api(base + 'batches', 'POST', { instanceId: prod.id, name: 'Production baseline', coverage: 'slow' });
  await loc('refresh').click();
  await loc('project').selectOption(otherProject.id);
  await loc('environment').selectOption('Stage');
  await visible(loc('status'), '新建批次后可以上传');
  assert(await loc('instance').inputValue() === stage.id && await loc('batch').isDisabled(), 'Empty stage scope did not reset batch');
  await loc('environment').selectOption('Production');
  await visible(loc('status'), 'Production');
  assert(await loc('instance').inputValue() === prod.id && await loc('batch').inputValue() === isolatedBatch.id, 'Production did not load the matching batch');
  await loc('environment').selectOption('');
  await loc('instance').selectOption(stage.id);
  await visible(loc('status'), '新建批次后可以上传');
  await page.reload();
  await visible(loc('status'), '新建批次后可以上传');
  assert(await loc('instance').inputValue() === stage.id && await loc('environment').inputValue() === '', 'Refresh lost valid empty-batch scope');
  await loc('instance').selectOption(prod.id);
  await visible(loc('status'), 'Production');
  for (const theme of ['light', 'dark']) {
    await page.locator('#theme-select').selectOption(theme);
    await page.setViewportSize({ width: 390, height: 850 });
    const directory = page.locator('.log-directory');
    assert(await directory.evaluate(node => [...node.querySelectorAll('select')].every(select =>
      select.getBoundingClientRect().right <= node.getBoundingClientRect().right + 1)), 'Log dropdowns overflow in ' + theme);
  }
  await page.setViewportSize({ width: 1440, height: 880 });
  await loc('project').selectOption(firstProject);
  await loc('batch').selectOption(after.id);
  await visible(loc('status'), 'Acceptance');
  results.push('Log scope selectors keep their project, environment, instance and batch consistent across switches, empty states, refresh and narrow layouts in both themes');

  if (process.env.DBSPHERE_APPROVER_PASSWORD) {
    const { organization } = await api('/api/admin/organizations', 'POST', { name: 'QA without approver ' + scenario });
    const isolatedName = 'Isolated work order fixture ' + scenario;
    const { asset: isolated } = await api('/api/assets', 'POST', {
      organizationId: organization.id, name: isolatedName, host: '127.0.0.1', port: 15435, database: 'postgres',
      username: 'postgres', password: 'dbsphere-browser-fixture', sslMode: 'disable', runtime: 'managed'
    });
    const unavailable = await api('/api/assets/' + isolated.id + '/check', 'POST');
    assert(unavailable.result.status === 'error', 'Expected the isolated endpoint to be unavailable');
    const { events } = await api('/api/events/page?' + new URLSearchParams({ q: isolatedName }));
    assert(events.length === 1, 'Expected one incident for the isolated asset');
    await page.goto(root + '/#incidents?event=' + events[0].id);
    await openTab('处置');
    await visible(event, '为当前资产配置独立审批账号');
    assert(await dialogButton(event, '提交作业审批').isHidden(), 'A platform approver was incorrectly offered for a different organization');
    results.push('Approval submission is hidden when no approver covers the event asset');
  }

  return { result: 'LOG_BROWSER_PASS', event: investigation.eventId, adoptedRecord: inventory.id, checks: results };
};

(async () => {
  const { chromium } = require('playwright');
  const browser = await chromium.launch();
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    await page.goto('http://127.0.0.1:18080');
    const results = await runAcceptance(page).catch(async error => {
      console.error(JSON.stringify(await page.evaluate(() => ({
        view: document.body.dataset.activeView,
        hash: location.hash,
        status: document.querySelector('[data-logs="status"]')?.textContent,
        dialogs: [...document.querySelectorAll('dialog[open]')].map(dialog => ({
          heading: dialog.querySelector('h2')?.textContent,
          fields: [...dialog.querySelectorAll('select')].map(select => select.name),
          message: dialog.querySelector('[role=status]')?.textContent
        }))
      }))));
      throw error;
    });
    console.log(JSON.stringify(results, null, 2));
  } finally {
    await browser.close();
  }
})().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
