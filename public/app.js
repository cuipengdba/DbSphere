let csrf = null;
let currentRole = null;
let viewerScoped = false;
let selectedAsset = null;
let selectedEvent = null;
let editingAsset = null;
let pg18Store = false;
const $ = (id) => document.getElementById(id);
const formatTime = (value) => value ? new Date(value).toLocaleString('zh-CN') : '尚无数据';
const growthMetric = 'pg.database_growth_bytes_per_day';
const formatAlertValue = (metric, value) => value == null ? '信号缺失' :
  metric === growthMetric ? `${(Number(value) / (1024 ** 3)).toFixed(2)} GiB/日` : String(value);
const views = {
  overview: ['总览', '先看异常，再进入资产和证据。'],
  incidents: ['问题事件', '一个问题，一个事件 ID，一条可追溯的证据链。'],
  assets: ['资产与实例', '纳管 PostgreSQL 14–18，并查看实例与基础设施状态。'],
  alerts: ['告警与通知', '处理当前告警，管理规则、静默和投递。'],
  inspections: ['巡检与报告', '运行定期检查，查看历史结果和差异。'],
  settings: ['系统状态', '检查监控数据存储与部署状态。']
};

function setView(view, updateHash = true) {
  const selected = views[view] ? view : 'overview';
  for (const section of document.querySelectorAll('[data-view]'))
    section.hidden = section.dataset.view !== selected || (section.id === 'admin-audit-panel' && currentRole !== 'admin');
  for (const button of document.querySelectorAll('[data-nav]')) {
    const active = button.dataset.nav === selected;
    button.classList.toggle('active', active);
    if (active) button.setAttribute('aria-current', 'page');
    else button.removeAttribute('aria-current');
  }
  $('view-title').textContent = views[selected][0];
  $('view-description').textContent = views[selected][1];
  if (updateHash && location.hash !== `#${selected}`) location.hash = selected;
  window.scrollTo({ top: 0, behavior: 'instant' });
}

function renderOverview(assets, events) {
  const focus = $('overview-events'); focus.replaceChildren();
  const open = events.filter(event => event.status === 'open').slice(0, 5);
  if (!open.length) focus.append(cell('p', '当前没有待处理事件。', 'empty'));
  for (const event of open) {
    const button = cell('button', `${event.assetName} · ${event.title} · ${formatTime(event.startedAt)}`, 'event-button event-open');
    button.addEventListener('click', () => showEvent(event.id)); focus.append(button);
  }
  const fleet = $('overview-assets'); fleet.replaceChildren();
  if (!assets.length) fleet.append(cell('p', '尚未纳管 PostgreSQL 实例。', 'empty'));
  for (const asset of assets.slice(0, 6)) {
    const button = cell('button', `${asset.name} · ${asset.runtime === 'k8s' ? 'K8S' : 'Linux'} · ${asset.lastStatus === 'ok' ? '正常' : asset.lastStatus === 'error' ? '采集失败' : '待采集'}`, `event-button ${asset.lastStatus === 'ok' ? 'health-ok' : asset.lastStatus === 'error' ? 'health-failed' : ''}`);
    button.addEventListener('click', () => { setView('assets'); void showAsset(asset.id); });
    fleet.append(button);
  }
}

async function api(path, method = 'GET', payload) {
  if (currentRole === 'viewer' && method !== 'GET' && path !== '/api/logout')
    throw new Error('只读账号不能修改监控数据。');
  const response = await fetch(path, {
    method, credentials: 'same-origin',
    headers: { ...(payload ? { 'Content-Type': 'application/json' } : {}), ...(method !== 'GET' && csrf ? { 'X-CSRF-Token': csrf } : {}) },
    body: payload ? JSON.stringify(payload) : undefined
  });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}

function notice(message, error = false) {
  $('notice').textContent = message;
  $('notice').className = error ? 'notice error' : 'notice';
}

function cell(tag, text, cls) {
  const el = document.createElement(tag);
  el.textContent = text;
  if (cls) el.className = cls;
  return el;
}

async function refresh() {
  const [{ assets }, { events }, { alerts }, { rules }, { silences }, { notifications }, { storage },
    { templates }, { schedules }, { runs }, audit] = await Promise.all([
    api('/api/assets'), api('/api/events'), api('/api/alerts'), api('/api/alert-rules'),
    api('/api/silences'), api('/api/notifications'), viewerScoped ? Promise.resolve({ storage: { mode: 'scoped' } }) : api('/api/storage'),
    api('/api/inspection-templates'), api('/api/inspection-schedules'), api('/api/inspection-runs'),
    currentRole === 'admin' ? api('/api/admin/audit').catch(() => null) : Promise.resolve(null)
  ]);
  $('asset-count').textContent = assets.length;
  $('healthy-count').textContent = assets.filter(a => a.lastStatus === 'ok').length;
  $('event-count').textContent = events.filter(e => e.status === 'open').length;
  $('event-count').closest('.stat').classList.toggle('alerting', events.some(e => e.status === 'open'));
  renderOverview(assets, events);
  renderAssets(assets);
  renderEvents(events);
  renderAlerts(alerts);
  renderSilenceOptions(assets, rules);
  renderSilences(silences, assets, rules);
  renderNotifications(notifications);
  pg18Store = storage.mode !== 'sqlite-preview';
  renderStorage(storage);
  $('admin-audit-panel').hidden = currentRole !== 'admin' || location.hash.slice(1) !== 'settings';
  if (currentRole === 'admin') renderAdminAudit(audit?.entries);
  renderInspections(assets, templates, schedules, runs);
  if (!document.activeElement || !$('rule-list').contains(document.activeElement)) renderRules(rules);
  if (selectedAsset) await showAsset(selectedAsset);
}

function renderAdminAudit(entries, target = $('admin-audit-list')) {
  target.replaceChildren();
  if (!entries) { target.append(cell('p', '操作记录暂时不可用。', 'empty')); return; }
  if (!entries.length) { target.append(cell('p', '尚无管理员写入操作。', 'empty')); return; }
  for (const item of entries) target.append(cell('p',
    `${formatTime(item.observedAt)} · ${item.actor} · ${item.method} ${item.path} · ${item.statusCode || '进行中'} · ${item.outcome}`, 'evidence'));
}

function k8sStateText(payload) {
  if (payload.status !== 'ok') return payload.reason || payload.status;
  const pod = payload.pod || {};
  const ready = (pod.conditions || []).find(condition => condition.type === 'Ready');
  const readiness = ready ? `Ready=${ready.status}${ready.reason ? ` (${ready.reason})` : ''}` : 'Ready=未知';
  const containers = (payload.containers || []).map(container =>
    `${container.name}: ${container.ready === false ? '未就绪' : container.ready === true ? '就绪' : '状态未知'}` +
    `${container.terminatedReason ? `（上次终止 ${container.terminatedReason}）` : ''}`).join('，');
  const unhealthy = (payload.events || []).filter(event => event.reason === 'Unhealthy');
  return `Pod ${pod.name || '未知'} / ${pod.phase || '未知'} / ${readiness}；${containers || '无容器状态'}` +
    `${unhealthy.length ? `；近期探针失败事件 ${unhealthy.length} 条` : ''}`;
}

function renderInspections(assets, templates, schedules, runs) {
  const assetSelect = $('inspection-asset');
  const templateSelect = $('inspection-template');
  const selectedAssetId = assetSelect.value;
  const selectedTemplateId = templateSelect.value;
  assetSelect.replaceChildren(...assets.map(asset => {
    const option = document.createElement('option'); option.value = asset.id; option.textContent = asset.name; return option;
  }));
  assetSelect.value = assets.some(asset => asset.id === selectedAssetId) ? selectedAssetId : assets[0]?.id ?? '';
  const runtime = assets.find(asset => asset.id === assetSelect.value)?.runtime;
  const available = templates.filter(template => template.scope === 'all' || template.scope === runtime);
  templateSelect.replaceChildren(...available.map(template => {
    const option = document.createElement('option'); option.value = template.id;
    option.textContent = `${template.title} · v${template.version}`; return option;
  }));
  templateSelect.value = available.some(template => template.id === selectedTemplateId)
    ? selectedTemplateId : available[0]?.id ?? '';
  const scheduleList = $('inspection-schedules'); scheduleList.replaceChildren();
  if (!schedules.length) scheduleList.append(cell('p', '尚未启用定时巡检。', 'empty'));
  for (const schedule of schedules) {
    const asset = assets.find(item => item.id === schedule.assetId);
    const template = templates.find(item => item.id === schedule.templateId);
    scheduleList.append(cell('p', `${asset?.name ?? '已删除资产'} · ${template?.title ?? schedule.templateId} · ${schedule.enabled ? '启用' : '停用'} · ${schedule.intervalMinutes} 分钟 · 下次 ${formatTime(schedule.nextDueAt)}`, 'evidence'));
  }
  const templateList = $('inspection-templates');
  if (!document.activeElement || !templateList.contains(document.activeElement)) {
    templateList.replaceChildren();
    for (const template of templates) {
      const details = document.createElement('details'); details.className = 'rule-card';
      details.append(cell('summary', `${template.title} · v${template.version} · ${template.checks.length} 项`));
      const form = document.createElement('form'); form.className = 'rule-form';
      const thresholds = new Map();
      for (const check of template.checks) {
        const input = document.createElement('input'); input.type = 'number'; input.min = '0';
        input.step = 'any'; input.value = check.threshold; input.required = true;
        const label = document.createElement('label'); label.append(cell('span', check.title), input);
        thresholds.set(check.id, input); form.append(label);
      }
      const save = cell('button', '保存模板阈值', 'small-button'); save.type = 'submit'; form.append(save);
      form.addEventListener('submit', async event => {
        event.preventDefault();
        try {
          await api(`/api/inspection-templates/${template.id}`, 'PATCH',
            { thresholds: Object.fromEntries([...thresholds].map(([id, input]) => [id, Number(input.value)])) });
          notice(`${template.title} 已发布新版本。`); document.activeElement?.blur(); await refresh();
        } catch (error) { notice(error.message, true); }
      });
      details.append(form); templateList.append(details);
    }
  }
  const runList = $('inspection-runs'); runList.replaceChildren();
  if (!runs.length) runList.append(cell('p', '尚无巡检历史。', 'empty'));
  for (const run of runs) {
    const details = document.createElement('details'); details.className = 'rule-card';
    const asset = assets.find(item => item.id === run.assetId);
    details.append(cell('summary', `${asset?.name ?? '资产'} · ${run.templateId} v${run.templateVersion} · ${run.status === 'failed' ? '执行失败' : `风险 ${run.summary.risk} / 未知 ${run.summary.unknown} / 不适用 ${run.summary.notApplicable ?? 0}`} · ${formatTime(run.completedAt)}`));
    details.append(cell('small', `巡检 ID ${run.id} · 样本 ${run.sampleId}${run.retryOf ? ' · 重试自 ' + run.retryOf : ''}`));
    renderRelatedEventButtons(details, run.eventIds);
    if (run.error) details.append(cell('p', run.error, 'warning'));
    for (const finding of run.findings) {
      const change = finding.previousStatus && finding.previousStatus !== finding.status
        ? ` · 从 ${finding.previousStatus} 变为 ${finding.status}` : '';
      details.append(cell('p', `${finding.status} · ${finding.title} · ${finding.observed ?? '未采集'} / ${finding.threshold} · ${finding.source}${finding.reason ? ' · ' + finding.reason : ''}${change}`, 'evidence'));
    }
    const retry = cell('button', '重新巡检', 'ghost small-button write-action');
    retry.addEventListener('click', async () => {
      try { const { run: repeated } = await api(`/api/inspection-runs/${run.id}/retry`, 'POST');
        notice(repeated.status === 'failed' ? repeated.error : '重新巡检已完成。', repeated.status === 'failed'); await refresh(); }
      catch (error) { notice(error.message, true); }
    });
    const markdown = cell('button', '下载 Markdown', 'ghost small-button');
    markdown.addEventListener('click', () => { window.location.href = `/api/inspection-runs/${run.id}/report?format=md`; });
    const html = cell('button', '下载 HTML', 'ghost small-button');
    html.addEventListener('click', () => { window.location.href = `/api/inspection-runs/${run.id}/report?format=html`; });
    const actions = document.createElement('div'); actions.className = 'actions';
    actions.append(retry, markdown, html); details.append(actions); runList.append(details);
  }
}

function renderAssets(assets) {
  const list = $('asset-list');
  list.replaceChildren();
  if (!assets.length) { list.append(cell('p', '尚无资产。请先在右侧登记 PostgreSQL。', 'empty')); return; }
  for (const asset of assets) {
    const item = document.createElement('article'); item.className = 'list-item';
    const info = document.createElement('div');
    info.append(cell('strong', asset.name), cell('small', `${asset.host}:${asset.port} · ${asset.runtime === 'k8s' ? 'K8S / ' + asset.namespace : 'Linux'}`));
    info.append(cell('small', `PG ${asset.versionNum ? Math.floor(asset.versionNum / 10000) : '待识别'}${asset.pg14Eol ? ' · EOL' : ''} · ${formatTime(asset.lastSampleAt)}`));
    const actions = document.createElement('div'); actions.className = 'actions';
    actions.append(cell('span', asset.lastStatus === 'ok' ? '正常' : asset.lastStatus === 'error' ? '采集失败' : '待采集', `pill ${asset.lastStatus}`));
    const view = cell('button', '查看', 'ghost small-button'); view.addEventListener('click', () => { setView('assets'); void showAsset(asset.id); });
    const edit = cell('button', '编辑', 'ghost small-button write-action'); edit.addEventListener('click', () => editAsset(asset));
    const check = cell('button', '采集', 'ghost small-button write-action'); check.addEventListener('click', async () => {
      try {
        const { result } = await api(`/api/assets/${asset.id}/check`, 'POST');
        const failed = result?.status === 'error';
        notice(failed ? `${asset.name} 采集失败，已记录问题事件。` : `${asset.name} 采集正常。`, failed);
        await refresh();
      }
      catch (e) { notice(e.message, true); }
    });
    const remove = cell('button', '删除', 'danger small-button write-action'); remove.addEventListener('click', async () => {
      if (!confirm(`删除资产“${asset.name}”及其采样和事件？`)) return;
      try { await api(`/api/assets/${asset.id}`, 'DELETE'); if (selectedAsset === asset.id) { selectedAsset = null; $('detail').textContent = '选择资产查看最新采样。'; } await refresh(); }
      catch (e) { notice(e.message, true); }
    });
    actions.append(view, edit, check, remove); item.append(info, actions); list.append(item);
  }
}

async function showAsset(id) {
  selectedAsset = id;
  const [{ asset }, { samples }, { performance }, { entries: slowLogs }, { plans }] = await Promise.all([
    api(`/api/assets/${id}`), api(`/api/assets/${id}/samples`),
    api(`/api/assets/${id}/sql`), api(`/api/assets/${id}/slow-logs`),
    api(`/api/assets/${id}/sql-plans`)]);
  const detail = $('detail'); detail.replaceChildren();
  detail.append(cell('h3', asset.name));
  detail.append(cell('p', `${asset.host}:${asset.port}/${asset.database} · ${asset.runtime === 'k8s' ? `${asset.k8sCluster}/${asset.namespace}/${asset.pod || asset.workload || '未映射'}` : asset.linuxHost || 'Linux 主机未关联'}`));
  if (asset.pg14Eol) detail.append(cell('p', 'PostgreSQL 14 已结束上游支持，请规划升级。', 'warning'));
  detail.append(cell('p', `PgBouncer：${asset.poolerConfigured ? '已配置只读统计连接' : '未配置'}`));
  detail.append(cell('p', `采集状态：${asset.lastStatus} · ${formatTime(asset.lastSampleAt)}${asset.lastError ? ' · ' + asset.lastError : ''}`));
  if (pg18Store) {
    const to = new Date(); const from = new Date(to.getTime() - 24 * 3600 * 1000);
    const { series } = await api(`/api/assets/${id}/series?from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(to.toISOString())}&bucketHours=1`);
    detail.append(cell('h3', '近 24 小时采样趋势'));
    if (!series.length) detail.append(cell('p', '暂无趋势数据。', 'empty'));
    for (const point of series) detail.append(cell('p', `${formatTime(point.bucket)} · ${point.samples} 次采样 · ${point.errors} 次失败 · 平均连接 ${point.averageConnections ?? '未采集'}`, 'evidence'));
  }
  const layerNav = document.createElement('div'); layerNav.className = 'actions layer-nav';
  for (const [label, section] of [
    ['PostgreSQL 诊断', 'pg-diagnostics'],
    ['Top SQL', 'sql-performance'],
    ['慢 SQL 日志', 'slow-logs'],
    ['执行计划', 'sql-plans'],
    [asset.runtime === 'k8s' ? 'Pod / Node / PVC' : 'Linux 主机', asset.runtime === 'k8s' ? 'k8s-detail' : 'linux-detail'],
    ...(asset.poolerConfigured ? [['PgBouncer', 'pooler-detail']] : [])
  ]) {
    const button = cell('button', label, 'ghost small-button');
    button.addEventListener('click', () => {
      const target = $(section);
      if (target) target.scrollIntoView({ behavior: 'smooth', block: 'start' });
      else notice(`${label} 当前没有可用采样。`, true);
    });
    layerNav.append(button);
  }
  detail.append(layerNav);
  const sample = samples[0];
  const currentSample = sample?.collectedAt === asset.lastSampleAt ? sample : null;
  if (currentSample?.status === 'ok' && currentSample.metrics) {
    const metrics = currentSample.metrics;
    if (metrics.privileged_role) detail.append(cell('p', '当前监控账号具有高权限。建议改用专用 pg_monitor 账号。', 'warning'));
    else if (!metrics.monitor_role) detail.append(cell('p', '当前账号未授予 pg_monitor，部分指标可能不可用。', 'warning'));
    const grid = document.createElement('div'); grid.className = 'metric-grid';
    for (const [label, value] of [['角色', metrics.role], ['监控权限', metrics.monitor_role ? 'pg_monitor' : '有限权限'], ['连接数', metrics.connections], ['提交事务累计', metrics.xact_commit], ['回滚事务累计', metrics.xact_rollback], ['数据库容量', `${(metrics.database_bytes / 1024 / 1024).toFixed(1)} MiB`], ['复制连接', metrics.replicas ?? metrics.wal_receiver ?? '未采集']]) {
      const box = document.createElement('div'); box.append(cell('span', label), cell('strong', String(value))); grid.append(box);
    }
    detail.append(grid);
    if (metrics.diagnostics) renderDiagnostics(detail, metrics.diagnostics);
  } else detail.append(cell('p', '当前配置尚无成功采样。', 'empty'));
  renderSqlPerformance(detail, performance);
  renderSlowLogs(detail, asset, slowLogs);
  renderSqlPlans(detail, asset, plans);
  if (currentSample?.metrics?.backup) {
    const backup = currentSample.metrics.backup;
    const label = backup.status === 'ok'
      ? `pgBackRest ${backup.stanza} · 仓库${backup.repositoryHealthy ? '正常' : '异常'} · 最近成功备份 ${formatTime(backup.lastSuccessAt)} · 最近尝试 ${backup.lastAttemptStatus ?? '未提供'}`
      : `备份状态未知：${backup.reason ?? backup.status}`;
    const fresh = backup.lastSuccessAt && Date.now() - new Date(backup.lastSuccessAt).getTime() <= 24 * 3600000;
    detail.append(cell('p', label, backup.status === 'ok' && backup.repositoryHealthy &&
      backup.lastAttemptStatus !== 'failed' && fresh ? 'evidence' : 'warning'));
  }
  if (currentSample?.metrics?.k8s) {
    const k8s = currentSample.metrics.k8s;
    const heading = cell('h3', 'Kubernetes 运行状态'); heading.id = 'k8s-detail'; detail.append(heading);
    if (k8s.status === 'ok') {
      detail.append(cell('p', `Pod ${k8s.pod.name} · ${k8s.pod.phase || '未知'} · Node ${k8s.pod.node || '未映射'}`));
      if (k8s.podMapping) detail.append(cell('p', `Workload ${k8s.podMapping.workload} 已自动解析到新 Pod ${k8s.podMapping.resolved}；资产中原 Pod 映射 ${k8s.podMapping.configured || '未设置'}。`, 'hint'));
      for (const service of k8s.services || []) detail.append(cell('p', `Service ${service.name} · 端口 ${(service.ports || []).join(', ') || '未知'}`));
      if (k8s.servicesError) detail.append(cell('p', `Service 关联未完整采集：${k8s.servicesError}`, 'warning'));
      if (k8s.pod.reason) detail.append(cell('p', `Pod 原因：${k8s.pod.reason}${k8s.pod.message ? ' · ' + k8s.pod.message : ''}`, 'warning'));
      for (const condition of k8s.pod.conditions || []) if (condition.status !== 'True') detail.append(cell('p', `${condition.type}=${condition.status} · ${condition.reason || '未知原因'}${condition.message ? ' · ' + condition.message : ''}`, 'warning'));
      for (const container of k8s.containers) detail.append(cell('p', `${container.name} · 重启 ${container.restartCount} 次${container.terminatedReason ? ' · 最近终止：' + container.terminatedReason : ''}`, container.terminatedReason === 'OOMKilled' ? 'warning' : ''));
      for (const pvc of k8s.pvcs) {
        detail.append(cell('p', `PVC ${pvc.name} · ${pvc.phase || pvc.error || '未知'} · 申请 ${pvc.requested || '未知'} / 容量 ${pvc.capacity || '未知'}`));
        if (pvc.volume) detail.append(cell('p', `PV ${pvc.volume.name} · ${pvc.volume.phase || '状态未知'}`));
        if (pvc.volumeError) detail.append(cell('p', `PV 未采集：${pvc.volumeError}`, 'warning'));
        if (pvc.usage?.status === 'ok') detail.append(cell('p', `PVC 实际已用 ${(pvc.usage.usedBytes / 1024 / 1024 / 1024).toFixed(2)} GiB · ${pvc.usage.source}`));
        else detail.append(cell('p', `PVC 实际用量未采集：${pvc.usage?.reason || '未知原因'}`, 'warning'));
      }
      if (k8s.node) detail.append(cell('p', `Node ${k8s.node.name} · ${k8s.node.conditions.map(c => `${c.type}=${c.status}`).join('，') || '状态未知'}`));
      else if (k8s.nodeError) detail.append(cell('p', `Node 状态未采集：${k8s.nodeError}`, 'warning'));
      for (const service of k8s.services || []) detail.append(cell('p', `Service ${service.name} · 端口 ${service.ports.join('、')}`));
      if (k8s.servicesError) detail.append(cell('p', `Service 未采集：${k8s.servicesError}`, 'warning'));
      if (k8s.resourceMetrics) {
        detail.append(cell('p', `资源来源 ${k8s.resourceMetrics.source} · ${formatTime(k8s.resourceMetrics.observedAt)}`));
        for (const container of k8s.resourceMetrics.containers) detail.append(cell('p', `${container.name}：CPU ${container.cpu || '未知'}，内存 ${container.memory || '未知'}`));
        if (k8s.resourceMetrics.node) detail.append(cell('p', `Node 使用量：CPU ${k8s.resourceMetrics.node.cpu || '未知'}，内存 ${k8s.resourceMetrics.node.memory || '未知'}`));
      }
      if (k8s.resourceMetricsError) detail.append(cell('p', `K8S 资源使用量未采集：${k8s.resourceMetricsError}`, 'warning'));
      if (k8s.nodeMetricsError) detail.append(cell('p', `Node 资源使用量未采集：${k8s.nodeMetricsError}`, 'warning'));
      for (const event of k8s.events) detail.append(cell('p', `${formatTime(event.at)} · ${event.reason || 'Event'} · ${event.message || ''}`, 'evidence'));
      if (k8s.eventsError) detail.append(cell('p', `K8S Event 未采集：${k8s.eventsError}`, 'warning'));
    } else detail.append(cell('p', `K8S 未采集：${k8s.reason || k8s.status}`, 'warning'));
  }
  if (currentSample?.metrics?.linux) {
    const linux = currentSample.metrics.linux;
    const heading = cell('h3', 'Linux 主机资源'); heading.id = 'linux-detail'; detail.append(heading);
    if (linux.status === 'ok') {
      detail.append(cell('p', `来源 ${linux.source} · CPU 已用 ${linux.cpu.usedPercent ?? '待下次采样'}% · 内存已用 ${linux.memory.usedPercent ?? '未知'}% · 1 分钟负载 ${linux.cpu.load1 ?? '未知'}`));
      for (const fs of linux.filesystems) detail.append(cell('p', `${fs.mountpoint}: ${fs.usedPercent}% 已用 · ${(fs.sizeBytes / 1024 / 1024 / 1024).toFixed(1)} GiB`));
      detail.append(cell('p', `磁盘累计读取 ${(linux.disk.readBytesTotal / 1024 / 1024).toFixed(1)} MiB，写入 ${(linux.disk.writtenBytesTotal / 1024 / 1024).toFixed(1)} MiB；网络累计接收 ${(linux.network.receiveBytesTotal / 1024 / 1024).toFixed(1)} MiB，发送 ${(linux.network.transmitBytesTotal / 1024 / 1024).toFixed(1)} MiB`));
      if (linux.disk.readBytesPerSecond != null) {
        const speed = value => value == null ? '未知' : `${(value / 1024 / 1024).toFixed(2)} MiB/s`;
        detail.append(cell('p', `磁盘读 ${speed(linux.disk.readBytesPerSecond)} · 写 ${speed(linux.disk.writtenBytesPerSecond)}；网络收 ${speed(linux.network.receiveBytesPerSecond)} · 发 ${speed(linux.network.transmitBytesPerSecond)}`));
      }
    } else detail.append(cell('p', `Linux 未采集：${linux.reason || linux.status}`, 'warning'));
  }
  if (currentSample?.metrics?.pgbouncer) {
    const pooler = currentSample.metrics.pgbouncer;
    const heading = cell('h3', 'PgBouncer 连接池'); heading.id = 'pooler-detail'; detail.append(heading);
    if (pooler.status === 'ok') {
      for (const entry of pooler.pools) detail.append(cell('p', `${entry.user} · 客户端活跃 ${entry.clientActive} / 等待 ${entry.clientWaiting} · 服务端活跃 ${entry.serverActive} / 空闲 ${entry.serverIdle} · 最久等待 ${entry.maxWaitSeconds.toFixed(2)} 秒`));
      for (const entry of pooler.stats) detail.append(cell('p', `累计事务 ${entry.transactionsTotal} · 累计查询 ${entry.queriesTotal} · 平均查询 ${entry.avgQueryMicroseconds} μs`));
    } else detail.append(cell('p', `PgBouncer 未采集：${pooler.reason || pooler.status}`, 'warning'));
  }
  if (pg18Store) {
    const { forecast } = await api(`/api/assets/${id}/capacity`);
    detail.append(cell('h3', '数据库容量趋势'));
    if (forecast.status === 'estimate') {
      const gib = value => `${(value / 1024 ** 3).toFixed(2)} GiB`;
      detail.append(cell('p', `最近 ${forecast.observedDays.toFixed(0)} 天、${forecast.points.length} 个每日样本；当前 ${gib(forecast.latestBytes)}，每日增长约 ${gib(forecast.dailyGrowthBytes)}，30 天后约 ${gib(forecast.projected30DayBytes)}。置信度 ${forecast.confidence}；${forecast.method}。`, 'evidence'));
      detail.append(cell('small', `最新样本 ${forecast.latestSampleId}；估计不代表磁盘剩余空间或备份可恢复性。`));
    } else detail.append(cell('p', `容量预测暂不可用：${forecast.reason}`, 'empty'));
  }
}

function renderDiagnostics(target, diagnostics) {
  const heading = cell('h3', 'PostgreSQL 深度监控'); heading.id = 'pg-diagnostics'; target.append(heading);
  const rows = [
    ['等待与会话', 'activity', d => d.map(x => `${x.state}/${x.waitType}: ${x.sessions}`).join('；') || '无活动会话'],
    ['长事务', 'transactions', d => `${d.longTransactions} 个；idle in transaction ${d.idleInTransaction} 个`],
    ['锁阻塞', 'locks', d => `${d.blockedSessions} 个会话`],
    ['阻塞链', 'lockChain', d => d.length ? d.slice(0, 3).map(x => `${x.waitingPid} ← ${x.blockerPid} (${x.waitType || 'wait'})`).join('；') : '无阻塞链'],
    ['死锁与缓存', 'database', d => `${d.deadlocks} 次死锁；命中 ${d.blocksHit} / 读取 ${d.blocksRead}`],
    ['WAL', 'wal', d => `${(Number(d.bytes) / 1024 / 1024).toFixed(1)} MiB 累计；${d.records} 条记录`],
    ['归档', 'archiver', d => `成功 ${d.archivedCount}，失败 ${d.failedCount}`],
    ['检查点', 'checkpoints', d => `定时 ${d.timed}，请求 ${d.requested}`],
    ['复制槽', 'slots', d => `总数 ${d.total}，活跃 ${d.active}`],
    ['事务冻结年龄', 'freeze', d => `${d.xidAge} / ${d.maxAge} XID（${d.usedPercent}%）`],
    ['复制槽 WAL 保留', 'slotDetails', d => d.length ? d.slice(0, 3).map(x => `${x.name}: ${x.active ? '活跃' : '未活跃'} / ${x.walStatus || '未知'} / ${x.retainedBytes == null ? '保留量未采集' : (x.retainedBytes / 1024 / 1024).toFixed(1) + ' MiB'}`).join('；') : '无复制槽'],
    ['关键参数', 'settings', d => d.map(x => `${x.name}=${x.value}${x.unit || ''}`).join('；')],
    ['autovacuum', 'vacuum', d => d.length ? d.slice(0, 3).map(x => `${x.schema}.${x.table}: 死元组估计 ${x.deadRowsEstimate}`).join('；') : '暂无用户表'],
    ['表与索引', 'tables', d => d.length ? d.slice(0, 3).map(x => `${x.schema}.${x.table}: 总量 ${(x.totalBytes / 1024 / 1024).toFixed(1)} MiB / 索引 ${(x.indexBytes / 1024 / 1024).toFixed(1)} MiB`).join('；') : '暂无用户表']
  ];
  const list = document.createElement('div'); list.className = 'diagnostic-list';
  for (const [label, key, format] of rows) {
    const entry = diagnostics[key];
    const row = document.createElement('div');
    row.append(cell('strong', label), cell('span', entry?.status === 'ok' ? format(entry.data) : `未采集：${entry?.reason || '未知'}`));
    list.append(row);
  }
  target.append(list);
}

function renderSqlPerformance(target, performance) {
  const heading = cell('h3', 'Top SQL 指纹'); heading.id = 'sql-performance'; target.append(heading);
  if (performance.status !== 'ok') {
    target.append(cell('p', `SQL 性能未采集：${performance.reason || performance.status}。启用 pg_stat_statements 后自动出现聚合指标。`, 'warning'));
    renderRelatedEventButtons(target, performance.eventIds);
    return;
  }
  target.append(cell('p', `来源 pg_stat_statements · ${formatTime(performance.collectedAt)} · 样本 ${performance.sampleId} · ${performance.comparison === 'interval' ? '相邻采样增量' : '扩展累计值（无可比样本）'}`, 'evidence'));
  target.append(cell('p', '默认不采集或保存 SQL 正文；指纹只在当前实例内比较，跨大版本升级不保证稳定。', 'hint'));
  if (performance.truncated) target.append(cell('p', '只采集累计耗时最高的前 50 个指纹，低排名语句可能没有趋势。', 'hint'));
  if (performance.hiddenQueryCount) target.append(cell('p', `${performance.hiddenQueryCount} 条语句缺少可见 Query ID，未纳入指纹比较。`, 'warning'));
  if (!performance.statements.length) target.append(cell('p', '暂无可见 SQL 指纹。', 'empty'));
  const list = document.createElement('div'); list.className = 'diagnostic-list';
  for (const [index, row] of performance.statements.entries()) {
    const entry = document.createElement('div');
    const delta = row.delta;
    entry.append(cell('strong', `Query ID ${row.queryId}`));
    entry.append(cell('span', delta
      ? `本期调用 ${delta.calls} 次 · 平均 ${delta.averageExecMs.toFixed(1)} ms · 总耗时 ${delta.totalExecMs.toFixed(1)} ms · 共享块读/写 ${delta.sharedBlocksRead}/${delta.sharedBlocksWritten} · 临时块读/写 ${delta.tempBlocksRead}/${delta.tempBlocksWritten}`
      : `累计调用 ${row.calls} 次 · 总耗时 ${row.totalExecMs.toFixed(1)} ms · 共享块读/写 ${row.sharedBlocksRead}/${row.sharedBlocksWritten} · 临时块读/写 ${row.tempBlocksRead}/${row.tempBlocksWritten}`));
    if (row.regression) entry.append(cell('span', `平均耗时较上一窗口上升 ${row.regression.ratio.toFixed(1)} 倍；请结合事件和资源证据复核。`, 'warning'));
    if (index < 5 && row.trend?.length) entry.append(cell('span', row.trend.slice(-3).map(point =>
      `${formatTime(point.at)}：${point.averageExecMs.toFixed(1)} ms` +
      (point.linuxCpuPercent == null ? '' : ` / CPU ${point.linuxCpuPercent}%`) +
      (point.linuxMemoryPercent == null ? '' : ` / 内存 ${point.linuxMemoryPercent}%`) +
      (point.podPhase ? ` / Pod ${point.podPhase}` : '') +
      (point.podRestarts == null ? '' : ` / 重启 ${point.podRestarts}`)).join('；'), 'evidence'));
    list.append(entry);
  }
  target.append(list);
  renderRelatedEventButtons(target, performance.eventIds);
}

function renderRelatedEventButtons(target, eventIds = []) {
  if (!eventIds.length) return;
  const events = document.createElement('div'); events.className = 'actions';
  for (const id of eventIds) {
    const link = cell('button', `关联事件 ${id.slice(0, 8)}`, 'ghost small-button');
    link.addEventListener('click', () => showEvent(id)); events.append(link);
  }
  target.append(events);
}

function renderSlowLogs(target, asset, entries) {
  const heading = cell('h3', '慢 SQL 日志'); heading.id = 'slow-logs'; target.append(heading);
  target.append(cell('p', '导入 PostgreSQL csvlog（PG14–18）或 jsonlog（PG15–18）的已轮转片段。每批不超过 60 KiB；仅保存时间、耗时和 Query ID，语句正文与参数在导入时丢弃。', 'hint'));
  const form = document.createElement('form'); form.className = 'actions';
  const format = document.createElement('select');
  for (const value of ['csvlog', 'jsonlog']) {
    const option = document.createElement('option'); option.value = value; option.textContent = value;
    format.append(option);
  }
  const file = document.createElement('input'); file.type = 'file'; file.required = true;
  file.accept = '.csv,.json,.log,.txt';
  const submit = cell('button', '导入片段'); submit.type = 'submit';
  form.append(format, file, submit);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!file.files?.[0]) return;
    try {
      if (file.files[0].size > 60000) throw new Error('文件超过 60 KiB，请按日志记录拆分批次。');
      const result = await api(`/api/assets/${asset.id}/slow-logs/import`, 'POST', {
        format: format.value, content: await file.files[0].text()
      });
      notice(`扫描 ${result.scanned} 条，导入 ${result.imported} 条，重复 ${result.duplicates} 条。`);
      await showAsset(asset.id);
    } catch (error) { notice(error.message, true); }
  });
  target.append(form);
  if (!entries.length) target.append(cell('p', '近 24 小时没有导入的慢 SQL 记录。', 'empty'));
  for (const row of entries.slice(0, 30)) {
    const item = document.createElement('div'); item.className = 'evidence';
    item.append(cell('span', `${formatTime(row.observedAt)} · ${row.durationMs.toFixed(1)} ms · ${row.queryId ? `Query ID ${row.queryId}` : 'Query ID 未提供'} · ${row.sourceFormat}`));
    for (const id of row.eventIds) {
      const link = cell('button', `事件 ${id.slice(0, 8)}`, 'ghost small-button');
      link.addEventListener('click', () => showEvent(id)); item.append(link);
    }
    target.append(item);
  }
}

function renderSqlPlans(target, asset, plans) {
  const heading = cell('h3', '执行计划证据'); heading.id = 'sql-plans'; target.append(heading);
  target.append(cell('p', '手动导入已有的 EXPLAIN (FORMAT JSON) 结果。DbSphere 不向生产数据库自动执行 EXPLAIN ANALYZE；仅保留节点类型和数值，过滤条件、SQL 正文与参数不入库。', 'hint'));
  const form = document.createElement('form'); form.className = 'actions';
  const queryId = document.createElement('input'); queryId.placeholder = 'Query ID';
  queryId.required = true; queryId.pattern = '-?[0-9]{1,20}';
  const file = document.createElement('input'); file.type = 'file'; file.accept = '.json'; file.required = true;
  const submit = cell('button', '导入计划'); submit.type = 'submit';
  form.append(queryId, file, submit);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    try {
      if (file.files[0].size > 50000) throw new Error('计划文件超过 50 KiB。');
      const plan = JSON.parse(await file.files[0].text());
      await api(`/api/assets/${asset.id}/sql-plans`, 'POST', {
        queryId: queryId.value, observedAt: new Date().toISOString(), plan
      });
      notice('执行计划摘要已导入。'); await showAsset(asset.id);
    } catch (error) { notice(error.message, true); }
  });
  target.append(form);
  if (!plans.length) target.append(cell('p', '尚无执行计划证据。', 'empty'));
  for (const plan of plans.slice(0, 20)) {
    const item = document.createElement('div'); item.className = 'evidence';
    item.append(cell('span', `Query ID ${plan.queryId} · ${formatTime(plan.observedAt)} · ${plan.summary.nodeCount} 个节点 · ${plan.summary.executionMs == null ? '无实际执行时间' : `${plan.summary.executionMs.toFixed(1)} ms`}`));
    if (plan.comparison?.shapeChanged) item.append(cell('span', '计划结构较上一份变化', 'warning'));
    if (plan.comparison?.executionRatio >= 2) item.append(cell('span', `导入计划的执行时间比前次高 ${plan.comparison.executionRatio.toFixed(1)} 倍，请结合采样复核。`, 'warning'));
    for (const id of plan.eventIds) {
      const link = cell('button', `事件 ${id.slice(0, 8)}`, 'ghost small-button');
      link.addEventListener('click', () => showEvent(id)); item.append(link);
    }
    target.append(item);
  }
}

function renderEvents(events) {
  const list = $('event-list'); list.replaceChildren();
  if (!events.length) { list.append(cell('p', '暂无问题事件。', 'empty')); return; }
  for (const event of events) {
    const button = cell('button', '', 'event-button incident-card');
    button.dataset.eventId = event.id;
    button.append(cell('strong', event.title, 'incident-card-title'));
    const meta = cell('span', '', 'incident-card-meta');
    meta.append(cell('span', event.status === 'open' ? '处理中' : '已恢复',
      `incident-status ${event.status === 'open' ? 'is-open' : 'is-resolved'}`));
    meta.append(cell('span', event.assetName || '已删除资产'));
    button.append(meta);
    button.append(cell('small', `事件 ${event.id.slice(0, 8)} · ${formatTime(event.startedAt)}`, 'incident-card-time'));
    button.classList.toggle('selected', event.id === selectedEvent);
    if (event.id === selectedEvent) button.setAttribute('aria-current', 'true');
    button.addEventListener('click', () => showEvent(event.id)); list.append(button);
  }
}

function renderAlerts(alerts) {
  const list = $('alert-list'); list.replaceChildren();
  if (!alerts.length) { list.append(cell('p', '当前没有触发中的告警。', 'empty')); return; }
  for (const alert of alerts) {
    const button = cell('button', `${alert.assetName} · ${alert.rule.title} · ${alert.rule.severity} · ${formatAlertValue(alert.rule.metric, alert.state.lastValue)}${alert.state.missingSince ? ' · 指标已失联' : ''}${alert.state.mutedBy ? ' · 通知已静默' : ''}`, 'event-button');
    button.addEventListener('click', () => showEvent(alert.state.eventId));
    list.append(button);
  }
}

function renderSilenceOptions(assets, rules) {
  for (const [id, items, label] of [['silence-asset', assets, 'name'], ['silence-rule', rules, 'title']]) {
    const select = $(id);
    const selected = select.value;
    select.replaceChildren(cell('option', id === 'silence-asset' ? '全部资产' : '全部规则'));
    select.firstChild.value = '';
    for (const item of items) {
      const option = cell('option', item[label]); option.value = item.id; select.append(option);
    }
    select.value = selected;
  }
}

function renderSilences(silences, assets, rules) {
  const list = $('silence-list'); list.replaceChildren();
  if (!silences.length) { list.append(cell('p', '尚未创建静默或维护窗。', 'empty')); return; }
  const assetNames = new Map(assets.map(asset => [asset.id, asset.name]));
  const ruleNames = new Map(rules.map(rule => [rule.id, rule.title]));
  const now = Date.now();
  for (const silence of silences) {
    const expired = Date.parse(silence.endsAt) <= now;
    const upcoming = Date.parse(silence.startsAt) > now;
    const status = silence.revokedAt ? '已撤销' : expired ? '已结束' : upcoming ? '待开始' : '生效中';
    const item = document.createElement('article'); item.className = 'list-item';
    const info = document.createElement('div');
    info.append(cell('strong', `${silence.kind === 'maintenance' ? '计划维护' : '告警静默'} · ${status}`));
    info.append(cell('small', `${assetNames.get(silence.assetId) || (silence.assetId ? '已删除资产' : '全部资产')} · ${ruleNames.get(silence.ruleId) || (silence.ruleId ? silence.ruleId : '全部规则')}`));
    info.append(cell('small', `${formatTime(silence.startsAt)} 至 ${formatTime(silence.endsAt)} · ${silence.reason}`));
    item.append(info);
    if (!silence.revokedAt && !expired) {
      const revoke = cell('button', '撤销', 'danger small-button write-action');
      revoke.addEventListener('click', async () => {
        try { await api(`/api/silences/${silence.id}`, 'DELETE'); notice('时间窗已撤销；下次采样将恢复通知。'); await refresh(); }
        catch (error) { notice(error.message, true); }
      });
      item.append(revoke);
    }
    list.append(item);
  }
}

function renderNotifications(notifications) {
  const list = $('notification-list'); list.replaceChildren();
  if (!notifications.length) { list.append(cell('p', '暂无通知记录。配置通道后，新的告警事件会投递。', 'empty')); return; }
  const labels = { pending: '待投递', delivered: '已送达', skipped: '未配置或未达到路由级别', failed: '投递失败' };
  for (const item of notifications.slice(0, 30)) {
    const row = document.createElement('div'); row.className = 'list-item';
    const button = cell('button', `${item.assetName} · ${item.channel === 'email' ? '邮件' : 'Webhook'} · ${labels[item.status] || item.status} · ${item.kind} · ${formatTime(item.observedAt)}${item.lastError ? ' · ' + item.lastError : ''}`, 'event-button');
    button.addEventListener('click', () => showEvent(item.eventId));
    row.append(button);
    if (item.status === 'failed') {
      const retry = cell('button', '重试', 'ghost small-button write-action');
      retry.addEventListener('click', async () => {
        try { await api(`/api/notifications/${item.id}/retry`, 'POST'); notice('通知已重新加入投递队列。'); await refresh(); }
        catch (error) { notice(error.message, true); }
      });
      row.append(retry);
    }
    list.append(row);
  }
}

function renderStorage(storage) {
  const target = $('storage-detail'); target.replaceChildren();
  if (storage.mode === 'scoped') {
    target.append(cell('p', '资产范围只读账号不显示全平台 PG18 存储统计。', 'empty'));
    return;
  }
  if (storage.mode === 'sqlite-preview') {
    target.append(cell('p', '当前是 SQLite 预览模式；正式长期存储需独立 PostgreSQL 18。', 'warning'));
    return;
  }
  const mib = value => `${(value / 1024 / 1024).toFixed(1)} MiB`;
  const grid = document.createElement('div'); grid.className = 'metric-grid';
  for (const [label, value] of [
    ['PG18 数据库占用', mib(storage.databaseBytes)],
    ['采样分区占用', mib(storage.samplePartitionBytes)],
    ['事件分区占用', mib(storage.eventPartitionBytes)],
    ['采样总数', storage.sampleCount],
    ['事件数 / 未结', `${storage.eventCount} / ${storage.openEventCount}`],
    ['预计每日采样', storage.estimatedSamplesPerDay],
    ['预计每日采样增长', `${mib(storage.estimatedSampleBytesPerDay)}（粗估）`],
    ['采样保留期', storage.retentionDays ? `${storage.retentionDays} 天` : '未启用自动清理'],
    ['已结事件保留期', storage.eventRetentionDays ? `${storage.eventRetentionDays} 天` : '永久保留'],
    ['最早采样', formatTime(storage.oldestSampleAt)],
    ['PG18 监控存储备份', storage.backup?.status === 'ok'
      ? `${storage.backup.ageHours} 小时前 · ${storage.backup.backupId}` : '未知 / 未配置'],
    ['备份上报恢复点', storage.backup?.status === 'ok' ? formatTime(storage.backup.restorePointAt) : '未知']
  ]) {
    const box = document.createElement('div'); box.append(cell('span', label), cell('strong', String(value))); grid.append(box);
  }
  target.append(grid);
  if (storage.backup?.status !== 'ok') target.append(cell('p', storage.backup?.reason ?? '监控存储备份状态未知', 'warning'));
}

function renderRules(rules) {
  const list = $('rule-list'); list.replaceChildren();
  for (const rule of rules) {
    const details = document.createElement('details'); details.className = 'rule-card';
    details.append(cell('summary', `${rule.enabled ? '启用' : '停用'} · ${rule.title} · ${rule.metric} ${rule.operator} ${formatAlertValue(rule.metric, rule.threshold)} · ${rule.durationSeconds}s`));
    if (rule.editable === false) {
      details.append(cell('p', 'Go 采集器尚未接入此规则；历史配置可查看，暂不能修改。', 'warning'));
      list.append(details);
      continue;
    }
    const form = document.createElement('form'); form.className = 'rule-form';
    const threshold = document.createElement('input'); threshold.type = 'number'; threshold.step = 'any'; threshold.min = '0';
    threshold.value = rule.metric === growthMetric ? Number(rule.threshold) / (1024 ** 3) : rule.threshold;
    threshold.required = true;
    const duration = document.createElement('input'); duration.type = 'number'; duration.step = '1'; duration.min = '0'; duration.max = '86400'; duration.value = rule.durationSeconds; duration.required = true;
    const severity = document.createElement('select');
    for (const value of ['info', 'warning', 'critical']) {
      const option = document.createElement('option'); option.value = value; option.textContent = value;
      severity.append(option);
    }
    severity.value = rule.severity;
    const enabled = document.createElement('input'); enabled.type = 'checkbox'; enabled.checked = rule.enabled;
    for (const [label, input] of [[rule.metric === growthMetric ? '阈值（GiB/日）' : '阈值', threshold], ['持续秒数', duration], ['级别', severity], ['启用', enabled]]) {
      const wrapper = document.createElement('label'); wrapper.append(cell('span', label), input); form.append(wrapper);
    }
    const save = cell('button', '保存规则', 'small-button'); save.type = 'submit'; form.append(save);
    form.addEventListener('submit', async event => {
      event.preventDefault();
      try {
        await api(`/api/alert-rules/${rule.id}`, 'PATCH', { threshold: Number(threshold.value) * (rule.metric === growthMetric ? 1024 ** 3 : 1),
          durationSeconds: Number(duration.value), severity: severity.value, enabled: enabled.checked });
        notice(`${rule.title} 已更新；新配置将在下次采样时生效。`);
        document.activeElement?.blur(); await refresh();
      } catch (error) { notice(error.message, true); }
    });
    details.append(form); list.append(details);
  }
}

function editAsset(asset) {
  editingAsset = asset.id;
  const form = $('asset-form');
  for (const [field, value] of Object.entries({
    name: asset.name, host: asset.host, port: asset.port, database: asset.database,
    username: asset.username, sslMode: asset.sslMode, runtime: asset.runtime,
    linuxHost: asset.linuxHost, linuxExporterPort: asset.linuxExporterPort, k8sCluster: asset.k8sCluster, namespace: asset.namespace,
    workload: asset.workload, pod: asset.pod, node: asset.node
  })) form.elements[field].value = value ?? '';
  form.elements.password.value = '';
  form.elements.poolerUrl.value = '';
  form.elements.poolerClear.checked = false;
  $('runtime').dispatchEvent(new Event('change'));
  $('asset-submit').textContent = '更新资产';
  $('cancel-edit').hidden = false;
  form.scrollIntoView({ behavior: 'smooth', block: 'start' });
}

function resetEdit() {
  editingAsset = null;
  $('asset-form').reset();
  $('runtime').dispatchEvent(new Event('change'));
  $('asset-submit').textContent = '保存资产';
  $('cancel-edit').hidden = true;
}

async function showEvent(id) {
  setView('incidents');
  const { event, analysis } = await api(`/api/events/${id}`);
  const eventAudit = currentRole === 'admin' ? await api(`/api/events/${id}/audit`).catch(() => null) : null;
  selectedEvent = id;
  for (const button of $('event-list').querySelectorAll('[data-event-id]')) {
    button.classList.toggle('selected', button.dataset.eventId === id);
    if (button.dataset.eventId === id) button.setAttribute('aria-current', 'true');
    else button.removeAttribute('aria-current');
  }
  const target = $('event-detail'); target.replaceChildren();
  target.append(cell('h3', `${event.title} · ${event.status === 'open' ? '处理中' : '已恢复'}`));
  target.append(cell('small', `事件 ID ${event.id} · ${formatTime(event.startedAt)} 至 ${formatTime(event.endedAt)}`));
  const diagnosis = document.createElement('div'); diagnosis.className = 'evidence';
  diagnosis.append(cell('h4', '同一事件 ID 的证据链与根因候选'));
  diagnosis.append(cell('p', `证据图：${analysis.graph.nodes.length} 个节点、${analysis.graph.edges.length} 条关系；${analysis.timeline.length} 条时间线记录。`));
  if (analysis.traceCoverage) diagnosis.append(cell('small', analysis.traceCoverage.spans
    ? `应用链路：${analysis.traceCoverage.services.join('、')} · ${analysis.traceCoverage.databaseSpans} 条 PostgreSQL 客户端 Span；采样率和时钟偏移未知。`
    : '应用链路：事件时间窗内无 Trace；数据库、Linux 与 K8S 侧诊断仍可使用。'));
  if (analysis.traceCoverage?.serviceImpact?.length) {
    const impact = document.createElement('details'); impact.append(cell('summary', '查看应用请求关联证据'));
    for (const service of analysis.traceCoverage.serviceImpact) {
      const row = document.createElement('div'); row.className = 'evidence';
      row.append(cell('p', `${service.service} · 同期 Trace ${service.observedTraces ?? 0} 条 · 故障信号关联 ${service.correlatedTraces ?? 0} 条（数据库错误 ${service.errorTraces ?? 0}，锁等待 PID 匹配 ${service.lockWaitTraces ?? 0}）· Span ${service.spans} · 数据库调用 ${service.databaseSpans} · 错误 Span ${service.errorSpans} · ${formatTime(service.firstAt)} 至 ${formatTime(service.lastAt)}`));
      if (service.traceIds?.length && event.assetId) {
        const links = document.createElement('p'); links.append(cell('small', '关联 Trace：'));
        for (const traceId of service.traceIds) {
          if (!/^[0-9a-f]{32}$/.test(traceId)) continue;
          const link = document.createElement('a'); link.href = `/api/assets/${event.assetId}/traces/${traceId}`;
          link.textContent = `${traceId.slice(0, 12)}…`; link.title = traceId;
          links.append(link, document.createTextNode(' '));
        }
        row.append(links);
      }
      impact.append(row);
    }
    impact.append(cell('small', '故障信号关联指数据库客户端错误或与锁样本精确匹配的后端 PID，并沿父 Span 关联请求；同窗出现与 PID 匹配都不能单独证明根因。'));
    if (analysis.traceCoverage.atLimit) impact.append(cell('small', '已达到 100 条 Span 显示上限，计数可能不完整。'));
    diagnosis.append(impact);
  }
  const graphTypes = new Set(analysis.graph.nodes.map(node => node.type));
  const chain = document.createElement('div'); chain.className = 'flow-strip';
  for (const [title, present, available, missing] of [
    ['应用请求', graphTypes.has('db_client_span'), '有数据库调用 Span', '未接入 Trace'],
    ['PostgreSQL', graphTypes.has('sample'), '有采样证据', '样本缺失'],
    ['会话与 SQL', ['wait', 'session', 'sql_fingerprint', 'slow_log', 'plan'].some(type => graphTypes.has(type)),
      '有等待、锁或 SQL 证据', '尚无会话或 SQL 证据'],
    ['Linux / K8S', ['linux', 'pod', 'pvc', 'k8s_node'].some(type => graphTypes.has(type)),
      '有主机或容器证据', '基础设施信号缺失']
  ]) {
    const step = document.createElement('div'); step.className = `flow-step ${present ? 'present' : 'missing'}`;
    step.append(cell('strong', title), cell('span', present ? available : missing));
    chain.append(step);
  }
  diagnosis.append(chain);
  const graphDetails = document.createElement('details');
  graphDetails.append(cell('summary', '查看证据关系'));
  const graphNodes = new Map(analysis.graph.nodes.map(node => [node.id, node]));
  for (const edge of analysis.graph.edges) {
    const from = graphNodes.get(edge.from); const to = graphNodes.get(edge.to);
    const relation = cell('p', `${from?.type ?? '未知'} ${from?.label ?? edge.from} → ${edge.relation} → ${to?.type ?? '未知'} ${to?.label ?? edge.to}`, 'evidence');
    if (to?.source) {
      const source = cell('a', ' · 原始数据');
      source.href = to.source;
      source.target = '_blank';
      source.rel = 'noopener noreferrer';
      relation.append(source);
    }
    graphDetails.append(relation);
  }
  diagnosis.append(graphDetails);
  if (analysis.status === 'unlocated') diagnosis.append(cell('p', '未定位：当前证据不足以支持根因候选。', 'warning'));
  for (const candidate of analysis.candidates) {
    diagnosis.append(cell('p', `${candidate.title} · 置信级别 ${candidate.confidence} · 证据 ${candidate.supports.join('、')}。${candidate.limitation}`, 'warning'));
    if (candidate.matchedConditions?.length) diagnosis.append(cell('small', `匹配条件：${candidate.matchedConditions.map(item =>
      `${item.metric}：${item.observed ?? '未知'} ${item.operator ?? ''} ${item.threshold ?? ''}`).join('；')}`));
    if (candidate.against?.length) diagnosis.append(cell('small', `反证：${candidate.against.join('；')}`));
    if (candidate.timeOrder) diagnosis.append(cell('small', `时间顺序：${candidate.timeOrder.firstObservedAt
      ? formatTime(candidate.timeOrder.firstObservedAt) : '未知'} · ${candidate.timeOrder.relation} · 事件开始 ${formatTime(candidate.timeOrder.eventStartedAt)}`));
    if (candidate.missing.length) diagnosis.append(cell('small', `缺失：${candidate.missing.join('；')}`));
    if (candidate.review) diagnosis.append(cell('p', `人工${candidate.review.decision === 'confirmed' ? '确认' : '驳回'} · ${formatTime(candidate.review.at)} · ${candidate.review.reason}`, 'evidence'));
    for (const [decision, label] of [['confirmed', '确认候选'], ['rejected', '驳回候选']]) {
      const button = cell('button', label, 'ghost small-button write-action');
      button.addEventListener('click', async () => {
        const reason = window.prompt(`${label}：请填写依据（至少 5 个字符）`);
        if (reason === null) return;
        try {
          await api(`/api/events/${event.id}/rca-decision`, 'POST',
            { code: candidate.code, decision, reason });
          await showEvent(event.id);
        } catch (error) { notice(error.message, true); }
      });
      diagnosis.append(button);
    }
  }
  if (analysis.missingSignals.length) diagnosis.append(cell('small', `信号缺口：${analysis.missingSignals.join('；')}`));
  target.append(diagnosis);
  const reports = document.createElement('div'); reports.className = 'actions';
  for (const [format, label] of [['md', '下载事件报告 · Markdown'], ['html', '下载事件报告 · HTML']]) {
    const button = cell('button', label, 'ghost small-button');
    button.addEventListener('click', () => { window.location.href = `/api/events/${event.id}/report?format=${format}`; });
    reports.append(button);
  }
  target.append(reports);
  if (currentRole === 'admin') {
    const auditDetails = document.createElement('details'); auditDetails.className = 'settings-details';
    auditDetails.append(cell('summary', '查看该事件的管理员操作记录'));
    const auditList = document.createElement('div'); auditList.className = 'list';
    renderAdminAudit(eventAudit?.entries, auditList);
    auditDetails.append(auditList); target.append(auditDetails);
  }
  const aiStatus = await api('/api/ai/status').catch(() => ({ enabled: false }));
  if (aiStatus.enabled) {
    const aiResult = document.createElement('div'); aiResult.className = 'evidence';
    const analyze = cell('button', `AI 辅助分析 · ${aiStatus.model} · 每日最多 ${aiStatus.dailyLimit ?? 20} 次`, 'ghost small-button write-action');
    analyze.addEventListener('click', async () => {
      analyze.disabled = true;
      aiResult.replaceChildren(cell('p', '正在分析当前事件的脱敏证据…'));
      try {
        const { result } = await api(`/api/events/${event.id}/ai-summary`, 'POST');
        aiResult.replaceChildren(cell('h4', 'AI 建议 · 待人工核验'),
          cell('p', result.summary), cell('p', `证据 ID：${result.evidenceIds.join('、') || '未引用'}`),
          cell('p', `不确定性：${result.uncertainty || '模型未提供'}`));
        for (const check of result.suggestedChecks) aiResult.append(cell('p', `待检查：${check}`));
        aiResult.append(cell('small', result.notice));
      } catch (error) { aiResult.replaceChildren(cell('p', error.message, 'warning')); }
      finally { analyze.disabled = false; }
    });
    target.append(analyze, aiResult);
  }
  if (event.acknowledgedAt) target.append(cell('p', `管理员已于 ${formatTime(event.acknowledgedAt)} 确认。`));
  else if (event.status === 'open') {
    const acknowledge = cell('button', '确认事件', 'ghost small-button write-action');
    acknowledge.addEventListener('click', async () => {
      try { await api(`/api/events/${event.id}/ack`, 'POST'); await showEvent(event.id); await refresh(); }
      catch (error) { notice(error.message, true); }
    });
    target.append(acknowledge);
  }
  const responseForm = document.createElement('form'); responseForm.className = 'response-form';
  responseForm.append(cell('h4', '处置与恢复验证'));
  const phaseLabel = cell('label', '记录类型');
  const phase = document.createElement('select');
  for (const [value, label] of [['diagnosis', '诊断记录'], ['mitigation', '处置记录'],
    ['recovery_verification', '恢复验证']]) {
    const option = document.createElement('option'); option.value = value; option.textContent = label;
    if (value === 'recovery_verification' && event.status !== 'resolved') option.disabled = true;
    phase.append(option);
  }
  phaseLabel.append(phase);
  const noteLabel = cell('label', '处理记录（5–500 字）');
  const note = document.createElement('textarea'); note.required = true; note.minLength = 5; note.maxLength = 500;
  note.rows = 3; note.placeholder = '记录核查、处置或恢复验证依据'; noteLabel.append(note);
  const sampleLabel = cell('label', '成功采样 ID');
  const sample = document.createElement('input'); sample.placeholder = '恢复后成功采样的 UUID';
  sample.value = [...event.evidence].reverse().find(item =>
    ['collection_recovered', 'alert_recovered'].includes(item.kind) && item.payload?.sampleId)?.payload.sampleId || '';
  sampleLabel.append(sample);
  const syncPhase = () => { sampleLabel.hidden = phase.value !== 'recovery_verification';
    sample.required = phase.value === 'recovery_verification'; };
  phase.addEventListener('change', syncPhase); syncPhase();
  const saveResponse = cell('button', '保存到事件时间线'); saveResponse.type = 'submit';
  responseForm.append(phaseLabel, noteLabel, sampleLabel, saveResponse);
  responseForm.addEventListener('submit', async formEvent => {
    formEvent.preventDefault();
    try {
      await api(`/api/events/${event.id}/response`, 'POST', {
        phase: phase.value, note: note.value.trim(), sampleId: sampleLabel.hidden ? '' : sample.value.trim()
      });
      notice('处理记录已保存到当前事件。'); await showEvent(event.id); await refresh();
    } catch (error) { notice(error.message, true); }
  });
  target.append(responseForm);
  for (const evidence of event.evidence) {
    let text = evidence.payload.error || evidence.payload.sampleId || '';
    if (evidence.kind === 'k8s_state') text = k8sStateText(evidence.payload);
    if (evidence.kind === 'root_cause_candidate') text = `${evidence.payload.hypothesis} · 置信度 ${evidence.payload.confidence}。${evidence.payload.limitation}`;
    if (evidence.kind === 'linux_state') text = evidence.payload.status === 'ok'
      ? `Linux 内存已用 ${evidence.payload.memory?.usedPercent ?? '未知'}%；负载 ${evidence.payload.cpu?.load1 ?? '未知'}`
      : evidence.payload.reason || evidence.payload.status;
    if (evidence.kind === 'pgbouncer_state') text = evidence.payload.status === 'ok'
      ? `${evidence.payload.pools?.length || 0} 个连接池；等待客户端 ${(evidence.payload.pools || []).reduce((sum, pool) => sum + pool.clientWaiting, 0)}`
      : evidence.payload.reason || evidence.payload.status;
    if (evidence.kind.startsWith('alert_')) text = `${evidence.payload.ruleId || '规则'} · ${evidence.payload.source || '未知来源'} · ${formatAlertValue(evidence.payload.metric, evidence.payload.value)} ${evidence.payload.operator || ''} ${formatAlertValue(evidence.payload.metric, evidence.payload.threshold)}${evidence.payload.reason ? ' · ' + evidence.payload.reason : ''}${evidence.payload.silenceId ? ' · 静默窗 ' + evidence.payload.silenceId : ''}`;
    if (evidence.kind === 'event_acknowledged') text = `${evidence.payload.actor || '管理员'} 已确认事件`;
    if (evidence.kind === 'rca_decision') text = `${evidence.payload.actor || '管理员'} ${evidence.payload.decision === 'confirmed' ? '确认' : '驳回'} ${evidence.payload.code}：${evidence.payload.reason}`;
    if (evidence.kind === 'ai_invocation') text = `${evidence.payload.actor === 'auto' ? '自动' : '手动'} · ${evidence.payload.model || '模型'} ${evidence.payload.status === 'success' ? '已给出待核验建议' : evidence.payload.status === 'pending' ? '分析中' : '分析失败'} · 引用证据 ID：${evidence.payload.evidenceIds?.join('、') || '无'}`;
    if (evidence.kind === 'ai_advisory') text = `AI 待核验建议：${evidence.payload.summary || ''} · 不确定性：${evidence.payload.uncertainty || ''} · 待检查：${evidence.payload.suggestedChecks?.join('；') || '无'} · 证据 ID：${evidence.payload.evidenceIds?.join('、') || '无'}`;
    if (evidence.kind === 'operator_response' || evidence.kind === 'recovery_verified') text =
      `${evidence.payload.actor || '管理员'} · ${evidence.payload.phase === 'diagnosis' ? '诊断' : evidence.payload.phase === 'mitigation' ? '处置' : '恢复验证'} · ${evidence.payload.note}${evidence.payload.sampleId ? ` · 成功采样 ${evidence.payload.sampleId}` : ''}`;
    target.append(cell('p', `${formatTime(evidence.observedAt)} · ${evidence.kind} · ${text}`, 'evidence'));
  }
}

async function boot() {
  const session = await api('/api/session');
  csrf = session.csrf;
  currentRole = session.authenticated ? (session.role || 'admin') : null;
  viewerScoped = session.authenticated && session.viewerScoped === true;
  document.body.dataset.role = currentRole || '';
  $('role-label').textContent = currentRole === 'viewer' ? '只读查看' : '管理员';
  $('role-label').hidden = !session.authenticated;
  $('login-panel').hidden = session.authenticated;
  $('workspace').hidden = !session.authenticated;
  $('logout').hidden = !session.authenticated;
  if (session.authenticated) { setView(location.hash.slice(1), false); await refresh(); }
}

for (const button of document.querySelectorAll('[data-nav]')) button.addEventListener('click', () => setView(button.dataset.nav));
for (const button of document.querySelectorAll('[data-open-view]')) button.addEventListener('click', () => setView(button.dataset.openView));
window.addEventListener('hashchange', () => setView(location.hash.slice(1), false));

$('login-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  try { const data = await api('/api/login', 'POST', { password: new FormData(event.target).get('password') }); csrf = data.csrf; await boot(); }
  catch (e) { alert(e.message); }
});

$('logout').addEventListener('click', async () => { await api('/api/logout', 'POST'); csrf = null; currentRole = null; selectedAsset = null; await boot(); });
$('refresh').addEventListener('click', () => refresh().catch(e => notice(e.message, true)));
$('inspection-asset').addEventListener('change', () => refresh().catch(error => notice(error.message, true)));
$('inspection-run').addEventListener('click', async () => {
  const form = $('inspection-form');
  if (!form.elements.assetId.value || !form.elements.templateId.value) return notice('请先添加资产。', true);
  try {
    const { run } = await api('/api/inspections/run', 'POST', { assetId: form.elements.assetId.value,
      templateId: form.elements.templateId.value });
    notice(run.status === 'failed' ? run.error : '巡检已完成，结果已归档。', run.status === 'failed'); await refresh();
  } catch (error) { notice(error.message, true); }
});
$('inspection-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = event.target;
  try {
    await api('/api/inspection-schedules', 'PUT', { assetId: form.elements.assetId.value,
      templateId: form.elements.templateId.value, intervalMinutes: Number(form.elements.intervalMinutes.value),
      enabled: form.elements.enabled.checked });
    notice('巡检计划已保存。'); await refresh();
  } catch (error) { notice(error.message, true); }
});
$('cancel-edit').addEventListener('click', resetEdit);
$('silence-form').addEventListener('submit', async event => {
  event.preventDefault();
  const data = Object.fromEntries(new FormData(event.target));
  const minutes = Number(data.durationMinutes);
  const payload = { kind: data.kind, assetId: data.assetId || null, ruleId: data.ruleId || null,
    reason: data.reason.trim(), endsAt: new Date(Date.now() + minutes * 60000).toISOString() };
  try {
    await api('/api/silences', 'POST', payload);
    event.target.elements.reason.value = '';
    notice('时间窗已创建；下次采样将标记受抑制的告警。');
    await refresh();
  } catch (error) { notice(error.message, true); }
});
$('runtime').addEventListener('change', (event) => {
  const k8s = event.target.value === 'k8s';
  $('k8s-fields').hidden = !k8s;
  for (const field of document.querySelectorAll('.linux-field')) field.hidden = k8s;
  $('asset-form').elements.k8sCluster.required = k8s;
  $('asset-form').elements.namespace.required = k8s;
});
$('discover-k8s').addEventListener('click', async () => {
  const form = $('asset-form');
  const cluster = form.elements.k8sCluster.value.trim();
  const namespace = form.elements.namespace.value.trim();
  const target = $('discovery-results');
  target.replaceChildren(cell('p', '正在查询 Kubernetes API…'));
  try {
    const { discovery } = await api(`/api/k8s/discover?cluster=${encodeURIComponent(cluster)}&namespace=${encodeURIComponent(namespace)}`);
    target.replaceChildren();
    if (discovery.status !== 'ok') { target.append(cell('p', discovery.reason || discovery.errors?.pods || discovery.status, 'warning')); return; }
    const candidates = discovery.pods.filter(pod => pod.postgresLikely);
    target.append(cell('p', `发现 ${discovery.pods.length} 个 Pod，其中 ${candidates.length} 个可能运行 PostgreSQL。请核对后选择。`));
    for (const pod of candidates) {
      const button = cell('button', `${pod.name} · ${pod.workload?.kind || 'Pod'}/${pod.workload?.name || '未关联'} · ${pod.services.join('、') || '无 Service'}`, 'ghost');
      button.type = 'button';
      button.addEventListener('click', () => {
        form.elements.pod.value = pod.name;
        form.elements.workload.value = pod.workload?.name || '';
        form.elements.node.value = pod.node || '';
        notice(`已填入 ${pod.name} 的 K8S 映射；请填写从 DbSphere 可达的 PostgreSQL 地址和凭据。`);
      });
      target.append(button);
    }
    for (const [resource, error] of Object.entries(discovery.errors || {})) target.append(cell('p', `${resource} 未采集：${error}`, 'warning'));
    for (const resource of Object.keys(discovery.truncated || {})) target.append(cell('p', `${resource} 超过 100 项，发现结果不完整。`, 'warning'));
  } catch (error) { target.replaceChildren(cell('p', error.message, 'warning')); }
});
$('asset-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const data = Object.fromEntries(new FormData(event.target));
  data.port = Number(data.port);
  data.poolerClear = data.poolerClear === 'on';
  try {
    await api(editingAsset ? `/api/assets/${editingAsset}` : '/api/assets', editingAsset ? 'PATCH' : 'POST', data);
    resetEdit(); notice('资产已保存。点击“采集”验证连接。'); await refresh();
  }
  catch (e) { notice(e.message, true); }
});

boot().catch(e => { $('login-panel').hidden = false; alert(e.message); });
setInterval(() => { if (csrf) refresh().catch(() => {}); }, 30000);
