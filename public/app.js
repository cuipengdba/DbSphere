let csrf = null;
let selectedAsset = null;
let editingAsset = null;
const $ = (id) => document.getElementById(id);
const formatTime = (value) => value ? new Date(value).toLocaleString('zh-CN') : '尚无数据';

async function api(path, method = 'GET', payload) {
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
  const [{ assets }, { events }, { alerts }, { rules }, { silences }] = await Promise.all([
    api('/api/assets'), api('/api/events'), api('/api/alerts'), api('/api/alert-rules'), api('/api/silences')
  ]);
  $('asset-count').textContent = assets.length;
  $('healthy-count').textContent = assets.filter(a => a.lastStatus === 'ok').length;
  $('event-count').textContent = events.filter(e => e.status === 'open').length;
  renderAssets(assets);
  renderEvents(events);
  renderAlerts(alerts);
  renderSilenceOptions(assets, rules);
  renderSilences(silences, assets, rules);
  if (!document.activeElement || !$('rule-list').contains(document.activeElement)) renderRules(rules);
  if (selectedAsset) await showAsset(selectedAsset);
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
    const view = cell('button', '查看', 'ghost small-button'); view.addEventListener('click', () => showAsset(asset.id));
    const edit = cell('button', '编辑', 'ghost small-button'); edit.addEventListener('click', () => editAsset(asset));
    const check = cell('button', '采集', 'ghost small-button'); check.addEventListener('click', async () => {
      try { await api(`/api/assets/${asset.id}/check`, 'POST'); notice(`${asset.name} 采集完成`); await refresh(); }
      catch (e) { notice(e.message, true); }
    });
    const remove = cell('button', '删除', 'danger small-button'); remove.addEventListener('click', async () => {
      if (!confirm(`删除资产“${asset.name}”及其采样和事件？`)) return;
      try { await api(`/api/assets/${asset.id}`, 'DELETE'); if (selectedAsset === asset.id) { selectedAsset = null; $('detail').textContent = '选择资产查看最新采样。'; } await refresh(); }
      catch (e) { notice(e.message, true); }
    });
    actions.append(view, edit, check, remove); item.append(info, actions); list.append(item);
  }
}

async function showAsset(id) {
  selectedAsset = id;
  const [{ asset }, { samples }] = await Promise.all([api(`/api/assets/${id}`), api(`/api/assets/${id}/samples`)]);
  const detail = $('detail'); detail.replaceChildren();
  detail.append(cell('h3', asset.name));
  detail.append(cell('p', `${asset.host}:${asset.port}/${asset.database} · ${asset.runtime === 'k8s' ? `${asset.k8sCluster}/${asset.namespace}/${asset.pod || asset.workload || '未映射'}` : asset.linuxHost || 'Linux 主机未关联'}`));
  if (asset.pg14Eol) detail.append(cell('p', 'PostgreSQL 14 已结束上游支持，请规划升级。', 'warning'));
  detail.append(cell('p', `PgBouncer：${asset.poolerConfigured ? '已配置只读统计连接' : '未配置'}`));
  detail.append(cell('p', `采集状态：${asset.lastStatus} · ${formatTime(asset.lastSampleAt)}${asset.lastError ? ' · ' + asset.lastError : ''}`));
  const layerNav = document.createElement('div'); layerNav.className = 'actions layer-nav';
  for (const [label, section] of [
    ['PostgreSQL 诊断', 'pg-diagnostics'],
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
  if (currentSample?.metrics?.k8s) {
    const k8s = currentSample.metrics.k8s;
    const heading = cell('h3', 'Kubernetes 运行状态'); heading.id = 'k8s-detail'; detail.append(heading);
    if (k8s.status === 'ok') {
      detail.append(cell('p', `Pod ${k8s.pod.name} · ${k8s.pod.phase || '未知'} · Node ${k8s.pod.node || '未映射'}`));
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

function renderEvents(events) {
  const list = $('event-list'); list.replaceChildren();
  if (!events.length) { list.append(cell('p', '暂无问题事件。', 'empty')); return; }
  for (const event of events) {
    const button = cell('button', `${event.status === 'open' ? '● ' : '✓ '}${event.assetName} · ${event.title} · ${formatTime(event.startedAt)}`, 'event-button');
    button.addEventListener('click', () => showEvent(event.id)); list.append(button);
  }
}

function renderAlerts(alerts) {
  const list = $('alert-list'); list.replaceChildren();
  if (!alerts.length) { list.append(cell('p', '当前没有触发中的告警。', 'empty')); return; }
  for (const alert of alerts) {
    const button = cell('button', `${alert.assetName} · ${alert.rule.title} · ${alert.rule.severity} · ${alert.state.lastValue ?? '信号缺失'}${alert.state.missingSince ? ' · 指标已失联' : ''}${alert.state.mutedBy ? ' · 通知已静默' : ''}`, 'event-button');
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
      const revoke = cell('button', '撤销', 'danger small-button');
      revoke.addEventListener('click', async () => {
        try { await api(`/api/silences/${silence.id}`, 'DELETE'); notice('时间窗已撤销；下次采样将恢复通知。'); await refresh(); }
        catch (error) { notice(error.message, true); }
      });
      item.append(revoke);
    }
    list.append(item);
  }
}

function renderRules(rules) {
  const list = $('rule-list'); list.replaceChildren();
  for (const rule of rules) {
    const details = document.createElement('details'); details.className = 'rule-card';
    details.append(cell('summary', `${rule.enabled ? '启用' : '停用'} · ${rule.title} · ${rule.metric} ${rule.operator} ${rule.threshold} · ${rule.durationSeconds}s`));
    const form = document.createElement('form'); form.className = 'rule-form';
    const threshold = document.createElement('input'); threshold.type = 'number'; threshold.step = 'any'; threshold.min = '0'; threshold.value = rule.threshold; threshold.required = true;
    const duration = document.createElement('input'); duration.type = 'number'; duration.step = '1'; duration.min = '0'; duration.max = '86400'; duration.value = rule.durationSeconds; duration.required = true;
    const severity = document.createElement('select');
    for (const value of ['info', 'warning', 'critical']) {
      const option = document.createElement('option'); option.value = value; option.textContent = value;
      severity.append(option);
    }
    severity.value = rule.severity;
    const enabled = document.createElement('input'); enabled.type = 'checkbox'; enabled.checked = rule.enabled;
    for (const [label, input] of [['阈值', threshold], ['持续秒数', duration], ['级别', severity], ['启用', enabled]]) {
      const wrapper = document.createElement('label'); wrapper.append(cell('span', label), input); form.append(wrapper);
    }
    const save = cell('button', '保存规则', 'small-button'); save.type = 'submit'; form.append(save);
    form.addEventListener('submit', async event => {
      event.preventDefault();
      try {
        await api(`/api/alert-rules/${rule.id}`, 'PATCH', { threshold: Number(threshold.value),
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
  const { event } = await api(`/api/events/${id}`);
  const target = $('event-detail'); target.replaceChildren();
  target.append(cell('h3', `${event.title} · ${event.status === 'open' ? '处理中' : '已恢复'}`));
  target.append(cell('small', `事件 ID ${event.id} · ${formatTime(event.startedAt)} 至 ${formatTime(event.endedAt)}`));
  if (event.acknowledgedAt) target.append(cell('p', `管理员已于 ${formatTime(event.acknowledgedAt)} 确认。`));
  else if (event.status === 'open') {
    const acknowledge = cell('button', '确认事件', 'ghost small-button');
    acknowledge.addEventListener('click', async () => {
      try { await api(`/api/events/${event.id}/ack`, 'POST'); await showEvent(event.id); await refresh(); }
      catch (error) { notice(error.message, true); }
    });
    target.append(acknowledge);
  }
  for (const evidence of event.evidence) {
    let text = evidence.payload.error || evidence.payload.sampleId || '';
    if (evidence.kind === 'k8s_state') text = evidence.payload.status === 'ok'
      ? `Pod ${evidence.payload.pod?.name || '未知'} / ${evidence.payload.pod?.phase || '未知'}；${(evidence.payload.containers || []).map(c => `${c.name}: ${c.terminatedReason || '运行中'}`).join('，')}`
      : evidence.payload.reason || evidence.payload.status;
    if (evidence.kind === 'root_cause_candidate') text = `${evidence.payload.hypothesis} · 置信度 ${evidence.payload.confidence}。${evidence.payload.limitation}`;
    if (evidence.kind === 'linux_state') text = evidence.payload.status === 'ok'
      ? `Linux 内存已用 ${evidence.payload.memory?.usedPercent ?? '未知'}%；负载 ${evidence.payload.cpu?.load1 ?? '未知'}`
      : evidence.payload.reason || evidence.payload.status;
    if (evidence.kind === 'pgbouncer_state') text = evidence.payload.status === 'ok'
      ? `${evidence.payload.pools?.length || 0} 个连接池；等待客户端 ${(evidence.payload.pools || []).reduce((sum, pool) => sum + pool.clientWaiting, 0)}`
      : evidence.payload.reason || evidence.payload.status;
    if (evidence.kind.startsWith('alert_')) text = `${evidence.payload.ruleId || '规则'} · ${evidence.payload.source || '未知来源'} · ${evidence.payload.value ?? '信号缺失'} ${evidence.payload.operator || ''} ${evidence.payload.threshold ?? ''}${evidence.payload.reason ? ' · ' + evidence.payload.reason : ''}${evidence.payload.silenceId ? ' · 静默窗 ' + evidence.payload.silenceId : ''}`;
    if (evidence.kind === 'event_acknowledged') text = `${evidence.payload.actor || '管理员'} 已确认事件`;
    target.append(cell('p', `${formatTime(evidence.observedAt)} · ${evidence.kind} · ${text}`, 'evidence'));
  }
}

async function boot() {
  const session = await api('/api/session');
  csrf = session.csrf;
  $('login-panel').hidden = session.authenticated;
  $('workspace').hidden = !session.authenticated;
  $('logout').hidden = !session.authenticated;
  if (session.authenticated) await refresh();
}

$('login-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  try { const data = await api('/api/login', 'POST', { password: new FormData(event.target).get('password') }); csrf = data.csrf; await boot(); }
  catch (e) { alert(e.message); }
});

$('logout').addEventListener('click', async () => { await api('/api/logout', 'POST'); csrf = null; selectedAsset = null; await boot(); });
$('refresh').addEventListener('click', () => refresh().catch(e => notice(e.message, true)));
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
