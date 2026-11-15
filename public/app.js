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
  const [{ assets }, { events }] = await Promise.all([api('/api/assets'), api('/api/events')]);
  $('asset-count').textContent = assets.length;
  $('healthy-count').textContent = assets.filter(a => a.lastStatus === 'ok').length;
  $('event-count').textContent = events.filter(e => e.status === 'open').length;
  renderAssets(assets);
  renderEvents(events);
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
  detail.append(cell('p', `采集状态：${asset.lastStatus} · ${formatTime(asset.lastSampleAt)}${asset.lastError ? ' · ' + asset.lastError : ''}`));
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
  } else detail.append(cell('p', '当前配置尚无成功采样。', 'empty'));
  if (currentSample?.metrics?.k8s) {
    const k8s = currentSample.metrics.k8s;
    detail.append(cell('h3', 'Kubernetes 运行状态'));
    if (k8s.status === 'ok') {
      detail.append(cell('p', `Pod ${k8s.pod.name} · ${k8s.pod.phase || '未知'} · Node ${k8s.pod.node || '未映射'}`));
      for (const container of k8s.containers) detail.append(cell('p', `${container.name} · 重启 ${container.restartCount} 次${container.terminatedReason ? ' · 最近终止：' + container.terminatedReason : ''}`, container.terminatedReason === 'OOMKilled' ? 'warning' : ''));
      for (const pvc of k8s.pvcs) detail.append(cell('p', `PVC ${pvc.name} · ${pvc.phase || pvc.error || '未知'} · ${pvc.capacity || '容量未知'}`));
      for (const event of k8s.events) detail.append(cell('p', `${formatTime(event.at)} · ${event.reason || 'Event'} · ${event.message || ''}`, 'evidence'));
      if (k8s.eventsError) detail.append(cell('p', `K8S Event 未采集：${k8s.eventsError}`, 'warning'));
    } else detail.append(cell('p', `K8S 未采集：${k8s.reason || k8s.status}`, 'warning'));
  }
}

function renderEvents(events) {
  const list = $('event-list'); list.replaceChildren();
  if (!events.length) { list.append(cell('p', '暂无问题事件。', 'empty')); return; }
  for (const event of events) {
    const button = cell('button', `${event.status === 'open' ? '● ' : '✓ '}${event.assetName} · ${event.title} · ${formatTime(event.startedAt)}`, 'event-button');
    button.addEventListener('click', () => showEvent(event.id)); list.append(button);
  }
}

function editAsset(asset) {
  editingAsset = asset.id;
  const form = $('asset-form');
  for (const [field, value] of Object.entries({
    name: asset.name, host: asset.host, port: asset.port, database: asset.database,
    username: asset.username, sslMode: asset.sslMode, runtime: asset.runtime,
    linuxHost: asset.linuxHost, k8sCluster: asset.k8sCluster, namespace: asset.namespace,
    workload: asset.workload, pod: asset.pod, node: asset.node
  })) form.elements[field].value = value ?? '';
  form.elements.password.value = '';
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
  for (const evidence of event.evidence) {
    let text = evidence.payload.error || evidence.payload.sampleId || '';
    if (evidence.kind === 'k8s_state') text = evidence.payload.status === 'ok'
      ? `Pod ${evidence.payload.pod?.name || '未知'} / ${evidence.payload.pod?.phase || '未知'}；${(evidence.payload.containers || []).map(c => `${c.name}: ${c.terminatedReason || '运行中'}`).join('，')}`
      : evidence.payload.reason || evidence.payload.status;
    if (evidence.kind === 'root_cause_candidate') text = `${evidence.payload.hypothesis} · 置信度 ${evidence.payload.confidence}。${evidence.payload.limitation}`;
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
$('runtime').addEventListener('change', (event) => {
  const k8s = event.target.value === 'k8s';
  $('k8s-fields').hidden = !k8s;
  document.querySelector('.linux-field').hidden = k8s;
  $('asset-form').elements.k8sCluster.required = k8s;
  $('asset-form').elements.namespace.required = k8s;
});
$('asset-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  const data = Object.fromEntries(new FormData(event.target));
  data.port = Number(data.port);
  try {
    await api(editingAsset ? `/api/assets/${editingAsset}` : '/api/assets', editingAsset ? 'PATCH' : 'POST', data);
    resetEdit(); notice('资产已保存。点击“采集”验证连接。'); await refresh();
  }
  catch (e) { notice(e.message, true); }
});

boot().catch(e => { $('login-panel').hidden = false; alert(e.message); });
setInterval(() => { if (csrf) refresh().catch(() => {}); }, 30000);
