let csrf = null;
let currentRole = null;
let currentUsername = '';
let currentOrganization = '';
let oidcEnabled = false;
let organizations = [];
let approvalEnabled = false;
let viewerScoped = false;
let selectedAsset = null;
let selectedAssetLayer = null;
let selectedAssetTrendHours = 24;
let selectedEvent = null;
let eventRequest = 0;
let eventSection = 'evidence';
let rcaWorkbenchRequest = 0;
const pageSections = { alerts: 'active', inspections: 'history', settings: 'storage' };
let recentEvents = [];
let allAssets = [];
let assetServerPaging = false;
let assetCursors = [''];
let assetPageIndex = 0;
let assetPageRequest = 0;
let assetSearchTimer = null;
let eventServerPaging = false;
let eventCursors = [''];
let eventPageIndex = 0;
let eventPageRequest = 0;
let eventSearchTimer = null;
let editingAsset = null;
let pg18Store = false;
let longTermStoreUnavailable = false;
let collectorSchedule = { known: false, enabled: false, intervalMs: 60000, clockOffsetMs: 0 };
const $ = (id) => document.getElementById(id);
const formatTime = (value) => value ? new Date(value).toLocaleString('zh-CN') : '尚无数据';
function assetHealth(asset) {
  if (!collectorSchedule.known) return asset.lastStatus === 'ok' ? { label: '正常', kind: 'ok' }
    : asset.lastStatus === 'error' ? { label: '采集失败', kind: 'error' } : { label: '待采集', kind: 'pending' };
  if (!collectorSchedule.enabled) return asset.lastStatus === 'ok' ? { label: '最近一次成功', kind: 'manual' }
    : asset.lastStatus === 'error' ? { label: '最近一次失败', kind: 'error' } : { label: '待采集', kind: 'pending' };
  const sampledAt = Date.parse(asset.lastSampleAt || asset.createdAt);
  if (!Number.isFinite(sampledAt) || Date.now() + collectorSchedule.clockOffsetMs - sampledAt >= 2 * collectorSchedule.intervalMs)
    return { label: asset.lastSampleAt ? '数据陈旧' : '采样未到达', kind: 'stale' };
  return asset.lastStatus === 'ok' ? { label: '近期正常', kind: 'ok' }
    : asset.lastStatus === 'error' ? { label: '采集失败', kind: 'error' } : { label: '待首采', kind: 'pending' };
}
const growthMetric = 'pg.database_growth_bytes_per_day';
const formatAlertValue = (metric, value) => value == null ? '信号缺失' :
  metric === growthMetric ? `${(Number(value) / (1024 ** 3)).toFixed(2)} GiB/日` : String(value);
const views = {
  overview: ['总览', '先看异常，再进入资产和证据。'],
  monitoring: ['监控工作台', '从集群健康进入实例趋势、等待分析和事件证据。'],
  incidents: ['问题事件', '一个问题，一个事件 ID，一条可追溯的证据链。'],
  assets: ['资产与实例', '纳管 PostgreSQL 14–19，并试用 MySQL 8.4 只读监控。'],
  alerts: ['告警与通知', '处理当前告警，管理规则、静默和投递。'],
  inspections: ['巡检与报告', '运行定期检查，查看历史结果和差异。'],
  settings: ['系统状态', '检查监控数据存储与部署状态。']
};

function setView(route, updateHash = true) {
  const [view, query] = route.split('?');
  if (view === 'shared' && currentRole) {
    const share = new URLSearchParams(query).get('id');
    if (/^[a-f0-9-]{36}$/.test(share || '')) {
      setView('incidents', false);
      void api(`/api/event-shares/${share}`).then(result => openEvent(result.eventId)).catch(error => notice(error.message, true));
      return;
    }
  }
  const selected = Object.hasOwn(views, view) ? view : 'overview';
  const params = new URLSearchParams(query);
  if (document.body.dataset.activeView !== selected) notice('');
  document.body.dataset.activeView = selected;
  if (selected !== 'assets' && $('asset-dialog').open) resetEdit();
  for (const section of document.querySelectorAll('[data-view]'))
    section.hidden = section.dataset.view !== selected ||
      ((section.id === 'admin-audit-panel' || section.id === 'collector-status-panel') && currentRole !== 'admin');
  for (const button of document.querySelectorAll('[data-nav]')) {
    const active = button.dataset.nav === selected;
    button.classList.toggle('active', active);
    if (active) button.setAttribute('aria-current', 'page');
    else button.removeAttribute('aria-current');
  }
  $('view-title').textContent = views[selected][0];
  $('view-description').textContent = views[selected][1];
  if (selected === 'monitoring' && !updateHash) monitoring.restore(params);
  if (pageSections[selected]) {
    const section = params.get('section');
    if (selected === 'alerts' && section === 'rca' && currentRole === 'admin') {
      showSection(selected, section, params.get('event'), params.get('rule'));
    } else {
      showSection(selected, selected === 'settings' && ['storage', 'collector', 'platform', 'organizations', 'access', 'audit', 'history-tiers'].includes(section) ? section : pageSections[selected]);
    }
  }
  monitoring.setVisible(selected === 'monitoring');
  if (selected === 'overview' && assetServerPaging) void loadFleetOverview();
  if (updateHash) {
    const next = selected === 'monitoring' ? monitoring.route() : query ? route : selected;
    if (location.hash !== `#${next}`) history.pushState(null, '', `#${next}`);
  }
  window.scrollTo({ top: 0, behavior: 'instant' });
  if (selected === 'incidents' && !updateHash) {
    const id = params.get('event');
    if (/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(id || '')) void openEvent(id, false);
  }
}

function showSection(view, section, eventId, ruleCode) {
  if (view === 'settings' && currentRole !== 'admin') section = 'storage';
  if (view === 'settings' && currentOrganization && ['collector', 'platform'].includes(section)) section = 'organizations';
  if (view === 'alerts' && section === 'rca' && (currentRole !== 'admin' || currentOrganization)) section = 'active';
  pageSections[view] = section;
  for (const button of document.querySelectorAll(`[data-view="${view}"] [data-section]`)) {
    button.setAttribute('aria-pressed', String(button.dataset.section === section));
    if (button.classList.contains('admin-only')) button.hidden = currentRole !== 'admin';
  }
  for (const panel of document.querySelectorAll(`[data-view="${view}"][data-section-panel], [data-view="${view}"] [data-section-panel]`)) {
    panel.hidden = panel.dataset.sectionPanel !== section || (panel.classList.contains('admin-only') && currentRole !== 'admin');
  }
  if (view === 'alerts' && section === 'rca') void renderRCAWorkbench(eventId, ruleCode);
  if (view === 'settings' && section === 'access') void loadAccessDirectory();
  if (view === 'settings' && section === 'platform') void loadPlatformStatus();
  if (view === 'settings' && section === 'organizations') void loadOrganizations();
  if (view === 'settings' && section === 'audit') renderOrganizationAudit();
  if (view === 'settings' && section === 'history-tiers') void loadHistoryPolicies();
}

const accessRoleLabels = { admin: '管理员', operator: '值班员', viewer: '只读查看', approver: '独立审批' };
const workActionLabels = { collect_once: '只读采样', inspect_once: '运行巡检', verify_restore: '隔离恢复核验', analyze_relation: '单表 ANALYZE' };
const workErrorLabels = { execution_timeout: '达到执行期限', restore_marker_mismatch: '恢复数据标记不匹配',
  execution_profile_unavailable: '执行配置已变更或不可用', execution_failed: '执行失败' };

function accessField(form, label, field) {
  const container = cell('label', label);
  container.append(field);
  form.append(container);
  return field;
}

function accessInput(form, label, name, value, attributes = {}) {
  const field = Object.assign(document.createElement('input'), { name, value }, attributes);
  return accessField(form, label, field);
}

function accessSelect(form, label, name, entries, selected, multiple = false) {
  const field = Object.assign(document.createElement('select'), { name, multiple });
  for (const [value, title] of entries) {
    const isSelected = selected.includes(value);
    field.add(new Option(title, value, isSelected, isSelected));
  }
  return accessField(form, label, field);
}

function accessForm(title, parent, existing, save) {
  const section = cell(existing ? 'details' : 'div', '', 'settings-details');
  section.append(cell(existing ? 'summary' : 'h4', title));
  const form = cell('form', '', 'response-form');
  section.append(form);
  parent.append(section);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    const button = form.querySelector('button[type="submit"]');
    button.disabled = true;
    try {
      await save(new FormData(form));
      form.reset();
      notice('已保存账号或团队设置。');
      await loadAccessDirectory();
    } catch (error) {
      notice(error.message, true);
      button.disabled = false;
    }
  });
  return form;
}

const defaultOrganizationID = '00000000-0000-4000-8000-000000000001';
const organizationOptions = () => organizations.map(organization => [organization.id, organization.name]);
const organizationName = id => organizations.find(organization => organization.id === (id || defaultOrganizationID))?.name || '默认组织';

async function loadOrganizations(render = true) {
  const target = $('organization-list');
  try {
    const response = await api('/api/admin/organizations');
    organizations = response.organizations || [];
    if (render) target.replaceChildren(...organizations.map(item => cell('p', `${item.name} · ${item.id}`, 'evidence')));
  } catch (error) {
    if (render) target.replaceChildren(cell('p', error.message, 'warning'));
  }
}

function renderOrganizationAudit() {
  const target = $('organization-audit');
  target.replaceChildren();
  const controls = cell('div', '', 'section-actions');
  const select = document.createElement('select');
  select.setAttribute('aria-label', '审计组织');
  for (const [id, name] of organizationOptions()) select.add(new Option(name, id, id === defaultOrganizationID, id === defaultOrganizationID));
  if (!select.options.length) select.add(new Option('默认组织', currentOrganization || defaultOrganizationID));
  const download = cell('a', '导出可验证审计 JSON');
  const fingerprint = cell('p', '', 'hint');
  const auditPath = () => `/api/admin/organization-audit?organizationId=${encodeURIComponent(select.value)}`;
  let pending = 0;
  const update = async () => {
    const request = ++pending;
    download.href = auditPath();
    fingerprint.textContent = '正在读取审计完整性信息…';
    try {
      const proof = await api(auditPath());
      if (request === pending) fingerprint.textContent = `已核验 ${proof.headSequence} 条审计记录 · 验证公钥指纹 ${proof.keyFingerprint}`;
    } catch (error) { if (request === pending) fingerprint.textContent = error.message; }
  };
  select.addEventListener('change', update);
  controls.append(select, download);
  target.append(controls, fingerprint, cell('p', '使用发布包的 verify-audit 命令可离线核对签名与链条。', 'hint'));
  void update();
}

async function loadAccessDirectory() {
  const content = $('access-directory-content');
  let directory;
  try {
    directory = await api('/api/admin/access');
  } catch (error) {
    content.replaceChildren(cell('p', error.message, 'empty'));
    return;
  }
  const { accounts = [], teams = [] } = directory;
  if (directory.organizations) organizations = directory.organizations;
  const teamPanel = cell('div', '', 'list');
  teamPanel.append(cell('h3', '项目团队'));
  for (const team of [null, ...teams]) {
    const form = accessForm(team ? organizationName(team.organizationId) + ' · ' + team.project + ' / ' + team.name : '新建团队', teamPanel, !!team, data => {
      const rawIDs = data.get('assetIds').split(/[\s,，]+/).filter(Boolean);
      return api(team ? '/api/admin/teams/' + team.id : '/api/admin/teams', team ? 'PATCH' : 'POST', {
        project: data.get('project').trim(), name: data.get('name').trim(), assetIds: [...new Set(rawIDs)], organizationId: data.get('organizationId') || ''
      });
    });
    if (!team && !currentOrganization && organizations.length) accessSelect(form, '所属组织', 'organizationId', organizationOptions(), [defaultOrganizationID]);
    accessInput(form, '项目', 'project', team?.project || '', { required: true, maxLength: 80 });
    accessInput(form, '团队名称', 'name', team?.name || '', { required: true, maxLength: 80 });
    const ids = Object.assign(document.createElement('textarea'), { name: 'assetIds', rows: 2, value: (team?.assetIds || []).join('\n') });
    accessField(form, '资产 ID（空格、逗号或换行分隔）', ids);
    form.append(cell('button', team ? '保存团队' : '创建团队'));
    form.lastChild.type = 'submit';
  }
  const accountPanel = cell('div', '', 'list');
  accountPanel.append(cell('h3', '独立账号'));
  for (const account of [null, ...accounts]) {
    const form = accessForm(account ? account.username + ' · ' + accessRoleLabels[account.role] : '新建或迁移账号', accountPanel, !!account, data => {
      const update = { role: data.get('role'), teamIds: data.getAll('teamIds'), enabled: data.get('enabled') === 'on', organizationId: data.get('organizationId') || '' };
      if (oidcEnabled && (data.get('oidcSubject') || '') !== (account?.oidcSubject || '')) update.oidcSubject = data.get('oidcSubject') || '';
      if (account) return api('/api/admin/accounts/' + account.id, 'PATCH', update);
      return api('/api/admin/accounts', 'POST', { ...update, username: data.get('username').trim(), password: data.get('password'), legacyRole: data.get('legacyRole') });
    });
    if (!account) accessInput(form, '账号', 'username', '', { required: true, minLength: 3, maxLength: 48, pattern: '[a-z][a-z0-9_.\\-]{2,47}', autocomplete: 'off' });
    const organization = !account && !currentOrganization && organizations.length ?
      accessSelect(form, '所属组织', 'organizationId', [['', '默认范围（平台管理员全局）'], ...organizationOptions()], ['']) : null;
    accessSelect(form, '角色', 'role', Object.entries(accessRoleLabels), [account?.role || 'viewer']);
    const teamPicker = accessSelect(form, '团队授权', 'teamIds', [], [], true);
    const updateTeams = () => {
      const selected = account?.organizationId || currentOrganization || organization?.value || defaultOrganizationID;
      teamPicker.replaceChildren(...teams.filter(team => (team.organizationId || defaultOrganizationID) === selected).map(team => {
        const member = account?.teamIds?.includes(team.id) ?? false;
        return new Option(team.project + ' / ' + team.name, team.id, member, member);
      }));
    };
    organization?.addEventListener('change', updateTeams);
    updateTeams();
    if (oidcEnabled) accessInput(form, 'OIDC Subject（可选）', 'oidcSubject', account?.oidcSubject || '', { maxLength: 255, autocomplete: 'off' });
    accessInput(form, '启用账号', 'enabled', 'on', { type: 'checkbox', checked: account?.enabled ?? true });
    if (!account) {
      const password = accessInput(form, '初始密码（至少 12 字符）', 'password', '', { type: 'password', minLength: 12, maxLength: 256, autocomplete: 'new-password', required: true });
      if (!currentOrganization) {
        const legacy = accessSelect(form, '迁移初始凭据', 'legacyRole', [['', '创建新密码'], ['admin', '初始管理员'], ['viewer', '初始只读账号'], ['approver', '初始审批账号']], ['']);
        const updateLegacy = () => {
          legacy.disabled = !!organization?.value;
          if (legacy.disabled) legacy.value = '';
          password.disabled = !!legacy.value;
          password.required = !legacy.value;
          if (legacy.value) form.elements.role.value = legacy.value;
        };
        legacy.addEventListener('change', updateLegacy);
        organization?.addEventListener('change', updateLegacy);
      }
    }
    form.append(cell('button', account ? '保存账号' : '创建账号'));
    form.lastChild.type = 'submit';
    if (!account) continue;
    const rotate = accessForm('轮换凭据 · ' + account.username, accountPanel, true, data =>
      api('/api/admin/accounts/' + account.id + '/rotate', 'POST', { password: data.get('password') }));
    accessInput(rotate, '新密码（会撤销该账号已有会话）', 'password', '', { type: 'password', required: true, minLength: 12, maxLength: 256, autocomplete: 'new-password' });
    rotate.append(cell('button', '轮换密码'));
    rotate.lastChild.type = 'submit';
  }
  content.replaceChildren(teamPanel, accountPanel);
}

const rcaEvaluationLabels = {
  matched: '显示候选', no_candidate: '该事件未产生此候选', disabled: '规则已停用',
  evidence_below_minimum: '支持引用低于规则要求', onset_not_established: '首次观测时间未达到规则要求'
};

async function renderRCAWorkbench(eventId = '', preferredCode = '') {
  const target = $('rca-workbench');
  const request = ++rcaWorkbenchRequest;
  target.replaceChildren(cell('p', '正在读取根因规则…', 'hint'));
  try {
    const { rules } = await api('/api/rca/rules');
    if (request !== rcaWorkbenchRequest) return;
    const active = cell('div', '', 'form-grid rca-rule-picker');
    const picker = document.createElement('select');
    picker.setAttribute('aria-label', '根因规则');
    for (const rule of rules) {
      const option = cell('option', rule.title); option.value = rule.code; picker.append(option);
    }
    if (rules.some(rule => rule.code === preferredCode)) picker.value = preferredCode;
    const pickerField = cell('label', '规则'); pickerField.append(picker);
    const eventField = cell('label', '回放事件 ID');
    const eventInput = document.createElement('input');
    eventInput.setAttribute('aria-label', '回放事件 ID'); eventInput.maxLength = 36; eventInput.value = eventId || '';
    eventField.append(eventInput); active.append(pickerField, eventField);
    const editor = cell('div', '', 'list');
    target.replaceChildren(active, editor);
    let revisionRequest = 0;
    const showRule = async () => {
      const run = ++revisionRequest;
      const rule = rules.find(item => item.code === picker.value);
      editor.replaceChildren();
      if (!rule) return;
      editor.append(cell('p', `当前版本 v${rule.version} · 新版本用于保存后新发生的事件。`, 'hint'));
      const form = cell('form', '', 'form-grid routing-form');
      const field = (labelText, control) => {
        const label = cell('label', labelText);
        control.setAttribute('aria-label', labelText);
        label.append(control); form.append(label);
      };
      const enabled = document.createElement('input'); enabled.type = 'checkbox'; enabled.checked = rule.enabled;
      const onset = document.createElement('input'); onset.type = 'checkbox'; onset.checked = rule.requireOnset;
      const minimum = document.createElement('input'); minimum.type = 'number'; minimum.min = '0';
      minimum.max = '10'; minimum.required = true; minimum.value = rule.minEvidence;
      const reason = document.createElement('input'); reason.required = true; reason.maxLength = 180;
      field('启用规则', enabled); field('最少支持引用', minimum);
      field('要求首次观测不晚于事件开始', onset); field('版本变更原因', reason);
      const save = cell('button', '保存规则版本'); save.type = 'submit'; form.append(save);
      form.addEventListener('submit', async event => {
        event.preventDefault();
        save.disabled = true;
        try {
          const updated = await api(`/api/rca/rules/${rule.code}`, 'PATCH',
            { expectedVersion: rule.version, enabled: enabled.checked, minEvidence: Number(minimum.value),
              requireOnset: onset.checked, reason: reason.value });
          notice(`已保存根因规则 v${updated.version}。`);
          await renderRCAWorkbench(eventInput.value, rule.code);
        } catch (error) { notice(error.message, true); }
        finally { save.disabled = false; }
      });
      const replay = cell('button', '回放规则', 'ghost small-button'); replay.type = 'button';
      const replayResult = cell('div', '', 'list');
      replay.addEventListener('click', async () => {
        const id = eventInput.value.trim();
        if (!/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(id)) {
          notice('请填写有效的事件 ID。', true); return;
        }
        replay.disabled = true;
        try {
          const result = await api(`/api/events/${id}/rca-replay?rule=${encodeURIComponent(rule.code)}`);
          if (run !== revisionRequest || request !== rcaWorkbenchRequest) return;
          const title = cell('p', `${result.eventStatus === 'resolved' ? '已恢复事件' : '事件'} ${result.eventId}`, 'hint');
          replayResult.replaceChildren(title);
          for (const [label, evaluation] of [['事件开始时规则', result.historical], ['当前规则', result.current]]) {
            const row = cell('div', '', 'evidence');
            row.append(cell('strong', `${label} v${evaluation.version} · ${rcaEvaluationLabels[evaluation.outcome] || evaluation.outcome}`));
            const candidate = evaluation.candidate;
            if (candidate?.coverage) row.append(cell('p', `支持引用 ${candidate.coverage.supports} · 反证 ${candidate.coverage.against} · 待核对 ${candidate.coverage.missing}`));
            if (candidate?.timeOrder?.firstObservedAt) row.append(cell('p', `首次观测 ${formatTime(candidate.timeOrder.firstObservedAt)} · 事件开始 ${formatTime(result.eventStartedAt)}`));
            for (const evidence of candidate?.supports || []) {
              if (evidence.startsWith('sample:')) row.append(replaySampleLink(result.assetId, evidence.slice(7)));
            }
            for (const counter of candidate?.against || []) row.append(cell('p', counter, 'warning'));
            for (const gap of candidate?.missing || []) row.append(cell('p', gap, 'hint'));
            replayResult.append(row);
          }
          const link = cell('a', '返回原事件', 'monitor-link');
          link.href = `#incidents?event=${id}`; replayResult.append(link);
        } catch (error) { notice(error.message, true); }
        finally { replay.disabled = false; }
      });
      const history = cell('details', '', 'settings-details');
      history.append(cell('summary', '版本历史'));
      editor.append(form, history, replay, replayResult);
      try {
        const { versions } = await api(`/api/rca/rules/${rule.code}`);
        if (run !== revisionRequest || request !== rcaWorkbenchRequest) return;
        for (const version of versions) {
          const at = version.version === 1 ? '初始版本' : formatTime(version.effectiveAt);
          history.append(cell('p', `v${version.version} · ${at} · ${version.enabled ? '启用' : '停用'} · 最少支持引用 ${version.minEvidence} · ${version.reason}`, 'evidence'));
        }
      } catch (error) { if (run === revisionRequest) history.append(cell('p', error.message, 'warning')); }
    };
    picker.addEventListener('change', () => { void showRule(); });
    void showRule();
  } catch (error) {
    if (request === rcaWorkbenchRequest) target.replaceChildren(cell('p', error.message, 'warning'));
  }
}

function assetRuntimeLabel(asset) {
  if (asset.runtime === 'mysql') return 'MySQL 8.4 预览';
  if (asset.runtime === 'k8s') return `K8S${asset.namespace ? ' / ' + asset.namespace : ''}`;
  return asset.runtime === 'managed' ? '托管 PG' : 'Linux';
}

function renderOverview(assets, events) {
  const focus = $('overview-events'); focus.replaceChildren();
  const open = events.slice(0, 5);
  if (!open.length) focus.append(cell('p', '当前没有待处理事件。', 'empty'));
  for (const event of open) {
    const button = cell('button', '', 'event-button event-open overview-row');
    button.append(cell('strong', event.title), cell('small', `${event.assetName} · ${formatTime(event.startedAt)}`));
    button.addEventListener('click', () => openEvent(event.id));
    focus.append(button);
  }
  const fleet = $('overview-assets'); fleet.replaceChildren();
  fleet.hidden = assetServerPaging;
  $('overview-fleet').hidden = !assetServerPaging;
  if (assetServerPaging) {
    if (document.body.dataset.activeView === 'overview') void loadFleetOverview();
    return;
  }
  if (!assets.length) fleet.append(cell('p', '尚未纳管 PostgreSQL 实例。', 'empty'));
  const healthOrder = { error: 0, stale: 1, pending: 2, manual: 3, ok: 4 };
  const ordered = [...assets].sort((a, b) => healthOrder[assetHealth(a).kind] - healthOrder[assetHealth(b).kind]);
  for (const asset of ordered.slice(0, 6)) {
    const health = assetHealth(asset);
    const button = cell('button', '', `event-button overview-row health-${health.kind}`);
    button.append(cell('strong', asset.name), cell('small', `${assetRuntimeLabel(asset)} · ${health.label} · ${formatTime(asset.lastSampleAt)}`));
    button.addEventListener('click', () => {
      if (assetServerPaging) { monitoring.openInstance(asset.id); setView('monitoring'); }
      else { setView('assets'); void showAsset(asset.id); }
    });
    fleet.append(button);
  }
}

let fleetPage = 1, fleetOverviewRequest = 0;

async function loadFleetOverview() {
  const key = ++fleetOverviewRequest;
  const target = $('fleet-result');
  const caption = $('fleet-caption');
  const params = new URLSearchParams(new FormData($('fleet-filters')));
  params.set('page', String(fleetPage));
  target.setAttribute('aria-busy', 'true');
  try {
    const { clusters, summary, total } = await api(`/api/monitoring/fleet?${params}`);
    if (key !== fleetOverviewRequest || document.body.dataset.activeView !== 'overview') return;
    if (!clusters.length && fleetPage > 1) { fleetPage--; return loadFleetOverview(); }
    caption.textContent = `${summary.clusters} 个匹配集群 · ${summary.incidents} 个有异常 · ` +
      `${summary.observed}/${summary.members} 个成员采集成功 · ${summary.failed} 个失败 · ${summary.stale} 个过期 · ${summary.pending} 个待采集`;
    $('fleet-prev').disabled = fleetPage <= 1;
    $('fleet-next').disabled = fleetPage * 12 >= total;
    target.replaceChildren();
    if (!clusters.length) { target.append(cell('p', '没有匹配的集群。', 'empty')); return; }
    const grid = cell('table', '');
    grid.setAttribute('aria-label', '集群值班列表');
    const head = grid.createTHead().insertRow();
    for (const title of ['集群 / 运行形态', 'PG 可用', '采集', '当前异常', '开始时间']) {
      const heading = cell('th', title); heading.scope = 'col'; head.append(heading);
    }
    const body = grid.createTBody();
    const names = { linux: 'Linux', k8s: 'Kubernetes', managed: '托管 PG' };
    const statusLabels = { incident: '有异常', coverage: '采集不完整', healthy: '已观测健康' };
    for (const item of clusters) {
      const row = body.insertRow();
      const link = cell('button', item.cluster_name, 'monitor-link');
      link.type = 'button';
      link.addEventListener('click', () => { monitoring.openCluster(item.cluster_key); setView('monitoring'); });
      row.insertCell().append(link, cell('small', [item.runtimes.map(value => names[value]).join(' / '),
        item.versions?.length ? `PG ${item.versions.join('/')}` : 'PG 版本待采集'].join(' · '), 'fleet-row-meta'));
      row.insertCell().textContent = `${item.available} / ${item.members}`;
      row.insertCell().append(cell('span', `${item.observed} / ${item.members}`),
        cell('small', statusLabels[item.status], `fleet-row-meta fleet-${item.status}`));
      const affected = row.insertCell();
      if (item.affected) {
        const member = cell('button', item.affected.name, 'monitor-link');
        member.type = 'button';
        member.addEventListener('click', () => { monitoring.openInstance(item.affected.id, 'waits'); setView('monitoring'); });
        affected.append(member);
      }
      if (item.first_event) {
        const event = cell('button', `${item.events} 个事件`, 'monitor-link fleet-row-meta');
        event.addEventListener('click', () => openEvent(item.first_event)); affected.append(event);
      }
      if (!affected.childNodes.length) affected.textContent = '—';
      const began = row.insertCell();
      began.textContent = item.started_at ? formatTime(item.started_at) : '—';
      if (item.started_at) {
        const elapsed = Math.max(0, Date.now() - Date.parse(item.started_at));
        began.append(cell('small', elapsed < 60000 ? '不足 1 分钟' : elapsed < 3600000 ?
          `${Math.floor(elapsed / 60000)} 分钟` : `${(elapsed / 3600000).toFixed(1)} 小时`, 'fleet-row-meta'));
      }
    }
    target.append(grid);
  } catch (error) {
    if (key !== fleetOverviewRequest) return;
    caption.textContent = `集群摘要读取失败 · ${error.message}`;
    target.replaceChildren();
  } finally {
    if (key === fleetOverviewRequest) target.removeAttribute('aria-busy');
  }
}

async function api(path, method = 'GET', payload) {
  if (currentRole === 'viewer' && method !== 'GET' && path !== '/api/logout')
    throw new Error('只读账号不能修改监控数据。');
  if (currentRole === 'approver' && method !== 'GET' && path !== '/api/logout' &&
      !(method === 'POST' && /^\/api\/work-orders\/[0-9a-f-]+\/(approve|reject)$/.test(path)))
    throw new Error('审批账号只能审批事件工单。');
  const response = await fetch(path, {
    method, credentials: 'same-origin',
    headers: { ...(payload ? { 'Content-Type': 'application/json' } : {}), ...(method !== 'GET' && csrf ? { 'X-CSRF-Token': csrf } : {}) },
    body: payload ? JSON.stringify(payload) : undefined
  });
  if (response.headers.get('X-DbSphere-Storage') === 'unavailable') showStorageAvailability(false);
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || `HTTP ${response.status}`);
  return data;
}

function showStorageAvailability(available) {
  longTermStoreUnavailable = !available;
  $('storage-unavailable').hidden = available;
}

async function pollStorageAvailability() {
  try {
    const ready = await fetch('/readyz', { cache: 'no-store', signal: AbortSignal.timeout(6000) });
    if (ready.status === 404) return;
    const wasUnavailable = longTermStoreUnavailable;
    showStorageAvailability(ready.ok);
    if (ready.ok && wasUnavailable) {
      $('login-error').textContent = '';
      await boot();
    }
  } catch {
    showStorageAvailability(false);
  } finally {
    window.setTimeout(pollStorageAvailability, 5000);
  }
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

const monitoring = window.DbSphereMonitoring.create($('monitoring-workspace'), {
  api, assetHealth, assetRuntimeLabel, formatTime, openEvent,
  canCaptureWaits: () => currentRole === 'admin',
  canRecordChanges: () => currentRole === 'admin',
  openAssets: () => setView('assets'),
  openPlans: id => {
    setView('assets');
    showAsset(id, 'sql-plans').then(() => $('detail').scrollIntoView({ block: 'start' }))
      .catch(error => notice(error.message, true));
  },
  renderSqlPerformance, routingOverview
});

async function refresh() {
  const [{ assets }, { events }, { alerts }, { rules }, { silences }, { notifications }, { storage },
    { templates }, { schedules }, { runs }, audit, runtime, collector, workbench] = await Promise.all([
    api('/api/assets'), api('/api/events'), api('/api/alerts'), api('/api/alert-rules'),
    api('/api/silences'), api('/api/notifications'), viewerScoped ? Promise.resolve({ storage: { mode: 'scoped' } }) : api('/api/storage'),
    api('/api/inspection-templates'), api('/api/inspection-schedules'), api('/api/inspection-runs'),
    currentRole === 'admin' && !currentOrganization ? api('/api/admin/audit').catch(() => null) : Promise.resolve(null),
    api('/api/version').catch(() => null),
    currentRole === 'admin' && !currentOrganization ? api('/api/admin/collector').catch(() => null) : Promise.resolve(null),
    api('/api/alert-workbench').catch(() => null)
  ]);
  const interval = Number(runtime?.collectionIntervalSeconds);
  const serverTime = Date.parse(runtime?.serverTime);
  if (runtime?.version) {
    const preview = runtime.version.includes('-');
    $('release-label').textContent = `V${runtime.version} · ${runtime.backend === 'go' ? 'Go' : 'Node.js'} ${preview ? '开发预览' : '正式版'}`;
    $('edition-label').textContent = preview ? '开发预览' : `V${runtime.version} 正式版`;
  }
  collectorSchedule = {
    known: runtime?.backend === 'go' && typeof runtime.collectorEnabled === 'boolean',
    enabled: runtime?.collectorEnabled === true,
    intervalMs: Number.isFinite(interval) && interval > 0 ? interval * 1000 : 60000,
    clockOffsetMs: Number.isFinite(serverTime) ? serverTime - Date.now() : 0
  };
  const eventOverview = runtime?.backend === 'go' ? await api('/api/events/overview') :
    { openCount: events.filter(event => event.status === 'open').length,
      latestOpen: events.filter(event => event.status === 'open').slice(0, 5) };
  $('asset-count').textContent = assets.length;
  $('healthy-label').textContent = collectorSchedule.known && !collectorSchedule.enabled ? '最近一次成功' : '采集正常';
  $('healthy-count').textContent = assets.filter(a => assetHealth(a).kind === (collectorSchedule.known && !collectorSchedule.enabled ? 'manual' : 'ok')).length;
  $('event-count').textContent = eventOverview.openCount;
  $('event-count').closest('.stat').classList.toggle('alerting', eventOverview.openCount > 0);
  allAssets = assets;
  assetServerPaging = runtime?.backend === 'go';
  monitoring.sync(assets.filter(asset => asset.runtime !== 'mysql'), assetServerPaging,
    [...new Map([...eventOverview.latestOpen, ...events].map(event => [event.id, event])).values()]);
  renderOverview(assets, eventOverview.latestOpen);
  $('asset-page-controls').hidden = !assetServerPaging;
  if (assetServerPaging) await loadAssetPage();
  else renderLegacyAssets();
  eventServerPaging = runtime?.backend === 'go';
  $('event-page-controls').hidden = !eventServerPaging;
  if (eventServerPaging) await loadEventPage();
  else renderLegacyEvents(events);
  renderAlerts(alerts, workbench?.incidents);
  renderSilenceOptions(assets, rules);
  renderSilences(silences, assets, rules);
  renderNotifications(notifications);
  pg18Store = storage.mode !== 'sqlite-preview';
  renderStorage(storage);
  if (currentRole === 'admin') {
    renderAdminAudit(audit?.entries);
    renderCollectorStatus(collector);
  }
  renderInspections(assets, templates, schedules, runs);
  if (!document.activeElement || !$('rule-list').contains(document.activeElement)) renderRules(rules);
  if (selectedAsset) await showAsset(selectedAsset);
}

function renderAdminAudit(entries, target = $('admin-audit-list')) {
  target.replaceChildren();
  if (!entries) { target.append(cell('p', '操作记录暂时不可用。', 'empty')); return; }
  if (!entries.length) { target.append(cell('p', '尚无事件写入操作。', 'empty')); return; }
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

function assetPagePath() {
  const query = new URLSearchParams({ limit: '50', runtime: $('asset-runtime').value,
    q: $('asset-search').value.trim() });
  if (assetCursors[assetPageIndex]) query.set('cursor', assetCursors[assetPageIndex]);
  return `/api/assets/page?${query}`;
}

async function loadAssetPage() {
  const request = ++assetPageRequest;
  const page = await api(assetPagePath());
  if (request !== assetPageRequest) return;
  assetCursors = assetCursors.slice(0, assetPageIndex + 1);
  if (page.nextCursor) assetCursors.push(page.nextCursor);
  renderAssets(page.assets,
    `第 ${assetPageIndex + 1} 页 · 显示 ${page.assets.length} 个资产${page.hasMore ? '，还有更多' : ''}；搜索覆盖全部纳管资产。`);
  $('asset-prev').disabled = assetPageIndex === 0;
  $('asset-next').disabled = !page.hasMore;
}

function renderLegacyAssets() {
  const query = $('asset-search').value.trim().toLocaleLowerCase('zh-CN');
  const runtime = $('asset-runtime').value;
  const filtered = allAssets.filter(asset => (runtime === 'all' || asset.runtime === runtime) &&
    (!query || [asset.id, asset.name, asset.host].some(value =>
      value.toLocaleLowerCase('zh-CN').includes(query))));
  renderAssets(filtered, `显示 ${filtered.length} / ${allAssets.length} 个资产。`);
}

function resetAssetPage() {
  if (!assetServerPaging) { renderLegacyAssets(); return; }
  assetCursors = [''];
  assetPageIndex = 0;
  clearTimeout(assetSearchTimer);
  assetSearchTimer = setTimeout(() => loadAssetPage().catch(error => notice(error.message, true)), 250);
}

function renderAssets(assets, summary = '') {
  const list = $('asset-list');
  list.replaceChildren();
  $('asset-filter-summary').textContent = summary;
  if (!assets.length) { list.append(cell('p', allAssets.length ? '当前筛选没有匹配资产。' : '尚无纳管资产。', 'empty')); return; }
  for (const asset of assets) {
    const item = document.createElement('article'); item.className = 'list-item';
    const info = document.createElement('div');
    info.append(cell('strong', asset.name), cell('small', `${asset.host}:${asset.port} · ${assetRuntimeLabel(asset)}`));
    const version = asset.runtime === 'mysql' ? (asset.versionNum ? `MySQL 8.4.${asset.versionNum % 100}` : 'MySQL 待识别')
      : `PG ${asset.versionNum ? Math.floor(asset.versionNum / 10000) : '待识别'}${asset.pg14Eol ? ' · EOL' : ''}`;
    info.append(cell('small', `${version} · ${formatTime(asset.lastSampleAt)}`));
    const actions = document.createElement('div'); actions.className = 'actions';
    const health = assetHealth(asset);
    actions.append(cell('span', health.label, `pill ${health.kind}`));
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

async function showAsset(id, layer) {
  const preferredLayer = layer ?? (selectedAsset === id ? selectedAssetLayer : null);
  const restoreLayerFocus = selectedAsset === id && document.activeElement?.getAttribute('role') === 'tab';
  selectedAsset = id;
  const [{ asset }, { samples }] = await Promise.all([api(`/api/assets/${id}`), api(`/api/assets/${id}/samples`)]);
  if (selectedAsset !== id) return;
  if (asset.runtime === 'mysql') {
    renderMySQLAsset($('detail'), asset, samples);
    return;
  }
  const [{ performance }, { entries: slowLogs }, { plans }] = await Promise.all([
    api(`/api/assets/${id}/sql`), api(`/api/assets/${id}/slow-logs`), api(`/api/assets/${id}/sql-plans`)]);
  if (selectedAsset !== id) return;
  const detail = $('detail'); detail.replaceChildren();
  detail.append(cell('h3', asset.name));
  const location = asset.runtime === 'k8s' ? `${asset.k8sCluster}/${asset.namespace}/${asset.pod || asset.workload || '未映射'}`
    : asset.runtime === 'managed' ? '托管 PostgreSQL · 主机与 K8S 资源信号不可用'
      : asset.linuxHost || 'Linux 主机未关联';
  detail.append(cell('p', `${asset.host}:${asset.port}/${asset.database} · ${location}`));
  if (asset.pg14Eol) detail.append(cell('p', 'PostgreSQL 14 已结束上游支持，请规划升级。', 'warning'));
  detail.append(cell('p', `PgBouncer：${asset.poolerConfigured ? '已配置只读统计连接' : '未配置统计连接'}${asset.poolerEndpoint ? ` · 应用端点 ${asset.poolerEndpoint}` : ''}`));
  detail.append(cell('p', `Patroni：${asset.patroniConfigured ? '已配置只读集群状态' : '未配置'}`));
  detail.append(cell('p', `采集状态：${assetHealth(asset).label} · ${formatTime(asset.lastSampleAt)}${asset.lastError ? ' · ' + asset.lastError : ''}`));
  const sample = samples[0];
  const currentSample = sample?.collectedAt === asset.lastSampleAt ? sample : null;
  renderSignalCoverage(detail, asset, currentSample, performance);
  if (pg18Store) {
    const trendHead = document.createElement('div'); trendHead.className = 'obs-trend-head';
    trendHead.append(cell('h3', '采样与连接趋势'));
    const periods = document.createElement('div'); periods.className = 'obs-periods';
    const trend = document.createElement('div');
    const loadTrend = async (hours, bucketHours) => {
      for (const button of periods.children) button.setAttribute('aria-pressed', String(Number(button.dataset.hours) === hours));
      trend.replaceChildren(cell('p', '正在读取趋势…', 'empty'));
      const to = new Date(); const from = new Date(to.getTime() - hours * 3600 * 1000);
      try {
        const result = await api(`/api/assets/${id}/series?from=${encodeURIComponent(from.toISOString())}&to=${encodeURIComponent(to.toISOString())}&bucketHours=${bucketHours}`);
        if (selectedAsset === id) trend.replaceChildren(
          window.DbSphereObservability.sampleTrend(result.series, hours, bucketHours),
          window.DbSphereObservability.metricTrend(result.series, hours, bucketHours));
      } catch (error) {
        if (selectedAsset === id) trend.replaceChildren(cell('p', `趋势读取失败：${error.message}`, 'warning'));
      }
    };
    for (const [label, hours, bucketHours] of [['24 小时', 24, 1], ['7 天', 168, 6]]) {
      const button = cell('button', label, 'ghost small-button'); button.type = 'button';
      button.dataset.hours = String(hours); button.setAttribute('aria-pressed', String(hours === selectedAssetTrendHours));
      button.addEventListener('click', () => { selectedAssetTrendHours = hours; void loadTrend(hours, bucketHours); }); periods.append(button);
    }
    trendHead.append(periods); detail.append(trendHead, trend);
    await loadTrend(selectedAssetTrendHours, selectedAssetTrendHours === 168 ? 6 : 1);
    detail.append(historyTierChart(id));
    detail.append(assetRiskPanel(id));
  }
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
  if (currentSample?.metrics?.backup) {
    const backup = currentSample.metrics.backup;
    const label = backup.status === 'ok'
      ? `pgBackRest ${backup.stanza} · 仓库${backup.repositoryHealthy ? '正常' : '异常'} · 最近成功备份 ${formatTime(backup.lastSuccessAt)} · 最近尝试 ${backup.lastAttemptStatus ?? '未提供'}`
      : `备份状态未知：${backup.reason ?? backup.status}`;
    const fresh = backup.lastSuccessAt && Date.now() - new Date(backup.lastSuccessAt).getTime() <= 24 * 3600000;
    detail.append(cell('p', label, backup.status === 'ok' && backup.repositoryHealthy &&
      backup.lastAttemptStatus !== 'failed' && fresh ? 'evidence' : 'warning'));
  }
  renderSqlPerformance(detail, performance);
  renderSlowLogs(detail, asset, slowLogs);
  renderSqlPlans(detail, asset, plans);
  if (currentSample?.metrics?.k8s) {
    const k8s = currentSample.metrics.k8s;
    const heading = cell('h3', 'Kubernetes 运行状态'); heading.id = 'k8s-detail'; detail.append(heading);
    if (k8s.status === 'ok') {
      detail.append(cell('p', `Pod ${k8s.pod.name} · ${k8s.pod.phase || '未知'} · Node ${k8s.pod.node || '未映射'}`));
      if (k8s.podMapping) detail.append(cell('p', `Workload ${k8s.podMapping.workload} · Pod ${k8s.podMapping.resolved}${k8s.workload?.uid ? ` · Workload UID ${k8s.workload.uid}` : ''}`, 'hint'));
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
  if (currentSample?.metrics?.patroni) {
    const patroni = currentSample.metrics.patroni;
    const heading = cell('h3', 'Patroni 集群拓扑'); heading.id = 'patroni-detail'; detail.append(heading);
    if (patroni.status === 'ok') {
      detail.append(cell('p', `来源 ${patroni.source} · leader ${patroni.leaderCount} 个 · 其他成员 ${patroni.replicaCount} 个 · 维护模式 ${patroni.paused ? '开启' : '关闭'}`,
        patroni.leaderCount === 1 ? 'evidence' : 'warning'));
      for (const member of patroni.members || []) detail.append(cell('p',
        `${member.name} · ${member.role} · ${member.state} · ${member.host}:${member.port} · timeline ${member.timeline}${member.replayLagBytes == null ? '' : ` · 回放延迟 ${member.replayLagBytes} 字节`}`,
        'evidence'));
    } else detail.append(cell('p', `Patroni 未采集：${patroni.reason || patroni.status}`, 'warning'));
  }
  if (pg18Store || viewerScoped) {
    const settings = await api(`/api/assets/${id}/routing`).catch(() => null);
    if (selectedAsset === id && settings) renderRoutingSettings(detail, asset, settings, currentSample);
  }
  if (pg18Store) {
    const { forecast } = await api(`/api/assets/${id}/capacity`);
    const heading = cell('h3', '数据库容量趋势'); heading.id = 'capacity-detail'; detail.append(heading);
    if (forecast.status === 'estimate') {
      const gib = value => `${(value / 1024 ** 3).toFixed(2)} GiB`;
      detail.append(cell('p', `最近 ${forecast.observedDays.toFixed(0)} 天、${forecast.points.length} 个每日样本；当前 ${gib(forecast.latestBytes)}，每日增长约 ${gib(forecast.dailyGrowthBytes)}，30 天后约 ${gib(forecast.projected30DayBytes)}。置信度 ${forecast.confidence}；${forecast.method}。`, 'evidence'));
      detail.append(cell('small', `最新样本 ${forecast.latestSampleId}；估计不代表磁盘剩余空间或备份可恢复性。`));
    } else detail.append(cell('p', `容量预测暂不可用：${forecast.reason}`, 'empty'));
  }
  organizeAssetLayers(detail, asset, preferredLayer, restoreLayerFocus);
}

const routingKinds = {pgbouncer: 'PgBouncer', haproxy: 'HAProxy', k8s_service: 'Kubernetes Service'};
const routingStatus = {
  ok: '入口可用', misrouted: '入口转发错位', pool_exhausted: '连接池占满，客户端排队',
  ready_pod_entry_unreachable: 'Pod 已就绪，Service 入口不可达', unreachable: '入口不可达',
  dns_error: '入口 DNS 解析失败', timeout: '入口探测超时', postgres_rejected: 'PostgreSQL 拒绝连接或探测',
  connection_error: '入口握手失败', backend_unknown: '后端身份待核对',
  configuration_error: '探测配置有误', cancelled: '探测取消'
};

function routingOverview(assetId, route, sampleId) {
  const body = cell('div', '', 'routing-overview');
  body.append(cell('p', `${routingKinds[route.kind]} · ${route.entry} · ${routingStatus[route.status] ?? route.status}`,
    route.status === 'ok' ? 'evidence' : 'warning'));
  const roles = {primary: '主库', standby: '备库'};
  const renderedRole = role => roles[role] ?? '未知';
  const probe = route.probe ?? {};
  body.append(cell('p', `入口后端 ${probe.backend || '未识别'} · ${renderedRole(probe.role)} · 库 ${probe.database || '未识别'}${probe.durationMs == null ? '' : ` · ${probe.durationMs} ms`}`));
  if (route.readEntry) {
    const read = route.readProbe || {};
    body.append(cell('p', `备库入口 ${route.readEntry} · ${routingStatus[route.readStatus] || route.readStatus} · ${read.backend || '后端待核对'} · ${renderedRole(read.role)}`,
      route.readStatus === 'ok' ? 'evidence' : 'warning'));
  }
  if (route.direct) body.append(cell('p', `资产直连 ${route.direct.backend} · ${renderedRole(route.direct.role)}`));
  if (route.expectedRole) body.append(cell('p', `期望角色 ${renderedRole(route.expectedRole)}`));
  if (probe.sqlState) body.append(cell('p', `SQLSTATE ${probe.sqlState}`, 'hint'));
  if (route.pooler) {
    body.append(cell('h4', 'PgBouncer 连接池'));
    for (const pool of route.pooler.pools ?? []) {
      body.append(cell('p', `${pool.user} · 排队 ${pool.clientWaiting} · 后端活跃 ${pool.serverActive} · 空闲 ${pool.serverIdle}`));
    }
    if (route.pooler.route) body.append(cell('p', `配置后端 ${route.pooler.route.host || '未知'}:${route.pooler.route.port || '未知'}`, 'hint'));
  }
  if (route.haproxy) {
    const stats = route.haproxy;
    body.append(cell('h4', `HAProxy 后端 ${stats.backend}`));
    if (stats.status !== 'ok') body.append(cell('p',
      stats.status === 'backend_missing' ? '统计中没有指定后端。' : '统计暂不可读。', 'hint'));
    for (const row of stats.rows ?? []) {
      body.append(cell('p', [row.name, row.addr, row.status, row.qcur == null ? '' : `当前队列 ${row.qcur}`,
        row.qmax == null ? '' : `峰值队列 ${row.qmax}`, row.check_status].filter(Boolean).join(' · ')));
    }
  }
  if (route.service) {
    const service = route.service;
    body.append(cell('h4', `Kubernetes Service ${service.namespace}/${service.name}`));
    body.append(cell('p', `Service ${service.portMapping.port} → targetPort ${service.portMapping.targetPort} · Pod ${service.pod} ${service.podReady ? '已就绪' : '就绪待确认'}`));
    body.append(cell('p', `EndpointSlice ${Object.keys(service.slices || {}).join('、') || '未找到该 Pod 的切片'} · Pod UID ${service.podUid || '未核对'}`, 'hint'));
  }
  const sources = [probe.source, route.direct?.source, route.pooler?.source, route.haproxy?.source, route.service?.source].filter(Boolean);
  const caption = cell('p', `${formatTime(probe.observedAt)} · ${sources.join(' · ')} · `, 'hint');
  caption.append(replaySampleLink(assetId, sampleId));
  body.append(caption);
  return body;
}

function renderRoutingSettings(detail, asset, settings, sample) {
  const heading = cell('h3', '代理与访问入口'); heading.id = 'routing-detail';
  detail.append(heading);
  const route = sample?.metrics?.routing;
  if (settings.kind && route?.entry === settings.entry && route.kind === settings.kind &&
      route.expectedRole === settings.expectedRole && route.matchDirect === settings.matchDirect &&
      (route.readEntry || '') === (settings.readEntry || ''))
    detail.append(routingOverview(asset.id, route, sample.id));
  else detail.append(cell('p', settings.kind ? '等待新配置的首次采样。' : '尚未配置入口探测。', 'hint'));
  if (currentRole !== 'admin') return;
  const config = cell('details', '', 'settings-details');
  config.append(cell('summary', '配置只读入口探测'));
  const form = cell('form', '', 'form-grid routing-form');
  const field = (name, control) => {
    const label = cell('label', name);
    control.setAttribute('aria-label', name);
    label.append(control);
    form.append(label);
  };
  const select = (choices, value) => {
    const control = document.createElement('select');
    for (const [key, label] of choices) {
      const option = cell('option', label); option.value = key; control.append(option);
    }
    control.value = value;
    return control;
  };
  const input = (value, placeholder, type = 'text') => {
    const control = document.createElement('input');
    control.type = type; control.value = value; control.placeholder = placeholder;
    return control;
  };
  const kinds = Object.entries(routingKinds).filter(([kind]) => kind !== 'k8s_service' || asset.runtime === 'k8s');
  const kind = select([['', '关闭'], ...kinds], settings.kind);
  const entry = input(settings.entry, 'db-service.namespace.svc.cluster.local:5432');
  const read = input(settings.readEntry || '', '可选，连接应返回备库');
  const role = select([['', '仅观察'], ['primary', '主库'], ['standby', '备库']], settings.expectedRole);
  const match = input('', '', 'checkbox'); match.checked = settings.matchDirect;
  const backend = input(settings.haproxyBackend, '例如 postgres-primary');
  const stats = input('', settings.statsConfigured ? '已设置，留空保留' : 'HAProxy 只读统计地址', 'password');
  stats.autocomplete = 'new-password';
  field('入口类型', kind); field('入口主机与端口', entry); field('期望 PostgreSQL 角色', role);
  field('备库入口主机与端口', read);
  field('要求入口与资产直连命中同一后端', match);
  field('HAProxy 后端名称', backend); field('HAProxy CSV 统计地址', stats);
  const updateFields = () => {
    for (const control of [entry, read, role, match]) control.parentElement.hidden = !kind.value;
    for (const control of [backend, stats]) control.parentElement.hidden = kind.value !== 'haproxy';
    entry.required = Boolean(kind.value);
  };
  updateFields();
  kind.addEventListener('change', updateFields);
  const submit = cell('button', '保存入口配置'); submit.type = 'submit'; form.append(submit);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    submit.disabled = true;
    try {
      await api(`/api/assets/${asset.id}/routing`, 'PATCH', {
        kind: kind.value, entry: entry.value, readEntry: read.value, expectedRole: role.value, matchDirect: match.checked,
        haproxyBackend: kind.value === 'haproxy' ? backend.value : '',
        haproxyStatsUrl: kind.value === 'haproxy' ? stats.value : ''
      });
      notice('入口配置已保存，下一次采样生效。');
      if (selectedAsset === asset.id) await showAsset(asset.id, 'routing-detail');
    } catch (error) {
      notice(error.message, true);
    } finally { submit.disabled = false; }
  });
  config.append(form); detail.append(config);
}

function renderSignalCoverage(target, asset, sample, performance) {
  const metrics = sample?.metrics || {};
  const diagnostics = Object.values(metrics.diagnostics || {});
  const available = diagnostics.filter(item => item?.status === 'ok').length;
  const rows = [
    ['数据库连接与基础指标', metrics.collector?.pgStatus === 'ok' ? '已采集' : sample ? '采集失败' : '待首采'],
    ['数据库深度诊断', diagnostics.length ? `${available}/${diagnostics.length} 项已采集${available < diagnostics.length ? '；其余信号缺失' : ''}` : '未采集'],
    ['Top SQL', performance?.status === 'ok'
      ? (performance.sampleId === sample?.id ? '已采集' : '仅有历史样本；本次未采集')
      : `未采集：${performance?.reason || '未启用或无权限'}`],
    [asset.runtime === 'k8s' ? 'K8S Pod / Node / PVC' : 'Linux 主机资源',
      asset.runtime === 'managed' ? '不适用：托管模式无主机或 K8S API 信号'
        : asset.runtime === 'k8s' ? (metrics.k8s?.status === 'ok' ? '已采集' : `未采集：${metrics.k8s?.reason || '待检查映射与权限'}`)
          : metrics.linux?.status === 'ok' ? '已采集' : `未采集：${metrics.linux?.reason || '未配置 node_exporter'}`],
    ['PgBouncer', asset.poolerConfigured ? (metrics.pgbouncer?.status === 'ok' ? '已采集' : `未采集：${metrics.pgbouncer?.reason || '连接或权限待检查'}`) : asset.poolerEndpoint ? '仅配置应用端点，未接入统计采集' : '未接入'],
    ['Patroni', asset.patroniConfigured ? (metrics.patroni?.status === 'ok' ? '已采集' : `未采集：${metrics.patroni?.reason || '端点待检查'}`) : '未接入']
  ];
  const adapter = metrics.adapter;
  if (asset.runtime === 'managed' && adapter?.engine === 'postgresql') {
    const transport = adapter.transport;
    const rds = adapter.profile === 'aws-rds-postgresql';
    const provider = adapter.providerStatus === 'provider_signals_observed' ? '已观测到厂商特征' : '厂商特征待核验';
    rows.push(
      ['采集适配器', rds ? `AWS RDS for PostgreSQL · ${provider}` : '通用托管 PostgreSQL'],
      ['采集会话', `${transport.verified ? '证书与主机名已验证' : transport.encrypted ? '已加密，身份未验证' : '未启用 TLS'}${transport.protocol ? ` · ${transport.protocol}` : ''} · ${transport.readOnly ? '只读' : '只读状态异常'}`],
      ['数据库角色', adapter.role === 'standby' ? '只读副本' : '主库']
    );
  }
  const panel = document.createElement('details'); panel.className = 'settings-details';
  panel.append(cell('summary', `采集能力与缺口 · ${assetHealth(asset).label}`));
  const list = document.createElement('div'); list.className = 'diagnostic-list';
  for (const [label, status] of rows) {
    const row = document.createElement('div'); row.append(cell('strong', label), cell('span', status)); list.append(row);
  }
  panel.append(list);
  const compatibility = metrics.pgCompatibility;
  if (compatibility?.signals?.length) {
    const labels = { activeWaits: '活动等待', instanceIO: '实例 I/O', checkpoints: '检查点', wal: 'WAL',
      replicationLinks: '复制链路', walReceiver: 'WAL 接收', sqlPerformance: 'Top SQL 指纹',
      logicalPublications: '逻辑发布', logicalSubscriptions: '逻辑订阅', logicalApplyProgress: '逻辑应用进度',
      logicalSubscriptionErrors: '逻辑订阅错误' };
    const statuses = { ok: '已采集', unavailable: '采集失败', not_configured: '当前数据库未安装扩展',
      not_supported: '此版本无对应视图', limited_permissions: '权限受限', inactive: '当前无活动接收器' };
    const signals = cell('div', '', 'diagnostic-list');
    for (const signal of compatibility.signals) {
      const status = statuses[signal.status] || '未知';
      const permissionReason = signal.reason?.endsWith('SQLSTATE 42501')
        ? ' · 读取视图被拒绝（SQLSTATE 42501）' : ' · 需可用的 pg_read_all_stats 权限';
      const reason = signal.status === 'limited_permissions' ? permissionReason
        : signal.status === 'unavailable' && signal.reason ? ` · ${signal.reason}` : '';
      const row = cell('div', '');
      row.append(cell('strong', labels[signal.key] || signal.key), cell('span', `${signal.source} · ${status}${reason}`));
      signals.append(row);
    }
    panel.append(cell('h4', `PostgreSQL ${compatibility.major} 版本能力${compatibility.lifecycle === 'upstream_eol' ? ' · 上游已停止支持' : ''}`), signals);
  }
  target.append(panel);
}

function renderMySQLAsset(detail, asset, samples) {
  detail.replaceChildren(cell('h3', asset.name),
    cell('p', `${asset.host}:${asset.port}/${asset.database} · MySQL 8.4 只读预览`),
    cell('p', `采集状态：${assetHealth(asset).label} · ${formatTime(asset.lastSampleAt)}${asset.lastError ? ' · ' + asset.lastError : ''}`));
  const sample = samples[0];
  const metrics = sample?.status === 'ok' && sample.collectedAt === asset.lastSampleAt ? sample.metrics : null;
  if (!metrics?.mysql || !metrics.adapter) {
    detail.append(cell('p', '当前配置尚无成功采样。', 'empty'));
    return;
  }
  const data = metrics.mysql;
  const report = metrics.adapter;
  const role = { writable: '可写实例', read_only: '只读实例', replica: '复制副本' }[report.role];
  const transport = report.transport;
  const tls = transport.verified ? `证书与主机名已验证${transport.protocol ? ' · ' + transport.protocol : ''}` : '未启用 TLS';
  const grid = cell('div', '', 'metric-grid');
  for (const [label, value] of [['版本', data.version], ['角色', role], ['当前连接', data.connections], ['运行线程', data.running], ['最大连接', data.maxConnections]]) {
    const metric = cell('div', '');
    metric.append(cell('span', label), cell('strong', value == null ? '未采集' : String(value)));
    grid.append(metric);
  }
  detail.append(grid, cell('p', `采集会话 · ${transport.readOnly ? '只读' : '只读状态异常'} · ${tls}`, 'hint'));
  const coverage = cell('details', '', 'settings-details');
  coverage.append(cell('summary', '采集能力与缺口'));
  const labels = { connections: '连接状态', lockWaits: 'InnoDB 锁等待', sqlPerformance: 'Top SQL 指纹', replication: '复制状态' };
  const status = { ok: '已采集', limited_permissions: '权限受限', not_configured: '未启用', unavailable: '采集失败' };
  const table = (headings, rows) => {
    const block = cell('div', '', 'monitor-table');
    block.append(importTable(headings, rows));
    return block;
  };
  coverage.append(table(['指标', '数据源', '采集状态'], report.signals.map(signal => [
    labels[signal.key] || signal.key, signal.source, `${status[signal.status] || '未知'}${signal.reason ? ' · ' + signal.reason : ''}`
  ])));
  detail.append(coverage);
  const available = key => report.signals.some(signal => signal.key === key && signal.status === 'ok');
  const connectionHistory = samples.slice(0, 12).filter(point => point.metrics?.mysql?.connections != null);
  if (connectionHistory.length) {
    const history = cell('details', '', 'settings-details');
    history.append(cell('summary', '最近连接采样'), table(['时间', '当前连接', '运行线程', '最大连接'],
      connectionHistory.map(point => [formatTime(point.collectedAt), String(point.metrics.mysql.connections),
        String(point.metrics.mysql.running ?? '未采集'), String(point.metrics.mysql.maxConnections)])));
    detail.append(history);
  }
  const incident = recentEvents.find(event => event.assetId === asset.id && event.kind === 'mysql_lock_wait');
  detail.append(cell('h3', 'InnoDB 锁等待'));
  if (incident) {
    const link = cell('button', incident.status === 'resolved' ? '查看最近锁等待事件' : '查看锁等待事件', 'ghost small-button');
    link.type = 'button';
    link.addEventListener('click', () => void openEvent(incident.id));
    detail.append(link);
  }
  if (available('lockWaits') && data.lockWaits?.length) {
    detail.append(table(['等待线程', '阻塞线程', '对象', '请求锁模式'], data.lockWaits.map(wait => [
      String(wait.waitingThread || '未知'), String(wait.blockingThread || '未知'), [wait.schema, wait.table].filter(Boolean).join('.') || '未采集', wait.mode || '未采集'
    ])));
    if (data.locksTruncated) detail.append(cell('p', '显示当前样本中的 20 条等待关系。', 'hint'));
  } else detail.append(cell('p', available('lockWaits') ? '本次采样未发现 InnoDB 锁等待。' : '本次锁等待信号不可用。', 'empty'));
  detail.append(cell('h3', 'Top SQL 指纹'));
  if (available('sqlPerformance') && data.topSql?.length) {
    detail.append(table(['指纹', '执行次数', '累计耗时 ms', '平均耗时 ms', '扫描行数'], data.topSql.map(row => [
      cell('code', row.digest), String(row.calls), row.totalMs.toFixed(2), row.meanMs.toFixed(2), String(row.rowsExamined)
    ])));
  } else detail.append(cell('p', available('sqlPerformance') ? '指定数据库暂无指纹数据。' : '本次 SQL 指纹信号不可用。', 'empty'));
  detail.append(cell('h3', '复制通道'));
  if (available('replication') && data.replication?.length) {
    detail.append(table(['通道', '源实例 UUID', '接收线程', '应用线程', '最近 I/O 错误码'], data.replication.map(row => [
      row.name || '默认', row.sourceUuid || '未知', row.receiver, row.applier, String(row.lastIoError)
    ])));
  } else detail.append(cell('p', available('replication') ? '本次采样未发现入站复制通道。' : '本次复制信号不可用。', 'empty'));
}

function organizeAssetLayers(detail, asset, preferredLayer, restoreFocus = false) {
  const definitions = [
    ['pg-diagnostics', 'PostgreSQL'], ['sql-performance', 'Top SQL'],
    ['slow-logs', '慢 SQL 日志'], ['sql-plans', '执行计划'],
    ...(asset.runtime === 'managed' ? [] : [[asset.runtime === 'k8s' ? 'k8s-detail' : 'linux-detail',
      asset.runtime === 'k8s' ? 'Pod / Node / PVC' : 'Linux 主机']]),
    ['pooler-detail', 'PgBouncer'], ['patroni-detail', 'Patroni'], ['routing-detail', '代理与访问入口'], ['capacity-detail', '容量趋势']
  ];
  const labels = new Map(definitions);
  const headings = [...detail.querySelectorAll('h3[id]')].filter(heading => labels.has(heading.id));
  if (!headings.length) return;
  const panels = new Map();
  for (let index = 0; index < headings.length; index++) {
    const heading = headings[index];
    const next = headings[index + 1];
    const panel = document.createElement('section');
    panel.className = 'asset-layer';
    panel.id = `asset-layer-${heading.id}`;
    panel.setAttribute('role', 'tabpanel');
    detail.insertBefore(panel, heading);
    for (let node = heading; node && node !== next;) {
      const following = node.nextSibling;
      panel.append(node);
      node = following;
    }
    panels.set(heading.id, panel);
  }
  const nav = document.createElement('div');
  nav.className = 'actions layer-nav';
  nav.setAttribute('role', 'tablist');
  nav.setAttribute('aria-label', '实例监控层级');
  const buttons = new Map();
  const activate = id => {
    selectedAssetLayer = id;
    for (const [key, panel] of panels) {
      const active = key === id;
      panel.hidden = !active;
      const button = buttons.get(key);
      button.classList.toggle('active', active);
      button.setAttribute('aria-selected', String(active));
      button.tabIndex = active ? 0 : -1;
    }
  };
  for (const [id, panel] of panels) {
    const button = cell('button', labels.get(id), 'ghost small-button');
    button.type = 'button';
    button.id = `asset-tab-${id}`;
    button.setAttribute('role', 'tab');
    button.setAttribute('aria-controls', panel.id);
    panel.setAttribute('aria-labelledby', button.id);
    button.addEventListener('click', () => activate(id));
    buttons.set(id, button);
    nav.append(button);
  }
  nav.addEventListener('keydown', event => {
    const keys = [...buttons.keys()];
    const current = keys.indexOf(selectedAssetLayer);
    const next = event.key === 'ArrowRight' ? (current + 1) % keys.length
      : event.key === 'ArrowLeft' ? (current - 1 + keys.length) % keys.length
        : event.key === 'Home' ? 0 : event.key === 'End' ? keys.length - 1 : -1;
    if (next < 0) return;
    event.preventDefault();
    buttons.get(keys[next]).focus();
    activate(keys[next]);
  });
  detail.insertBefore(nav, panels.get(headings[0].id));
  const initial = panels.has(preferredLayer) ? preferredLayer : headings[0].id;
  activate(initial);
  if (restoreFocus) buttons.get(initial).focus();
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

async function* slowLogRecords(file, csv) {
  const reader = file.stream().pipeThrough(new TextDecoderStream('utf-8', { fatal: true })).getReader();
  let record = '', quoted = false;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      for (const char of value) {
        record += char;
        if (record.length > 60000) throw new Error('一条日志超过 60 KB。');
        if (csv && char === '"') quoted = !quoted;
        if (char === '\n' && !quoted) {
          if (record.trim()) yield record;
          record = '';
        }
      }
    }
    if (quoted) throw new Error('CSV 日志末尾存在未完整写入的记录。');
    if (record.trim()) yield record;
  } finally {
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

async function* slowLogBatches(file, format) {
  const encoder = new TextEncoder();
  let records = [], bytes = 0, encodedBytes = 0;
  for await (const record of slowLogRecords(file, format === 'csvlog')) {
    const size = encoder.encode(record).byteLength;
    const encoded = encoder.encode(JSON.stringify(record)).byteLength - 2;
    if (size > 60000 || encoded > 130000) throw new Error('一条日志超过导入上限。');
    if (records.length === 200 || bytes + size > 60000 || encodedBytes + encoded > 130000) {
      yield records.join('');
      records = []; bytes = 0; encodedBytes = 0;
    }
    records.push(record);
    bytes += size;
    encodedBytes += encoded;
  }
  if (records.length) yield records.join('');
}

function renderSlowLogs(target, asset, entries) {
  const heading = cell('h3', '慢 SQL 日志'); heading.id = 'slow-logs'; target.append(heading);
  target.append(cell('p', '选择 UTF-8 的 PostgreSQL csvlog 或 jsonlog。文件按完整记录分批导入，重跑时自动跳过已导入的记录。日志正文和参数在导入时丢弃。', 'hint'));
  const form = document.createElement('form'); form.className = 'actions';
  form.hidden = currentRole !== 'admin';
  const format = document.createElement('select');
  for (const value of ['csvlog', 'jsonlog']) {
    const option = document.createElement('option'); option.value = value; option.textContent = value;
    format.append(option);
  }
  const file = document.createElement('input'); file.type = 'file'; file.required = true;
  file.accept = '.csv,.json,.log,.txt';
  const submit = cell('button', '增量导入'); submit.type = 'submit';
  const progress = cell('p', '', 'hint'); progress.setAttribute('aria-live', 'polite');
  form.append(format, file, submit);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!file.files?.[0]) return;
    const selected = file.files[0], source = format.value;
    const totals = { scanned: 0, imported: 0, duplicates: 0, ignored: 0 };
    const report = () => `已扫描 ${totals.scanned} 条，新增 ${totals.imported} 条，重复 ${totals.duplicates} 条，忽略 ${totals.ignored} 条。`;
    submit.disabled = true; file.disabled = true; format.disabled = true;
    try {
      for await (const content of slowLogBatches(selected, source)) {
        const result = await api(`/api/assets/${asset.id}/slow-logs/import`, 'POST', { format: source, content });
        for (const key of Object.keys(totals)) totals[key] += result[key];
        progress.textContent = report();
      }
      notice(report());
      if (target.isConnected) await showAsset(asset.id, 'slow-logs');
    } catch (error) {
      progress.textContent = `${report()} ${error.message}`;
      notice(progress.textContent, true);
    } finally { submit.disabled = false; file.disabled = false; format.disabled = false; }
  });
  target.append(form, progress);
  if (!entries.length) target.append(cell('p', '近 24 小时没有导入的慢 SQL 记录。', 'empty'));
  for (const row of entries.slice(0, 30)) {
    const item = document.createElement('div'); item.className = 'evidence';
    item.append(cell('span', `${formatTime(row.observedAt)} · ${row.database || '库名待核对'} · ${row.durationMs.toFixed(1)} ms · ${row.queryId ? `Query ID ${row.queryId}` : 'Query ID 未提供'} · ${row.sourceFormat}`));
    for (const id of row.eventIds) {
      const link = cell('button', `事件 ${id.slice(0, 8)}`, 'ghost small-button');
      link.addEventListener('click', () => showEvent(id)); item.append(link);
    }
    target.append(item);
  }
}

function renderPlanComparison(comparison, assetId, currentId) {
  const detail = cell('details', '', 'evidence');
  detail.append(cell('summary', '查看计划前后对照'));
  const labels = { executionMs: '执行耗时 ms', estimateFactor: '最大行数估算偏差倍数', tempReadBlocks: '根节点临时读块', tempWrittenBlocks: '根节点临时写块' };
  const display = value => typeof value === 'number' ? value.toLocaleString('zh-CN', { maximumFractionDigits: 2 }) : '—';
  const rows = [...(comparison.operations || []), ...(comparison.measurements || [])].map(row => [labels[row.name] || row.name, display(row.before), display(row.after)]);
  const table = importTable(['扫描、连接与指标', '事前', '事后'], rows);
  table.setAttribute('aria-label', 'SQL 执行计划前后对照');
  const wrap = cell('div', '', 'monitor-table'); wrap.append(table); detail.append(wrap);
  const references = cell('div', '', 'actions');
  for (const [label, id] of [['事前计划摘要', comparison.priorId], ['事后计划摘要', currentId]]) {
    const link = cell('a', label, 'monitor-link'); link.href = `/api/assets/${assetId}/sql-plans/${id}`;
    references.append(link);
  }
  detail.append(references);
  return detail;
}

function renderSqlPlans(target, asset, plans) {
  const heading = cell('h3', '执行计划证据'); heading.id = 'sql-plans'; target.append(heading);
  target.append(cell('p', `导入从 ${asset.database} 取得的 EXPLAIN JSON。带 Query Identifier 的计划会核对该标识。保存的摘要仅包含节点和统计数值。`, 'hint'));
  const form = document.createElement('form'); form.className = 'actions';
  form.hidden = currentRole !== 'admin';
  const queryId = document.createElement('input'); queryId.placeholder = 'Query ID';
  queryId.required = true; queryId.pattern = '-?[0-9]{1,20}';
  const file = document.createElement('input'); file.type = 'file'; file.accept = '.json'; file.required = true;
  const submit = cell('button', '导入计划'); submit.type = 'submit';
  form.append(queryId, file, submit);
  form.addEventListener('submit', async event => {
    event.preventDefault();
    try {
      if (file.files[0].size > 50000) throw new Error('计划文件超过 50 KiB。');
      await api(`/api/assets/${asset.id}/sql-plans`, 'POST', {
        queryId: queryId.value, database: asset.database, observedAt: new Date().toISOString(), plan: (await file.files[0].text()).replace(/^\uFEFF/, '')
      });
      notice('执行计划摘要已导入。'); await showAsset(asset.id);
    } catch (error) { notice(error.message, true); }
  });
  target.append(form);
  if (!plans.length) target.append(cell('p', '尚无执行计划证据。', 'empty'));
  for (const plan of plans.slice(0, 20)) {
    const item = document.createElement('div'); item.className = 'evidence';
    item.append(cell('span', `Query ID ${plan.queryId} · ${plan.database || '库名待核对'} · ${formatTime(plan.observedAt)} · ${plan.summary.nodeCount} 个节点 · ${plan.summary.executionMs == null ? '无实际执行时间' : `${plan.summary.executionMs.toFixed(1)} ms`}`));
    if (plan.summary.queryIdentity === 'explain') item.append(cell('small', '已核对计划 Query Identifier'));
    if (plan.comparison?.shapeChanged) item.append(cell('span', '计划结构较上一份变化', 'warning'));
    if (plan.comparison?.executionRatio >= 2) item.append(cell('span', `导入计划的执行时间比前次高 ${plan.comparison.executionRatio.toFixed(1)} 倍，请结合采样复核。`, 'warning'));
    if (plan.comparison) item.append(renderPlanComparison(plan.comparison, asset.id, plan.id));
    for (const id of plan.eventIds) {
      const link = cell('button', `事件 ${id.slice(0, 8)}`, 'ghost small-button');
      link.addEventListener('click', () => showEvent(id)); item.append(link);
    }
    target.append(item);
  }
}

function eventPagePath() {
  const query = new URLSearchParams({ limit: '50', status: $('event-status').value,
    q: $('event-search').value.trim() });
  if (eventCursors[eventPageIndex]) query.set('cursor', eventCursors[eventPageIndex]);
  return `/api/events/page?${query}`;
}

async function loadEventPage() {
  const request = ++eventPageRequest;
  const page = await api(eventPagePath());
  if (request !== eventPageRequest) return;
  eventCursors = eventCursors.slice(0, eventPageIndex + 1);
  if (page.nextCursor) eventCursors.push(page.nextCursor);
  renderEvents(page.events, page.hasMore);
}

function resetEventPage() {
  if (!eventServerPaging) { renderLegacyEvents(recentEvents); return; }
  eventCursors = [''];
  eventPageIndex = 0;
  clearTimeout(eventSearchTimer);
  eventSearchTimer = setTimeout(() => loadEventPage().catch(error => notice(error.message, true)), 250);
}

function renderLegacyEvents(events) {
  recentEvents = events;
  const query = $('event-search').value.trim().toLocaleLowerCase('zh-CN');
  const status = $('event-status').value;
  const filtered = events.filter(event => (status === 'all' || event.status === status) &&
    (!query || [event.id, event.title, event.assetName || ''].some(value =>
      value.toLocaleLowerCase('zh-CN').includes(query))));
  renderEvents(filtered, false,
    `显示 ${filtered.length} / ${events.length} 条最近事件；列表最多包含 200 条。完整事件 ID 可按回车直达。`);
}

function renderEvents(events, hasMore, summary) {
  const list = $('event-list'); list.replaceChildren();
  $('event-filter-summary').textContent = summary || `第 ${eventPageIndex + 1} 页 · 显示 ${events.length} 条${hasMore ? '，还有更多' : ''}；搜索覆盖已保留的历史事件。完整事件 ID 可按回车直达。`;
  $('event-prev').disabled = eventPageIndex === 0;
  $('event-next').disabled = !hasMore;
  if (!events.length) { list.append(cell('p', '当前筛选没有匹配事件。', 'empty')); return; }
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
    button.addEventListener('click', () => openEvent(event.id));
    list.append(button);
  }
}

async function openEvent(id, updateHash = true, monitoringReturn = '') {
  try {
    await showEvent(id, updateHash, monitoringReturn);
    if (selectedEvent === id && matchMedia('(max-width: 1080px)').matches)
      $('event-detail').scrollIntoView({ behavior: 'smooth', block: 'start' });
  } catch (error) {
    if (location.hash.startsWith('#incidents?') && new URLSearchParams(location.hash.split('?')[1]).get('event') === id)
      $('event-detail').replaceChildren(cell('p', error.message, 'warning'));
    else notice(error.message, true);
  }
}

let workbenchStatus = 'all';
function renderAlerts(alerts, incidents) {
  const list = $('alert-list'); list.replaceChildren();
  if (incidents) {
    const select = cell('select');
    select.setAttribute('aria-label', '值班事件范围');
    for (const [value, label] of [['all', '全部事件'], ['open', '处理中'], ['resolved', '最近 72 小时已恢复']]) {
      const option = cell('option', label); option.value = value; select.append(option);
    }
    select.value = workbenchStatus;
    const rows = cell('div', '', 'list');
    list.append(select, rows);
    const render = () => {
      rows.replaceChildren();
      const filtered = incidents.filter(item => select.value === 'all' || item.status === select.value);
      if (!filtered.length) { rows.append(cell('p', '此范围没有值班事件。', 'empty')); return; }
      for (const item of filtered) {
        const line = cell('article', '', 'list-item');
        const info = cell('div');
        const action = cell('button', `${item.assetName} · ${item.title}`, 'event-button');
        action.addEventListener('click', () => openEvent(item.id));
        const state = item.status === 'resolved' ? '已恢复' : item.recoveringSignals ? '恢复观察中' : item.ack ? '已确认' : '待确认';
        const tags = [state, item.severity || '级别未知', item.activeSignals > 0 && `${item.activeSignals} 项触发`,
          item.missingSignals > 0 && `${item.missingSignals} 项缺采`, item.mutedSignals > 0 && `${item.mutedSignals} 项静默`,
          item.maintenance && '有维护或静默记录', item.handoff && '已交接', item.falsePositive && '已记录误报'].filter(Boolean);
        const time = `起点 ${formatTime(item.start)} · 最近触发 ${formatTime(item.lastTrigger)} · 恢复 ${formatTime(item.end)}`;
        const counts = item.notifications || {};
        const delivery = `已送达 ${counts.delivered || 0} · 待投递 ${counts.pending || 0} · 失败 ${counts.failed || 0} · 已跳过 ${counts.skipped || 0}`;
        info.append(action, cell('small', tags.join(' · ')), cell('small', time),
          cell('small', [item.cluster, (item.sources || []).join(' / '), delivery].filter(Boolean).join(' · ')));
        line.append(info);
        rows.append(line);
      }
    };
    select.addEventListener('change', () => { workbenchStatus = select.value; render(); });
    render();
    return;
  }
  if (!alerts.length) { list.append(cell('p', '当前没有触发中的告警。', 'empty')); return; }
  for (const alert of alerts) {
    const button = cell('button', `${alert.assetName} · ${alert.rule.title} · ${alert.rule.severity} · ${formatAlertValue(alert.rule.metric, alert.state.lastValue)}${alert.state.missingSince ? ' · 指标已失联' : ''}${alert.state.mutedBy ? ' · 通知已静默' : ''}`, 'event-button');
    button.addEventListener('click', () => openEvent(alert.state.eventId));
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
  if (storage.backup?.status !== 'ok') {
    target.append(cell('p', storage.backup?.reason ?? '监控存储备份状态未知', 'warning'));
    return;
  }
  if (!storage.backup.restoreTests?.length) {
    target.append(cell('p', '当前备份尚未上报隔离恢复演练。', 'hint'));
    return;
  }
  const seconds = value => Number.isFinite(value) ? `${value.toLocaleString('zh-CN', {maximumFractionDigits: 3})} 秒` : '—';
  target.append(cell('h3', 'PG18 监控库恢复演练'));
  for (const test of storage.backup.restoreTests) {
    const record = cell('div', '', 'evidence');
    const verified = test.verification === 'verified';
    record.append(cell('strong', `${test.id || test.backupId} · ${verified ? '隔离核验通过' : '待核对'}`),
      cell('p', `备份 ${test.backupId} · 备份恢复点 ${formatTime(test.targetAt)} · 恢复验收 ${formatTime(test.finishedAt)}`),
      cell('p', `实测 RPO ${seconds(test.rpoSeconds)} · 实测 RTO ${seconds(test.rtoSeconds)}`),
      cell('p', `事件 ${test.eventsReadable ? '可读' : '待核对'} · 配置 ${test.configurationReadable ? '可读' : '待核对'} · 历史采样 ${test.samplesReadable ? '可读' : '待核对'}`));
    if (test.isolated) record.append(cell('p', `备份前标记 ${test.beforeMarker} · 排除备份后标记 ${test.afterMarker}`));
    const digest = document.createElement('details');
    digest.append(cell('summary', '归档 SHA-256'), cell('pre', test.archiveSha256));
    record.append(digest);
    target.append(record);
  }
}

const historyMetrics = {
  connections: '连接数', replicationLagBytes: '复制延迟', databaseBytes: '数据库容量', linuxCpuPercent: 'Linux CPU 使用率'
};

async function loadHistoryPolicies() {
  const target = $('history-policies');
  if (currentRole !== 'admin') return;
  target.replaceChildren(cell('p', '正在读取指标保留策略。', 'empty'));
  try {
    const [{ policies }] = await Promise.all([api('/api/admin/history-retention'), loadOrganizations(false)]);
    const form = cell('form', '', 'response-form');
    const org = accessSelect(form, '组织', 'organization', organizationOptions(), [currentOrganization || defaultOrganizationID]);
    const metric = accessSelect(form, '指标', 'metric', Object.entries(historyMetrics), ['connections']);
    const raw = accessInput(form, '原始值保留天数', 'rawDays', '7', { type: 'number', min: 1, max: 90, required: true });
    const hourly = accessInput(form, '小时汇总保留天数', 'hourlyDays', '90', { type: 'number', min: 1, max: 365, required: true });
    const submit = cell('button', '保存保留策略'); submit.type = 'submit';
    form.append(submit);
    form.addEventListener('submit', async event => {
      event.preventDefault();
      submit.disabled = true;
      try {
        await api('/api/admin/history-retention', 'PUT', {
          organizationId: org.value, metric: metric.value, rawDays: raw.valueAsNumber, hourlyDays: hourly.valueAsNumber
        });
        notice('已保存保留策略。');
        await loadHistoryPolicies();
      } catch (error) { notice(error.message, true); submit.disabled = false; }
    });
    target.replaceChildren(form);
    if (!policies.length) target.append(cell('p', '暂无指标分层策略。', 'empty'));
    for (const policy of policies) {
      const line = cell('div', '', 'list-item');
      line.append(cell('span', organizationName(policy.organizationId) + ' · ' + historyMetrics[policy.metric] +
        ' · 原始 ' + policy.rawDays + ' 天 · 小时汇总 ' + policy.hourlyDays + ' 天 · 归档至 ' + formatTime(policy.archiveThrough)));
      const edit = cell('button', '编辑', 'ghost small-button'); edit.type = 'button';
      edit.addEventListener('click', () => {
        org.value = policy.organizationId; metric.value = policy.metric;
        raw.value = policy.rawDays; hourly.value = policy.hourlyDays; raw.focus();
      });
      line.append(edit);
      target.append(line);
    }
  } catch (error) { target.replaceChildren(cell('p', error.message, 'warning')); }
}

function historyTierChart(assetId) {
  const panel = cell('details', '', 'settings-details');
  panel.append(cell('summary', '分层历史趋势'));
  const controls = cell('form', '', 'response-form');
  const metric = accessSelect(controls, '指标', 'metric', Object.entries(historyMetrics), ['connections']);
  const period = accessSelect(controls, '时间范围', 'period', [['7', '最近 7 天'], ['30', '最近 30 天'], ['90', '最近 90 天']], ['30']);
  const apply = cell('button', '查看历史', 'ghost'); apply.type = 'submit'; controls.append(apply);
  const output = cell('div');
  panel.append(controls, output);
  let sequence = 0;
  const load = async (start, end) => {
    const request = ++sequence;
    const hour = 3600000;
    const to = end ?? Math.floor(Date.now() / hour) * hour;
    const from = start ?? to - Number(period.value) * 24 * hour;
    const key = metric.value;
    output.replaceChildren(cell('p', '正在读取分层历史。', 'empty'));
    const query = new URLSearchParams({ metric: key, from: new Date(from).toISOString(), to: new Date(to).toISOString() });
    try {
      const { buckets } = await api('/api/assets/' + assetId + '/history-tier?' + query);
      if (!output.isConnected || sequence !== request) return;
      const format = key === 'linuxCpuPercent' ? value => value.toFixed(1) + '%' :
        key.endsWith('Bytes') ? value => (value / (1024 * 1024)).toFixed(1) + ' MiB' :
        value => value.toLocaleString('zh-CN', { maximumFractionDigits: 1 });
      output.replaceChildren(window.DbSphereMonitoring.historyChart(historyMetrics[key], buckets, from, to, format, load));
    } catch (error) {
      if (sequence === request) output.replaceChildren(cell('p', error.message, 'warning'));
    }
  };
  panel.addEventListener('toggle', () => { if (panel.open && !sequence) void load(); });
  controls.addEventListener('submit', event => { event.preventDefault(); void load(); });
  return panel;
}

const riskMetricLabels = {connections: '连接使用率', capacity: '数据库容量增长', wal: 'WAL 生成速率', recurrence: '重复故障'};
const riskStatusLabels = {risk: '达到预警条件', clear: '基线内', planned: '计划变更窗口', insufficient: '采样不足'};
const riskReasonLabels = {
  threshold_already_reached: '当前值达到预警阈值', rising_towards_threshold: '趋势预计触及预警阈值',
  within_baseline: '当前走势在基线范围内', maintenance_window: '计划变更或维护窗口内暂停判定',
  recent_samples_missing: '近期有效采样不足', seasonal_baseline_missing: '同周期历史样本不足',
  continuous_samples_missing: '相邻时段有效采样不足', similar_failures_this_week: '近七天同类可用性故障达到三次'
};

function assetRiskPanel(assetId) {
  const panel = cell('details', '', 'settings-details risk-panel');
  panel.append(cell('summary', '风险与历史回测'));
  const output = cell('div', '', 'list');
  panel.append(output);
  let loaded = false;
  const path = '/api/assets/' + assetId + '/risk';
  const format = (metric, value) => metric === 'capacity' ? (value / (1024 ** 3)).toFixed(2) + ' GiB' :
    metric === 'wal' ? (value / (1024 ** 2)).toFixed(2) + ' MiB/s' :
    metric === 'connections' ? value.toFixed(1) + '%' : String(value);
  const load = async () => {
    loaded = true;
    try {
      const state = await api(path);
      output.replaceChildren(cell('p', state.enabled ? '预警通知已启用' : '影子预警 · 仅在工作台展示', 'hint'));
      if (currentRole === 'admin') {
        const control = cell('button', state.enabled ? '切换为影子预警' : '启用预警通知', 'ghost small-button');
        control.type = 'button'; control.disabled = !state.enabled && !state.reviewed;
        control.addEventListener('click', async () => {
          try { await api(path + '/mode', 'PUT', {enabled: !state.enabled}); await load(); }
          catch (error) { notice(error.message, true); }
        });
        output.append(control);
      }
      if (!state.assessments.length) output.append(cell('p', '等待收集风险基线的历史采样。', 'empty'));
      const latest = state.assessments[0]?.evaluatedAt;
      for (const item of state.assessments) {
        const forecast = item.forecast;
        if (item.evaluatedAt !== latest && (item.mode !== 'shadow' || item.review || state.enabled)) continue;
        const row = cell('article', '', 'evidence');
        row.append(cell('strong', riskMetricLabels[forecast.metric] + ' · ' + riskStatusLabels[forecast.status]),
          cell('p', formatTime(item.evaluatedAt) + ' · ' + riskReasonLabels[forecast.reason]));
        if (forecast.status === 'risk' || forecast.status === 'clear') {
          const values = ['观测 ' + format(forecast.metric, forecast.observed), '阈值 ' + format(forecast.metric, forecast.threshold)];
          if (forecast.horizonHours) values.push(forecast.horizonHours + ' 小时内预计 ' + format(forecast.metric, forecast.projected));
          if (forecast.leadMinutes > 0) values.push('预计提前 ' + forecast.leadMinutes + ' 分钟');
          row.append(cell('p', values.join(' · ')));
        }
        if (forecast.sampleId) row.append(replaySampleLink(assetId, forecast.sampleId));
        if (item.eventId || forecast.sourceEventId) {
          const event = cell('a', '打开关联事件', 'monitor-link');
          event.href = '/#incidents?event=' + (item.eventId || forecast.sourceEventId);
          row.append(document.createTextNode(' · '), event);
        }
        if (item.review) row.append(cell('p', item.reviewer + ' · ' + (item.review === 'confirmed' ? '确认预警' : '标记误报'), 'hint'));
        if (item.mode === 'shadow' && forecast.status === 'risk' && !item.review && ['admin', 'operator'].includes(currentRole)) {
          const actions = cell('div', '', 'actions');
          for (const [verdict,label] of [['confirmed','确认影子预警'],['false_positive','标记为误报']]) {
            const button = cell('button', label, 'ghost small-button'); button.type = 'button';
            button.addEventListener('click', async () => {
              try { await api(path + '/review', 'POST', {observationId:item.id, verdict}); await load(); }
              catch (error) { notice(error.message, true); }
            });
            actions.append(button);
          }
          row.append(actions);
        }
        output.append(row);
      }
      const backtest = cell('button', '运行近 7 天回测', 'ghost small-button');
      backtest.type = 'button';
      const report = cell('div');
      backtest.addEventListener('click', async () => {
        backtest.disabled = true;
        try {
          const {metrics} = await api(path + '/backtest');
          const table = cell('div', '', 'monitor-table');
          table.append(importTable(['指标','可评估','证据不足','命中','误报','漏报','先导中位（分钟）'],
            metrics.map(item => [riskMetricLabels[item.metric],item.evaluated,item.unknown,item.truePositive,
              item.falsePositive,item.falseNegative,item.truePositive ? item.medianLeadMinutes : '—'])));
          report.replaceChildren(table);
        } catch (error) { report.replaceChildren(cell('p', error.message, 'warning')); }
        finally { backtest.disabled = false; }
      });
      output.append(backtest,report);
    } catch (error) { output.replaceChildren(cell('p', error.message, 'warning')); }
  };
  panel.addEventListener('toggle', () => { if (panel.open && !loaded) void load(); });
  return panel;
}

async function loadPlatformStatus() {
  const target = $('platform-status-detail');
  try {
    const { replicas, maintenance } = await api('/api/admin/platform');
    const owner = replicas.find(replica => replica.id === maintenance?.owner);
    target.replaceChildren(cell('p', maintenance ? `维护主节点 ${owner?.name || maintenance.owner} · 第 ${maintenance.generation} 任 · 租约到期 ${formatTime(maintenance.leaseUntil)}` : '等待选出维护主节点。', 'hint'));
    if (replicas.length) {
      const table = cell('div', '', 'monitor-table');
      table.append(importTable(['副本', '运行状态', '自动采集', '本进程采集轮次', '最近心跳'], replicas.map(replica => [
        `${replica.name} · ${replica.id.slice(0, 8)}`, replica.online ? '在线' : '离线',
        replica.collector ? '启用' : '关闭', replica.completedCycles, formatTime(replica.seenAt)
      ])));
      target.append(table);
    } else target.append(cell('p', '尚无平台注册记录。', 'empty'));
  } catch (error) { target.replaceChildren(cell('p', error.message, 'warning')); }
}

function renderCollectorStatus(status) {
  const target = $('collector-status-detail'); target.replaceChildren();
  if (!status) { target.append(cell('p', '当前进程的采集状态暂时不可用。', 'warning')); return; }
  const labels = { disabled: '自动采集已关闭', starting: '等待首轮采集', running: '正在采集',
    overdue: '本轮已超过两个采集周期', healthy: '采集循环正常',
    degraded: '最近一轮有采集器错误', stalled: '采集循环未按周期推进' };
  target.append(cell('p', labels[status.state] ?? '状态未知',
    ['overdue', 'degraded', 'stalled'].includes(status.state) ? 'warning' : 'hint'));
  const grid = document.createElement('div'); grid.className = 'metric-grid';
  for (const [label, value] of [
    ['采集周期 / 并发', `${status.intervalSeconds} 秒 / ${status.workerLimit} 个`],
    ['进程启动', formatTime(status.processStartedAt)],
    ['当前轮开始', status.activeStartedAt ? formatTime(status.activeStartedAt) : '未运行'],
    ['最近一轮开始', formatTime(status.cycleStartedAt)],
    ['最近一轮结束', formatTime(status.cycleFinishedAt)],
    ['最近一轮耗时', status.cycleFinishedAt ? `${status.cycleDurationMs} ms` : '尚无结果'],
    ['本进程完成轮次', status.completedCycles],
    ['最近一轮选中 / 实际处理', `${status.cycle?.selected ?? 0} / ${status.cycle?.processed ?? 0}`],
    ['最近一轮目标失败', status.cycle?.targetFailures ?? 0],
    ['最近一轮执行错误', status.cycle?.collectorErrors ?? 0]
  ]) {
    const box = document.createElement('div'); box.append(cell('span', label), cell('strong', String(value))); grid.append(box);
  }
  target.append(grid);
  if (status.cycle?.dueScanFailed) target.append(cell('p', '最近一轮未能读取待采集资产。', 'warning'));
  if (status.cycle?.atBatchLimit) target.append(cell('p', `最近一轮领取了 ${status.batchLimit} 个任务。`, 'hint'));
  target.append(cell('p', '运行轮次按当前进程统计。下方队列保存任务、租约与重试状态。', 'hint'));
  const queue = cell('div', '', 'monitor-table');
  queue.setAttribute('aria-live', 'polite');
  const loadQueue = cell('button', '查看采集队列', 'ghost small-button');
  loadQueue.addEventListener('click', async () => {
    loadQueue.disabled = true;
    try {
      const { counts, jobs } = await api('/api/admin/collector/queue');
      const labels = { due: '到期', delayed: '调度延迟', missing: '尚无采样', running: '正在采集', backoff: '退避重试', scheduled: '已排期' };
      const outcomes = { ok: '采样成功', target_error: '目标采集失败', execution_error: '采集器执行失败' };
      queue.replaceChildren(cell('p', Object.entries(labels).map(([key, label]) => `${label} ${counts[key]}`).join(' · '), 'hint'));
      if (jobs.length) queue.append(importTable(['实例', '调度状态', '最近结果', '下次到期', '租约截止', '连续失败', '采样'], jobs.map(job => [
        job.name, labels[job.state], outcomes[job.outcome] ?? '待执行', formatTime(job.dueAt), formatTime(job.leaseUntil), job.failures,
        job.lastSampleId ? replaySampleLink(job.assetId, job.lastSampleId) : '—'
      ])));
      if (counts.total > jobs.length) queue.append(cell('p', `显示优先级最高的 ${jobs.length} 个任务，共 ${counts.total} 个。`, 'hint'));
    } catch (error) { queue.replaceChildren(cell('p', error.message, 'warning')); }
    finally { loadQueue.disabled = false; }
  });
  target.append(loadQueue, queue);
  const loadTrend = cell('button', '查看小时汇总进度', 'ghost small-button');
  const trendReport = cell('p', '', 'hint');
  trendReport.setAttribute('aria-live', 'polite');
  loadTrend.addEventListener('click', async () => {
    loadTrend.disabled = true;
    try {
      const trend = await api('/api/admin/collector/hourly-trends');
      const window = trend.from === trend.through ? '等待首次计算' : `${formatTime(trend.from)} 至 ${formatTime(trend.through)}`;
      trendReport.textContent = `小时汇总 · ${window} · 待补算 ${trend.pendingHours} 小时 · 待重算 ${trend.dirtyHours} 小时`;
    } catch (error) { trendReport.textContent = error.message; }
    finally { loadTrend.disabled = false; }
  });
  target.append(loadTrend, trendReport);
  const loadCoverage = cell('button', '查看近 7 天采集覆盖', 'ghost small-button');
  const report = document.createElement('div');
  loadCoverage.addEventListener('click', async () => {
    loadCoverage.disabled = true;
    try {
      const coverage = await api('/api/admin/collector/coverage');
      report.replaceChildren(cell('p', `当前资产 ${coverage.targets} · 已到采集时间 ${coverage.due} · 近两周期有效 ${coverage.fresh}`, 'hint'));
      const rows = coverage.days.map(day => [day.day.slice(0, 10), day.observed, day.samples, day.postgresOK,
        day.samples ? `${(100 * day.failed / day.samples).toFixed(1)}%` : '无样本', `${(day.sampleBytes / 1048576).toFixed(2)} MiB`]);
      report.append(importTable(['日期 UTC', '覆盖资产', '采样数', 'PG 有效采样', '失败占比', '指标载荷'], rows));
    } catch (error) { report.replaceChildren(cell('p', error.message, 'warning')); }
    finally { loadCoverage.disabled = false; }
  });
  report.className = 'monitor-table';
  target.append(loadCoverage, report);
}

function renderRules(rules) {
  const list = $('rule-list'); list.replaceChildren();
  for (const rule of rules) {
    const details = document.createElement('details'); details.className = 'rule-card';
    details.append(cell('summary', `${rule.enabled ? '启用' : '停用'} · ${rule.title} · ${rule.metric} ${rule.operator} ${formatAlertValue(rule.metric, rule.threshold)} · 触发 ${rule.durationSeconds}s · 恢复 ${rule.recoverySeconds || 0}s`));
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
    const recovery = duration.cloneNode(); recovery.value = rule.recoverySeconds || 0;
    const severity = document.createElement('select');
    for (const value of ['info', 'warning', 'critical']) {
      const option = document.createElement('option'); option.value = value; option.textContent = value;
      severity.append(option);
    }
    severity.value = rule.severity;
    const enabled = document.createElement('input'); enabled.type = 'checkbox'; enabled.checked = rule.enabled;
    for (const [label, input] of [[rule.metric === growthMetric ? '阈值（GiB/日）' : '阈值', threshold], ['触发持续秒数', duration],
      ['恢复持续秒数', recovery], ['级别', severity], ['启用', enabled]]) {
      const wrapper = document.createElement('label'); wrapper.append(cell('span', label), input); form.append(wrapper);
    }
    const save = cell('button', '保存规则', 'small-button'); save.type = 'submit'; form.append(save);
    form.addEventListener('submit', async event => {
      event.preventDefault();
      try {
        await api(`/api/alert-rules/${rule.id}`, 'PATCH', { threshold: Number(threshold.value) * (rule.metric === growthMetric ? 1024 ** 3 : 1),
          durationSeconds: Number(duration.value), recoverySeconds: Number(recovery.value), severity: severity.value, enabled: enabled.checked });
        notice(`${rule.title} 已更新；新配置将在下次采样时生效。`);
        document.activeElement?.blur(); await refresh();
      } catch (error) { notice(error.message, true); }
    });
    details.append(form); list.append(details);
  }
}

function importTable(headings, rows) {
  const table = document.createElement('table');
  const head = table.createTHead().insertRow();
  for (const label of headings) {
    const field = cell('th', label); field.scope = 'col'; head.append(field);
  }
  const body = table.createTBody();
  for (const values of rows) {
    const row = body.insertRow();
    for (const value of values) {
      const field = row.insertCell();
      if (value instanceof Node) field.append(value); else field.textContent = value;
    }
  }
  return table;
}

function setupAssetImport() {
  const form = $('asset-import-form'), apply = $('asset-import-apply'), probe = $('asset-import-probe');
  const output = $('asset-import-plan'), results = $('asset-import-results'), status = $('asset-import-status');
  const download = $('asset-import-export');
  const cnpgForm = $('cnpg-preview-form'), cnpgResults = $('cnpg-preview-results'), cnpgFill = $('cnpg-preview-fill');
  let plan, checkedIDs = [], nextExport = '', exportPage = 1;
  let cnpgCandidates = [];
  cnpgForm.addEventListener('submit', async event => {
    event.preventDefault();
    cnpgFill.hidden = true;
    const context = cnpgForm.elements.context.value.trim(), namespace = cnpgForm.elements.namespace.value.trim();
    const submit = event.submitter;
    if (submit) submit.disabled = true;
    cnpgResults.replaceChildren(cell('p', '正在读取 CloudNativePG Cluster', 'hint'));
    try {
      const result = await api(`/api/assets/import/cloudnativepg?${new URLSearchParams({context, namespace})}`);
      if (result.status !== 'ok') throw Error(result.reason || '集群观测暂不可用');
      cnpgCandidates = result.clusters.filter(cluster => cluster.readWriteService).map(cluster => ({
        name: cluster.name, host: `${cluster.readWriteService}.${namespace}.svc`, port: 5432, database: 'postgres',
        runtime: 'k8s', clusterName: cluster.name, k8sCluster: context, namespace, workload: cluster.name
      }));
      cnpgResults.replaceChildren();
      if (result.clusters.length) cnpgResults.append(importTable(['名称', 'Cluster UID', '主库 / 目标', '就绪实例', '读写 Service', '阶段'],
        result.clusters.map(cluster => [cluster.name, cluster.uid, `${cluster.currentPrimary || '未知'} / ${cluster.targetPrimary || '未知'}`,
          `${cluster.readyInstances ?? '—'} / ${cluster.instanceCount ?? '—'}`, cluster.readWriteService || '—', cluster.phase || '未知'])));
      else cnpgResults.append(cell('p', '该命名空间没有可读取的 CloudNativePG 集群。', 'hint'));
      if (result.truncated) cnpgResults.append(cell('p', '显示当前命名空间的前 100 个 Cluster。', 'hint'));
      cnpgFill.hidden = !cnpgCandidates.length;
    } catch (error) { cnpgResults.replaceChildren(cell('p', error.message, 'warning')); }
    finally { if (submit) submit.disabled = false; }
  });
  cnpgFill.addEventListener('click', () => {
    form.elements.format.value = 'yaml';
    form.elements.content.value = JSON.stringify({assets: cnpgCandidates}, null, 2);
    form.elements.discover.checked = true;
    form.dispatchEvent(new Event('input'));
    form.elements.content.focus();
  });
  const payload = () => ({ format: form.elements.format.value, content: form.elements.content.value,
    username: form.elements.username.value.trim(), credentialRef: form.elements.credentialRef.value.trim(),
    discover: form.elements.discover.checked });
  const labels = { create: '新增', update: '更新', unchanged: '保持', conflict: '冲突', invalid: '无效' };
  const print = response => output.replaceChildren(importTable(['资产', '数据库目标', '变更', '核对项'], response.rows.map(row => {
    const discovery = row.discovery;
    const location = discovery?.status === 'observed'
      ? `${discovery.namespace}/${discovery.pod} · ${discovery.port} ${discovery.portDeclared ? '已声明' : '未声明'}`
      : discovery ? `${discovery.namespace || '未知命名空间'} · 未取得 Kubernetes 观测` : '';
    return [row.name || `第 ${row.index} 项`, row.endpoint, labels[row.action],
      [row.reason, row.changes?.join('、'), location,
        discovery?.workloadIdentity?.operator && `CloudNativePG · ${discovery.workloadIdentity.uid}`].filter(Boolean).join(' · ')];
  })));
  form.addEventListener('input', () => { plan = null; apply.disabled = true; output.replaceChildren(); });
  form.addEventListener('submit', async event => {
    event.preventDefault();
    const input = payload();
    apply.disabled = true;
    status.textContent = '正在预览清单';
    try {
      const response = await api('/api/assets/import/preview', 'POST', input);
      if (JSON.stringify(input) !== JSON.stringify(payload())) { status.textContent = '清单已修改，请重新预览'; return; }
      plan = { ...input, revision: response.revision };
      print(response);
      apply.disabled = !response.canApply;
      status.textContent = `${response.rows.length} 项 · ${response.canApply ? '可应用' : '请修正冲突或无效项'}`;
    } catch (error) { status.textContent = error.message; }
  });
  apply.addEventListener('click', async () => {
    if (!plan) return;
    apply.disabled = true;
    try {
      const response = await api('/api/assets/import/apply', 'POST', plan);
      print(response);
      checkedIDs = response.rows.map(row => row.id);
      nextExport = ''; exportPage = 1; download.textContent = '导出 YAML';
      probe.textContent = '分批检查连接与权限'; probe.hidden = false;
      results.replaceChildren();
      status.textContent = `已应用 ${checkedIDs.length} 项`;
      await refresh();
    } catch (error) { status.textContent = error.message; }
    finally { plan = null; }
  });
  probe.addEventListener('click', async () => {
    probe.disabled = true; apply.disabled = true;
    const failed = [], rows = [];
    for (let offset = 0; offset < checkedIDs.length; offset += 5) {
      const ids = checkedIDs.slice(offset, offset + 5);
      status.textContent = `正在检查 ${offset + 1}–${offset + ids.length} / ${checkedIDs.length}`;
      try {
        const response = await api('/api/assets/import/probe', 'POST', { ids });
        for (const result of response.results) {
          const pg = result.postgres;
          if (result.status !== 'ok' && pg?.pgStatus !== 'ok') failed.push(result.id);
          const evidence = result.sampleId ? replaySampleLink(result.id, result.sampleId) : '无样本';
          rows.push([result.id, pg?.pgStatus === 'ok' ? 'PostgreSQL 已连接' : '连接或基础采集未通过',
            pg?.monitorRole === true ? 'pg_monitor' : pg?.monitorRole === false ? '缺少 pg_monitor' : '未知',
            pg?.reason || (result.status !== 'ok' && pg?.pgStatus === 'ok' ? '可选采集源异常' : ''), evidence]);
        }
      } catch (error) { failed.push(...ids); rows.push([ids.join('、'), '批次未完成', '', error.message, '']); }
      results.replaceChildren(importTable(['资产 ID', '连接', '权限', '状态说明', '证据'], rows));
    }
    checkedIDs = failed;
    status.textContent = failed.length ? `${failed.length} 项待重试` : '连接检查完成';
    probe.textContent = '重试失败项'; probe.hidden = !failed.length; probe.disabled = false;
  });
  download.addEventListener('click', async () => {
    download.disabled = true;
    try {
      const result = await api(`/api/assets/import/export?after=${encodeURIComponent(nextExport)}`);
      const url = URL.createObjectURL(new Blob([JSON.stringify({ assets: result.assets }, null, 2)], {type:'application/yaml'}));
      const link = document.createElement('a'); link.href = url; link.download = `dbsphere-assets-${exportPage}.yaml`; link.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      nextExport = result.next; exportPage = nextExport ? exportPage + 1 : 1;
      download.textContent = nextExport ? '导出下一批 YAML' : '导出 YAML';
      status.textContent = `已导出 ${result.assets.length} 项及凭据引用`;
    } catch (error) { status.textContent = error.message; }
    finally { download.disabled = false; }
  });
}

setupAssetImport();

function editAsset(asset) {
  editingAsset = asset.id;
  const form = $('asset-form');
  for (const option of $('runtime').options) option.disabled = (option.value === 'mysql') !== (asset.runtime === 'mysql');
  for (const [field, value] of Object.entries({
    name: asset.name, host: asset.host, port: asset.port, database: asset.database,
    username: asset.username, sslMode: asset.sslMode, runtime: asset.runtime,
    linuxHost: asset.linuxHost, linuxExporterPort: asset.linuxExporterPort, k8sCluster: asset.k8sCluster, namespace: asset.namespace,
    workload: asset.workload, pod: asset.pod, node: asset.node,
    poolerEndpoint: asset.poolerEndpoint, clusterName: asset.clusterName, environment: asset.environment,
    tags: asset.tags?.join(', ')
  })) form.elements[field].value = value ?? '';
  form.elements.password.value = '';
  form.elements.poolerUrl.value = '';
  form.elements.poolerClear.checked = false;
  form.elements.patroniUrl.value = '';
  form.elements.patroniClear.checked = false;
  $('runtime').dispatchEvent(new Event('change'));
  $('asset-submit').textContent = '更新资产';
  $('asset-dialog').dataset.mode = 'edit';
  $('asset-dialog-title').textContent = asset.runtime === 'mysql' ? '编辑 MySQL 实例' : '编辑 PostgreSQL 实例';
  $('asset-error').textContent = '';
  if (!$('asset-dialog').open) $('asset-dialog').showModal();
}

function resetEdit() {
  if ($('asset-dialog').open) $('asset-dialog').close();
  editingAsset = null;
  $('asset-dialog-title').textContent = '纳管数据库实例';
  $('asset-error').textContent = '';
  for (const option of $('runtime').options) option.disabled = false;
  $('asset-form').reset();
  $('asset-organization-field').hidden = true;
  $('asset-organization-field').querySelector('select').disabled = true;
  $('runtime').dispatchEvent(new Event('change'));
  $('asset-submit').textContent = '保存资产';
  $('asset-dialog').dataset.mode = 'new';
  $('asset-integrations').open = false;
}

const replayLayers = { app: '应用 Trace', postgres: 'PostgreSQL', mysql: 'MySQL', sql: 'SQL', linux: 'Linux', k8s: 'Kubernetes', replication: '复制与高可用', notification: '通知投递',
  pooler: 'PgBouncer', response: '处置', ai: 'AI 辅助' };
const replayStrength = { direct: '直接证据', correlated: '时间相关', conflict: '标识不符', insufficient: '证据不足', record: '事件记录' };
const replayKinds = { trace_span: '应用 Span', event_started: '事件开始', event_ended: '事件关闭', alert_firing: '告警触发', alert_recovered: '告警恢复',
  ai_evidence_review: '模型证据审核',
  risk_forecast: '无模型风险预警', risk_forecast_cleared: '风险信号回落',
  routing_observed: '入口诊断更新',
  alert_recovery_pending: '开始观察恢复', alert_recovery_cancelled: '异常再次出现',
  notification_delivery: '通知尝试', notification_retry: '通知重试',
  external_alert_firing: '外部告警触发', external_alert_resolved: '外部告警恢复', external_ticket_linked: '外部工单关联',
  collection_error: '采集发现异常', collection_recovered: '采样已恢复', mysql_lock_wait: 'InnoDB 锁等待', recovery_verified: '人工验证恢复',
  linux_state: 'Linux 采集', k8s_state: 'Kubernetes 采集', pgbouncer_state: 'PgBouncer 采集', patroni_state: 'Patroni 采集',
  event_acknowledged: '事件确认', event_handoff: '值班交接', operator_response: '处置记录', rca_decision: '人工复核', ai_advisory: 'AI 分析建议' };

function evidenceReviewPanel(event, showSource) {
  const panel = cell('details', '', 'settings-details ai-evidence-panel');
  panel.append(cell('summary', '证据助手'));
  const output = cell('div', '', 'list');
  panel.append(output);
  const path = '/api/events/' + event.id + '/ai-evidence-review';
  const kinds = {candidate: '候选解释', counterevidence: '反证', verification: '核查建议'};
  const status = {
    no_model: '规则核查 · 模型未接入', ready: '规则核查 · 可请求模型审核', model: '模型审核已记录',
    model_saved: '上次模型审核', model_unavailable: '模型暂不可用 · 显示规则核查',
    invalid_citations: '模型引用无法验证 · 显示规则核查', insufficient: '证据不足',
    budget_exhausted: '当日模型预算已用完 · 显示规则核查'
  };
  let loaded = false;
  const load = async (method = 'GET') => {
    loaded = true;
    try {
      const review = await api(path, method, method === 'POST' ? {} : undefined);
      output.replaceChildren(cell('p', status[review.status] || review.status, 'hint'),
        cell('small', '事件证据 ' + review.sources.length + ' 项 · ' + review.inputBytes + '/' + review.maxInputBytes +
          ' 字节 · 每日模型请求上限 ' + review.dailyLimit, 'hint'));
      const sources = new Map(review.sources.map(source => [source.id,source]));
      for (const claim of review.result.claims) {
        const row = cell('article', '', 'evidence');
        row.append(cell('strong', kinds[claim.kind] + (claim.origin === 'rule' ? ' · 规则补充' : '')), cell('p', claim.text));
        const actions = cell('div', '', 'actions');
        for (const id of claim.evidenceIds) {
          const source = sources.get(id);
          if (!source) continue;
          const link = cell('button', '证据 ' + id.slice(0,8), 'monitor-link');
          link.type = 'button';
          link.title = id + ' · ' + formatTime(source.observedAt);
          link.addEventListener('click', () => showSource(id));
          actions.append(link);
          if (source.sampleId) actions.append(replaySampleLink(event.assetId,source.sampleId));
        }
        row.append(actions);
        output.append(row);
      }
      if (review.result.uncertainty) output.append(cell('p', review.result.uncertainty, 'hint'));
      if (review.canRequest && review.status !== 'budget_exhausted') {
        const analyze = cell('button', '请求模型审核', 'ghost small-button');
        analyze.type = 'button';
        analyze.addEventListener('click', () => { analyze.disabled = true; analyze.textContent = '审核中'; void load('POST'); });
        output.append(analyze);
      }
    } catch (error) { output.replaceChildren(cell('p', error.message, 'warning')); }
  };
  panel.addEventListener('toggle', () => { if (panel.open && !loaded) void load(); });
  return panel;
}

function replaySampleLink(assetId, sampleId) {
  const link = cell('a', '原始采样', 'monitor-link');
  link.href = `/api/assets/${assetId}/samples/${sampleId}`;
  link.target = '_blank'; link.rel = 'noopener';
  return link;
}

function incidentReplayPanel(event, replay, analysis, aiEnabled) {
  const panel = cell('section', '', 'incident-replay');
  panel.append(cell('h4', '全链路故障回放'));
  const jump = point => {
    const topic = point.signal === 'pg.blocked_sessions' ? 'locks' : point.signal === 'pg.archive_fail_delta' ? 'backup' : { k8s: 'infra', linux: 'infra', replication: 'replication',
      pooler: 'replication', sql: 'sql' }[point.layer] || 'metrics';
    monitoring.openIncident(event, topic, point);
    setView('monitoring');
  };
  const pointActions = point => {
    const actions = cell('div', '', 'incident-replay-actions');
    if (assetServerPaging && point.stage !== 'response' && !point.traceId) {
      const button = cell('button', '定位同窗', 'ghost small-button');
      button.type = 'button'; button.addEventListener('click', () => jump(point));
      actions.append(button);
    }
    if (point.sampleId) actions.append(replaySampleLink(event.assetId, point.sampleId));
    if (point.traceId) {
      const link = cell('a', '原始 Trace', 'monitor-link');
      link.href = `/api/assets/${event.assetId}/traces/${point.traceId}`;
      link.target = '_blank'; link.rel = 'noopener'; actions.append(link);
    }
    return actions;
  };
  const milestones = cell('div', '', 'incident-replay-milestones');
  const milestone = (label, point, empty) => {
    const item = cell('div', '', 'incident-replay-milestone');
    item.append(cell('span', label), cell('strong', point ? formatTime(point.at) : empty));
    if (point) item.append(cell('small', replayKinds[point.kind] || point.kind), pointActions(point));
    return item;
  };
  const layers = Object.keys(replay.observedLayers).map(key => replayLayers[key]).filter(Boolean);
  if (!layers.includes('应用 Trace') && analysis.traceCoverage?.spans) layers.unshift('应用 Trace');
  const scope = cell('div', '', 'incident-replay-milestone');
  scope.append(cell('span', '影响实例'), cell('strong', event.assetName || event.assetId),
    cell('small', layers.length ? layers.join(' · ') : '暂缺有原始采样的信号'),
    cell('small', aiEnabled ? '规则诊断 · 已启用可选 AI' : '规则诊断 · AI 未启用'));
  milestones.append(milestone('首次发现', replay.detection, '待确认'), scope, milestone('恢复观测', replay.recovery, '仍在处理中'));
  panel.append(milestones);

  const phases = { before: '故障前', during: '故障期间', after: '恢复后' };
  if (replay.comparison?.length) {
    const values = [ ['connections', '连接'], ['blockedSessions', '阻塞会话'], ['poolWaiting', '连接池等待'],
      ['replicationLagBytes', '回放差距（字节）'], ['podUnready', 'Pod 就绪'] ];
    const comparison = cell('div', '', 'monitor-table');
    comparison.append(cell('h5', '故障前、触发与恢复采样对照'), importTable(['阶段', '记录时间', ...values.map(item => item[1]), '证据'],
      replay.comparison.map(row => [phases[row.phase], row.at ? formatTime(row.at) : '缺少可比采样',
        ...values.map(([key]) => key === 'podUnready' && row.metrics?.[key] != null
          ? (row.metrics[key] ? '未就绪' : '就绪') : row.metrics?.[key] ?? '—'),
        row.sampleId ? replaySampleLink(event.assetId, row.sampleId) : '—'])));
    panel.append(comparison);
  }

  const body = cell('div', '', 'incident-replay-rows');
  const toolbar = cell('div', '', 'incident-replay-actions');
  const label = cell('label', '证据来源 ');
  const filter = cell('select', ''); filter.setAttribute('aria-label', '回放证据来源');
  for (const [key, title] of [['', '全部'], ...Object.entries(replayLayers).filter(([key]) => replay.points.some(point => point.layer === key))]) {
    const option = cell('option', title); option.value = key; filter.append(option);
  }
  label.append(filter); toolbar.append(label); panel.append(toolbar, body);
  const filterBy = (name, values) => {
    const field = cell('select'); field.setAttribute('aria-label', name);
    for (const [value, title] of [['', '全部'], ...Object.entries(values)]) {
      const option = cell('option', title); option.value = value; field.append(option);
    }
    const caption = cell('label', `${name} `); caption.append(field); toolbar.append(caption);
    return field;
  };
  const phase = filterBy('回放阶段', phases);
  const strength = filterBy('证据判断', replayStrength);
  const state = new URLSearchParams(location.hash.split('?')[1]);
  const filters = { layer: filter, phase, strength };
  for (const [key, field] of Object.entries(filters)) {
    const value = state.get(key) || '';
    if ([...field.options].some(option => option.value === value)) field.value = value;
  }
  const share = cell('button', '复制当前证据链接', 'ghost small-button');
  const shareURL = cell('input'); shareURL.type = 'url'; shareURL.readOnly = true; shareURL.hidden = true;
  shareURL.setAttribute('aria-label', '当前证据链接');
  share.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(location.href); share.textContent = '已复制证据链接'; }
    catch { shareURL.hidden = false; shareURL.value = location.href; shareURL.select(); }
  });
  toolbar.append(share, shareURL);
  let offset = 0;
  const pageSize = 8;
  const show = () => {
    const points = replay.points.filter(point => Object.entries(filters).every(([key, field]) => !field.value || point[key] === field.value));
    body.replaceChildren();
    for (const point of points.slice(offset, offset + pageSize)) {
      const row = cell('div', '', 'incident-replay-row');
      const text = cell('div', '');
      text.append(cell('strong', `${replayLayers[point.layer]} · ${replayKinds[point.kind] || point.kind}`),
        cell('small', [formatTime(point.at), phases[point.phase], point.source, point.signal, replayStrength[point.strength]].filter(Boolean).join(' · ')));
      if (point.limitation) text.append(cell('small', point.limitation));
      row.dataset.evidenceId = point.evidenceId || '';
      row.append(text, pointActions(point));
      body.append(row);
    }
    if (!points.length) body.append(cell('p', '当前事件没有相关的原始信号记录。', 'empty'));
    if (points.length <= pageSize) return;
    const nav = cell('div', '', 'incident-replay-actions');
    const previous = cell('button', '上一页证据', 'ghost small-button');
    const next = cell('button', '下一页证据', 'ghost small-button');
    previous.disabled = offset === 0; next.disabled = offset + pageSize >= points.length;
    previous.addEventListener('click', () => { offset -= pageSize; show(); });
    next.addEventListener('click', () => { offset += pageSize; show(); });
    nav.append(previous, cell('small', `${offset + 1}–${Math.min(points.length, offset + pageSize)} / ${points.length}`), next);
    body.append(nav);
  };
  for (const field of Object.values(filters)) field.addEventListener('change', () => {
    const params = new URLSearchParams({ event: event.id });
    for (const [key, input] of Object.entries(filters)) if (input.value) params.set(key, input.value);
    history.replaceState(history.state, '', `#incidents?${params}`);
    share.textContent = '复制当前证据链接'; shareURL.hidden = true;
    offset = 0; show();
  });
  show();
  return panel;
}

function traceRequestEvidence(paths, assetId, limited) {
  const section = cell('details', '', 'evidence');
  section.append(cell('summary', `查看数据库请求路径（${paths.length}）`));
  const parentStates = { complete: '父 Span 齐全', database_root: '数据库调用是当前根节点', missing_parent: '上游父 Span 未取得', cycle: '父子关系出现循环', depth_limit: '达到路径深度上限' };
  const backendStates = { matched: '等待 PID 匹配', mismatch: '调用 PID 与所见等待 PID 不符', no_lock_evidence: '调用窗内无可核对的锁样本', pid_unreported: '应用未上报后端 PID', time_unreliable: '数据库 Span 时间异常' };
  const poolerStates = { mapped_endpoint: '命中配置的 PgBouncer 入口', pool_sample_observed: '命中 PgBouncer 入口且调用窗内有连接池采样', time_unreliable: '代理采样的时间关联待核对' };
  for (const path of paths) {
    const card = cell('details', '', 'evidence');
    const services = path.steps.filter(step => step.kind !== 'postgresql_client').map(step => step.service)
      .filter((service, index, values) => index === 0 || service !== values[index - 1]);
    card.append(cell('summary', [...services, ...(path.route === 'pgbouncer' ? ['PgBouncer'] : []), 'PostgreSQL'].join(' → ')));
    const status = [parentStates[path.parentState], path.clockAnomaly ? '路径包含时间异常' : '', poolerStates[path.poolerState], backendStates[path.backendState]].filter(Boolean);
    card.append(cell('p', status.join(' · ')));
    const metrics = [path.backendPid ? `上报后端 PID ${path.backendPid}` : '', path.blockerPid ? `阻塞 PID ${path.blockerPid}` : ''];
    if (metrics.some(Boolean)) card.append(cell('small', metrics.filter(Boolean).join(' · ')));
    const table = importTable(['服务 / 调用', '开始时间', '耗时 ms', '采样声明', '时间检查'], path.steps.map(step => [
      step.kind === 'postgresql_client' ? `${step.service} · PostgreSQL` : step.service, formatTime(step.startedAt), String(step.durationMs),
      step.samplingProbability == null ? '未声明' : `${Number((step.samplingProbability * 100).toPrecision(4))}%`,
      { unreliable: '时间异常', unavailable: '接收时间缺失', observed: '接收时间可核对' }[step.clock]
    ]));
    table.setAttribute('aria-label', '多服务请求证据');
    const wrap = cell('div', '', 'monitor-table'); wrap.append(table); card.append(wrap);
    const refs = cell('div', '', 'actions');
    for (const [label, endpoint, id] of [['原始请求 Trace', 'traces', path.traceId], ['后端会话证据', 'samples', path.backendSampleId], ['连接池采样', 'samples', path.poolerSampleId]]) {
      if (!id) continue;
      const link = cell('a', label, 'monitor-link'); link.href = `/api/assets/${assetId}/${endpoint}/${id}`; refs.append(link);
    }
    card.append(refs); section.append(card);
  }
  if (limited) section.append(cell('small', '此处显示优先核对的 12 条路径。'));
  return section;
}

function incidentImpactPanel(impact, event) {
  const panel = cell('section', '', 'evidence incident-impact');
  const members = impact.members || [];
  const history = impact.history || [];
  const resolved = members.filter(item => item.status === 'resolved').length;
  const open = members.filter(item => item.status === 'open').length;
  panel.append(cell('h4', '跨实例影响范围'),
    cell('p', `${impact.truncated ? '当前展示 ' : ''}${members.length} 个实例事件 · ${open} 个处理中 · ${resolved} 个已恢复`, 'hint'));
  const eventLink = (id, label) => {
    const link = cell('a', label, 'monitor-link');
    link.href = `#incidents?event=${encodeURIComponent(id)}`;
    return link;
  };
  for (const member of members) {
    const row = cell('div', '', 'evidence impact-member');
    const prefix = member.eventId === impact.primaryEventId ? '主事件' : '子事件';
    row.append(eventLink(member.eventId, `${prefix} · ${member.assetName} · ${member.title}`),
      cell('span', `${member.cluster ? member.cluster + ' · ' : ''}${member.status === 'resolved' ? '已恢复' : member.status === 'open' ? '处理中' : member.status}`, 'hint'));
    if (member.dependency) {
      const observed = cell('p', `${member.dependency.entry} · ${routingStatus[member.dependency.signal] || member.dependency.signal} · `, 'hint');
      observed.append(replaySampleLink(member.assetId, member.dependency.sampleId));
      row.append(observed);
    }
    if (member.services?.length) row.append(cell('p', `同窗数据库失败调用 · ${member.services.map(service => `${service.name} ${service.failures} 次`).join(' · ')}`));
    panel.append(row);
  }
  if (history.length) {
    const details = cell('details', '', 'settings-details');
    details.append(cell('summary', '关联与拆分历史'));
    for (const item of history) {
      const line = cell('p', '', 'impact-history');
      line.append(eventLink(item.parentEventId, item.parentAsset), document.createTextNode(' → '),
        eventLink(item.childEventId, item.childAsset), document.createTextNode(
          ` · ${item.source === 'dependency' ? '共同入口观测' : '人工关联'} · ${formatTime(item.linkedAt)} · ${item.reason}`));
      if (item.unlinkedAt) line.append(document.createTextNode(` · 已拆分 ${formatTime(item.unlinkedAt)} · ${item.unlinkReason}`));
      details.append(line);
    }
    panel.append(details);
  }
  if (currentRole === 'admin') {
    const details = cell('details', '', 'settings-details');
    details.append(cell('summary', '管理事件关联'));
    const form = cell('form', '', 'form-grid routing-form');
    const field = (labelText, control) => {
      const label = cell('label', labelText);
      control.setAttribute('aria-label', labelText);
      label.append(control);
      form.append(label);
    };
    const action = document.createElement('select');
    for (const [value, label] of [['link', '关联子事件'], ['split', '拆分子事件']]) {
      const option = cell('option', label); option.value = value; action.append(option);
    }
    const parent = document.createElement('input');
    const child = document.createElement('input');
    const reason = document.createElement('input');
    parent.required = child.required = reason.required = true;
    parent.maxLength = child.maxLength = 36; reason.maxLength = 200;
    parent.value = impact.primaryEventId;
    if (event.id !== impact.primaryEventId) child.value = event.id;
    field('操作', action); field('主事件 ID', parent); field('子事件 ID', child); field('关联或拆分原因', reason);
    const submit = cell('button', '保存关联操作');
    submit.type = 'submit';
    form.append(submit);
    form.addEventListener('submit', async e => {
      e.preventDefault();
      const primary = parent.value.trim(), secondary = child.value.trim();
      if (![primary, secondary].every(id => /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(id))) {
        notice('请填写有效的主事件和子事件 ID。', true);
        return;
      }
      submit.disabled = true;
      try {
        await api(`/api/events/${primary}/impact`, 'POST',
          {action: action.value, childEventId: secondary, reason: reason.value.trim()});
        notice(action.value === 'link' ? '事件已关联。' : '事件已拆分。');
        if (selectedEvent === event.id) await showEvent(event.id, false);
      } catch (error) {
        notice(error.message, true);
      } finally {
        submit.disabled = false;
      }
    });
    details.append(form); panel.append(details);
  }
  return panel;
}

function eventSharingPanel(eventId) {
  const panel = cell('details', '', 'settings-details');
  panel.append(cell('summary', '事件分享'));
  const listing = cell('div', '', 'list');
  const form = cell('form', '', 'response-form operator-action');
  const hours = accessInput(form, '链接有效期（小时）', 'hours', '24', { type: 'number', min: 1, max: 168, required: true });
  const create = cell('button', '创建事件链接', 'ghost small-button');
  create.type = 'submit';
  form.append(create);
  panel.append(cell('p', '接收者登录后需要具备该事件资产的访问权限。', 'hint'), form, listing);
  const path = `/api/events/${eventId}/shares`;
  const load = async () => {
    try {
      const { shares = [] } = await api(path);
      listing.replaceChildren();
      if (!shares.length) listing.append(cell('p', '当前没有有效的分享链接。', 'empty'));
      for (const share of shares) {
        const row = cell('div', '', 'evidence');
        const link = cell('a', '打开分享链接');
        link.href = `/#shared?id=${share.id}`;
        const url = Object.assign(document.createElement('input'), { value: link.href, readOnly: true });
        url.setAttribute('aria-label', '分享链接地址');
        url.addEventListener('focus', () => url.select());
        const revoke = cell('button', '撤销链接', 'ghost small-button');
        revoke.type = 'button';
        revoke.addEventListener('click', async () => {
          revoke.disabled = true;
          try { await api(`${path}/${share.id}`, 'DELETE'); await load(); }
          catch (error) { notice(error.message, true); revoke.disabled = false; }
        });
        row.append(link, cell('p', `到期 ${formatTime(share.expiresAt)}`, 'hint'), url, revoke);
        listing.append(row);
      }
    } catch (error) { listing.replaceChildren(cell('p', error.message, 'warning')); }
  };
  panel.addEventListener('toggle', () => { if (panel.open) void load(); });
  form.addEventListener('submit', async event => {
    event.preventDefault();
    create.disabled = true;
    try { await api(path, 'POST', { hours: hours.valueAsNumber }); await load(); }
    catch (error) { notice(error.message, true); }
    finally { create.disabled = false; }
  });
  return panel;
}

async function showEvent(id, updateHash = true, monitoringReturn = '') {
  const request = ++eventRequest;
  if (updateHash) setView(`incidents?event=${encodeURIComponent(id)}`);
  if (monitoringReturn) history.replaceState({ monitoringReturn }, '', location.href);
  const target = $('event-detail');
  target.replaceChildren(cell('p', '正在读取事件证据…', 'hint'));
  const { event, analysis, replay, relatedEvents = [], waitCaptures = [] } = await api(`/api/events/${id}`);
  const canRespond = ['admin', 'operator'].includes(currentRole);
  const canReviewOrders = canRespond || currentRole === 'approver';
  const [eventAudit, workOrders, aiStatus, impact, handoff] = await Promise.all([
    currentRole === 'admin' ? api(`/api/events/${id}/audit`).catch(() => null) : null,
    canReviewOrders ? api(`/api/events/${id}/work-orders`).catch(() => ({ orders: [] })) : { orders: [] },
    api('/api/ai/status').catch(() => ({ enabled: false })),
    replay ? api(`/api/events/${id}/impact`).catch(() => null) : null,
    canRespond ? api(`/api/events/${id}/handoff`).catch(() => null) : null
  ]);
  if (request !== eventRequest) return;
  if (selectedEvent !== id) eventSection = 'evidence';
  selectedEvent = id;
  for (const button of $('event-list').querySelectorAll('[data-event-id]')) {
    button.classList.toggle('selected', button.dataset.eventId === id);
    if (button.dataset.eventId === id) button.setAttribute('aria-current', 'true');
    else button.removeAttribute('aria-current');
  }
  const summary = cell('div', '', 'incident-summary');
  const status = cell('span', event.status === 'open' ? '处理中' : '已恢复', `incident-status is-${event.status}`);
  const header = cell('div', '', 'incident-card-meta');
  header.append(status, cell('span', event.assetName || allAssets.find(asset => asset.id === event.assetId)?.name || event.assetId));
  summary.append(header, cell('h3', event.title), cell('small', `开始 ${formatTime(event.startedAt)}${event.endedAt ? ' · 恢复 ' + formatTime(event.endedAt) : ''}`),
    cell('small', `事件 ID ${event.id}`));
  const back = history.state?.monitoringReturn;
  if (typeof back === 'string' && /^monitoring(?:\?|$)/.test(back)) {
    const link = cell('a', '返回集群同窗', 'monitor-link incident-monitor-return');
    link.href = `#${back}`;
    summary.append(link);
  }
  target.replaceChildren(summary);
  const sections = ['evidence', 'response', 'timeline'];
  const content = new Map(sections.map(key => [key, cell('section', '', 'incident-content')]));
  const evidencePanel = content.get('evidence');
  const responsePanel = content.get('response');
  const timelinePanel = content.get('timeline');
  const sectionBar = cell('div', '', 'section-nav incident-nav');
  sectionBar.setAttribute('aria-label', '事件工作区');
  const labels = ['根因与证据', '处置', '时间线与报告'];
  const sectionButtons = sections.map((key, index) => {
    const button = cell('button', labels[index]);
    button.type = 'button';
    button.addEventListener('click', () => activate(key));
    return button;
  });
  function activate(key) {
    eventSection = key;
    sections.forEach((item, index) => {
      sectionButtons[index].setAttribute('aria-pressed', String(item === key));
      content.get(item).hidden = item !== key;
    });
  }
  sectionBar.append(...sectionButtons);
  activate(eventSection);
  target.append(sectionBar, ...content.values());
  if (replay) evidencePanel.append(incidentReplayPanel(event, replay, analysis, aiStatus.enabled));
  if (waitCaptures.length) {
    const captures = cell('div', '', 'evidence');
    captures.append(cell('h4', '同一事件的短时等待采样'));
    for (const capture of waitCaptures) {
      const row = cell('div', '', 'actions');
      const jump = cell('button', `${formatTime(capture.startedAt)} · ${capture.observed}/${capture.expected} 帧`, 'ghost small-button');
      jump.type = 'button';
      jump.addEventListener('click', () => {
        monitoring.openIncident(event, 'waits', { at: capture.startedAt, waitCaptureId: capture.id });
        setView('monitoring');
      });
      row.append(jump);
      captures.append(row);
    }
    evidencePanel.append(captures);
  }
  if (assetServerPaging || replay) {
    const openWindow = async (topic, point) => {
      try {
        if (!assetServerPaging) await refresh();
        if (selectedEvent !== event.id) return;
        monitoring.openIncident(event, topic, point);
        setView('monitoring');
      } catch (error) {
        notice(error.message, true);
      }
    };
    const jump = cell('button', '查看事件时段的监控', 'ghost small-button');
    jump.type = 'button';
    jump.addEventListener('click', () => { void openWindow(); });
    const changes = cell('button', '查看同窗变更', 'ghost small-button');
    changes.type = 'button';
    changes.addEventListener('click', () => { void openWindow('changes'); });
    const actions = cell('div', '', 'actions monitor-event-jump');
    actions.append(jump, changes);
    if (analysis.candidates.some(item => item.code === 'patroni_leader_anomaly')) {
      const failover = cell('button', '查看切换前后', 'ghost small-button');
      failover.type = 'button';
      failover.addEventListener('click', () => { void openWindow('replication'); });
      actions.append(failover);
    }
    const logicalCandidate = analysis.candidates.find(item => item.code === 'logical_slot_wal_retention');
    if (logicalCandidate) {
      const slot = cell('button', '查看逻辑复制槽', 'ghost small-button');
      slot.type = 'button';
      const trigger = event.evidence.find(item => item.kind === 'alert_firing' &&
        item.payload?.metric === 'pg.logical_slot_retained_bytes' && logicalCandidate.supports.includes(`sample:${item.payload.sampleId}`));
      slot.addEventListener('click', () => { void openWindow('replication', trigger && {at: trigger.observedAt, sampleId: trigger.payload.sampleId}); });
      actions.append(slot);
    }
    if (analysis.candidates.some(item => item.code === 'wal_archive_failures')) {
      const backup = cell('button', '查看备份与 WAL 证据', 'ghost small-button');
      backup.type = 'button';
      const trigger = event.evidence.find(item => item.kind === 'alert_firing' && item.payload?.metric === 'pg.archive_fail_delta');
      backup.addEventListener('click', () => { void openWindow('backup', trigger && {at: trigger.observedAt, sampleId: trigger.payload.sampleId}); });
      actions.append(backup);
    }
    if (analysis.candidates.some(item => item.code === 'vacuum_backlog')) {
      const trigger = event.evidence.find(item => item.kind === 'alert_firing' && item.payload?.metric === 'pg.vacuum_backlog_percent' &&
        item.payload.detail?.databaseOid && item.payload.detail?.tableOid);
      if (trigger) {
        const object = cell('button', '查看对象维护证据', 'ghost small-button');
        object.type = 'button';
        object.addEventListener('click', () => { void openWindow('catalog', {at: trigger.observedAt, sampleId: trigger.payload.sampleId,
          databaseOid: trigger.payload.detail.databaseOid, tableOid: trigger.payload.detail.tableOid}); });
        actions.append(object);
      }
    }
    summary.append(actions);
  }
  if (relatedEvents.length) {
    const related = document.createElement('details'); related.className = 'evidence';
    related.append(cell('summary', `查看共享原始样本的其他事件（${relatedEvents.length}）`));
    for (const item of relatedEvents) {
      const row = document.createElement('p');
      const button = cell('button', `${item.eventGroup === 'telemetry' ? '采集缺口' : '可用性'} · ${item.title} · ${item.status === 'open' ? '处理中' : '已恢复'}`, 'ghost small-button');
      button.addEventListener('click', () => showEvent(item.id));
      row.append(button, cell('small', ` 事件 ID ${item.id} · 共享样本 ${item.sharedSampleId}`));
      related.append(row);
    }
    related.append(cell('small', '共享样本表示两个事件记录了同一次采样，不证明它们具有相同根因。'));
    evidencePanel.append(related);
  }
  if (impact && (impact.members?.length > 1 || impact.history?.length || impact.members?.[0]?.dependency || currentRole === 'admin'))
    evidencePanel.append(incidentImpactPanel(impact, event));
  const diagnosis = document.createElement('div'); diagnosis.className = 'evidence';
  diagnosis.append(cell('h4', '同一事件 ID 的证据链与根因候选'));
  if (replay && currentRole === 'admin') {
    const ruleReplay = cell('a', '回放根因规则', 'monitor-link');
    const params = new URLSearchParams({ section: 'rca', event: event.id });
    if (analysis.candidates[0]?.code) params.set('rule', analysis.candidates[0].code);
    ruleReplay.href = `#alerts?${params}`; diagnosis.append(ruleReplay);
  }
  diagnosis.append(cell('p', `证据图：${analysis.graph.nodes.length} 个节点、${analysis.graph.edges.length} 条关系；${analysis.timeline.length} 条时间线记录。`));
  if (analysis.traceCoverage) {
    const coverage = analysis.traceCoverage;
    diagnosis.append(cell('small', coverage.spans
      ? `应用链路：${coverage.services.join('、')} · ${coverage.databaseSpans} 条 PostgreSQL 客户端 Span。`
      : '应用链路：事件时间窗内无 Trace；数据库、Linux 与 K8S 侧诊断仍可使用。'));
    if (coverage.spans) {
      const declared = coverage.sampling;
      const rate = value => `${Number((value * 100).toPrecision(4))}%`;
      const hasRate = declared?.declaredSpans > 0 &&
        Number.isFinite(declared.minProbability) && Number.isFinite(declared.maxProbability);
      diagnosis.append(cell('small', hasRate
        ? `应用声明采样概率：${rate(declared.minProbability)}${declared.minProbability === declared.maxProbability ? '' : `–${rate(declared.maxProbability)}`}（${declared.declaredSpans}/${coverage.spans} 条 Span）；实际请求与 Trace 覆盖率未知。`
        : '应用未声明采样概率；实际请求与 Trace 覆盖率未知。'));
      const timing = coverage.clock;
      diagnosis.append(cell('small', timing?.futureTimestampSpans > 0
        ? `${timing.futureTimestampSpans} 条 Span 结束时间晚于 DbSphere 接收超过 1 秒，最大表观超前 ${timing.maxApparentLeadMs} ms；这些 Span 不用于跨层时间关联，实际时钟偏移仍未知。`
        : `接收时间可核对 ${timing?.timedSpans ?? 0}/${coverage.spans} 条 Span；未发现结束时间晚于接收超过 1 秒，实际时钟偏移仍未知。`));
    }
  }
  if (analysis.traceCoverage?.unresolvedParentSpans) diagnosis.append(cell('small',
    `链路断点：${analysis.traceCoverage.unresolvedParentSpans} 条 Span 的父 Span 未在当前事件窗口与显示上限内找到；可能由采样、延迟或时间窗造成。`));
  if (analysis.traceCoverage?.requestPaths?.length) {
    diagnosis.append(traceRequestEvidence(analysis.traceCoverage.requestPaths, event.assetId, analysis.traceCoverage.requestPathsAtLimit));
  }
  if (analysis.traceCoverage?.serviceImpact?.length) {
    const impact = document.createElement('details'); impact.append(cell('summary', '查看应用请求关联证据'));
    for (const service of analysis.traceCoverage.serviceImpact) {
      const row = document.createElement('div'); row.className = 'evidence work-order-card';
      const signals = [
        ['数据库错误', service.errorTraces],
        ['锁等待 PID 匹配', service.lockWaitTraces],
        ['SQL 退化同指纹慢调用', service.sqlRegressionTraces]
      ].filter(([, count]) => count > 0).map(([label, count]) => `${label} ${count}`);
      row.append(cell('p', `${service.service} · 同期 Trace ${service.observedTraces ?? 0} 条 · 故障信号关联 ${service.correlatedTraces ?? 0} 条`));
      row.append(cell('small', `${signals.join(' / ') || '无明确故障信号'} · Span ${service.spans} · 数据库调用 ${service.databaseSpans} · 错误 Span ${service.errorSpans} · ${formatTime(service.firstAt)} 至 ${formatTime(service.lastAt)}`));
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
    impact.append(cell('small', '故障信号关联指数据库客户端错误、与锁样本精确匹配的后端 PID，或 SQL 退化事件中 Query ID 与唯一指纹匹配的慢调用；沿父 Span 关联请求。这些线索不能单独证明根因。'));
    if (analysis.traceCoverage.atLimit) impact.append(cell('small', '已达到 100 条 Span 显示上限，计数可能不完整。'));
    diagnosis.append(impact);
  }
  const graphTypes = new Set(analysis.graph.nodes.map(node => node.type));
  const hasTrace = (analysis.traceCoverage?.spans ?? 0) > 0;
  const chain = document.createElement('div'); chain.className = 'flow-strip';
  const graphSteps = [
    ['应用服务', graphTypes.has('service_span'), '有上游服务 Span', hasTrace ? '缺少上游服务 Span' : '未接入 Trace'],
    ['数据库调用', graphTypes.has('db_client_span'), '有 PostgreSQL 客户端 Span', hasTrace ? '缺少数据库客户端 Span' : '未接入 Trace'],
    ['PostgreSQL', graphTypes.has('sample'), '有采样证据', '样本缺失'],
    ['会话与 SQL', ['wait', 'session', 'sql_fingerprint', 'slow_log', 'plan'].some(type => graphTypes.has(type)),
      '有等待、锁或 SQL 证据', '尚无会话或 SQL 证据'],
    ['Linux / K8S', ['linux', 'pod', 'app_pod', 'pvc', 'k8s_node'].some(type => graphTypes.has(type)),
      '有主机或容器证据', '未采集或不适用']
  ];
  if ((analysis.traceCoverage?.appPodReported ?? 0) > 0) graphSteps.splice(1, 0,
    ['应用 Pod', (analysis.traceCoverage?.appPodVerified ?? 0) > 0,
      `${analysis.traceCoverage.appPodVerified}/${analysis.traceCoverage.appPodReported} 条数据库 Span 已经 Kubernetes API 核对`,
      'OTel 已上报应用 Pod，Kubernetes API 尚未核对']);
  if (graphTypes.has('pgbouncer')) graphSteps.splice(2, 0,
    ['PgBouncer', true, '有连接池路由或采样证据', '未接入连接池']);
  for (const [title, present, available, missing] of graphSteps) {
    const step = document.createElement('div'); step.className = `flow-step ${present ? 'present' : 'missing'}`;
    step.append(cell('strong', title), cell('span', present ? available : missing));
    chain.append(step);
  }
  const coverageDetails = document.createElement('details'); coverageDetails.className = 'obs-coverage-details';
  coverageDetails.append(cell('summary', '查看分层信号覆盖与缺口'), chain);
  const graphDetails = document.createElement('details');
  graphDetails.append(cell('summary', '查看证据关系'));
  const graphNodes = new Map(analysis.graph.nodes.map(node => [node.id, node]));
  const relationLabels = {
    affects: '影响', contains: '包含', observed_in: '见于样本', collected_from: '采自',
    collected_with: '同期采集', emitted: '产生', parent_of: '父链路', calls: '调用',
    calls_service: '调用服务', via_pooler: '经由连接池', forwards_to: '转发至',
    time_window: '处于事件时间窗', time_unreliable: '时间关联待核对', query_id_matches: 'Query ID 匹配',
    same_backend_pid: '后端 PID 匹配', observed_session: '观测到会话',
    blocked_by: '被阻塞于', observed_sql: '观测到 SQL', scheduled_on: '调度于',
    originated_from: '来自应用 Pod', references_pvc: '引用 PVC',
    observed_pvc_status: '核对 PVC 状态',
    routes_to: '路由至', compared_with: '与基线比较', hypothesizes: '提出候选',
    supported_by: '由证据支持', reviewed_by: '人工复核',
    baseline_before_event_and_log_within_event: '事前基线与事件内慢日志对应'
  };
  for (const edge of analysis.graph.edges) {
    const from = graphNodes.get(edge.from); const to = graphNodes.get(edge.to);
    const relation = cell('p', `${from?.type ?? '未知'} ${from?.label ?? edge.from} → ${relationLabels[edge.relation] ?? edge.relation} → ${to?.type ?? '未知'} ${to?.label ?? edge.to}`, 'evidence');
    if (to?.source) {
      const source = cell('a', ' · 原始数据');
      source.href = to.source;
      source.target = '_blank';
      source.rel = 'noopener noreferrer';
      relation.append(source);
    }
    graphDetails.append(relation);
  }
  if (analysis.ruleSuppressedCount > 0)
    diagnosis.append(cell('p', `事件适用的规则过滤了 ${analysis.ruleSuppressedCount} 条候选。`, 'hint'));
  if (analysis.status === 'unlocated')
    diagnosis.append(cell('p', analysis.ruleSuppressedCount ? '未定位 · 当前规则条件未满足' : '未定位 · 证据不足', 'warning'));
  for (const candidate of analysis.candidates) {
    const candidateCard = cell('section', '', 'rca-candidate');
    candidateCard.append(cell('h5', candidate.title),
      cell('p', `${replayStrength[candidate.evidenceStrength] || '时间相关'} · ${candidate.limitation}`, 'rca-candidate-interpretation'));
    const facts = cell('div', '', 'rca-facts');
    if (candidate.rule && candidate.coverage) facts.append(cell('p',
      `规则 v${candidate.rule.version} · 支持引用 ${candidate.coverage.supports} · 反证 ${candidate.coverage.against} · 待核对 ${candidate.coverage.missing}`, 'hint'));
    const supports = document.createElement('details');
    supports.append(cell('summary', `查看支持证据（${candidate.supports.length} 条）`));
    for (const support of candidate.supports) {
      const node = graphNodes.get(support) ?? graphNodes.get(`evidence:${support}`);
      const row = cell('p', `${node?.type ?? '证据'} · ${node?.label ?? support}`, 'evidence');
      row.title = support;
      if (node?.source) {
        const link = cell('a', '查看原始数据'); link.href = node.source;
        link.target = '_blank'; link.rel = 'noopener noreferrer';
        row.append(document.createTextNode(' · '), link);
      }
      supports.append(row);
    }
    facts.append(supports);
    if (candidate.matchedConditions?.length) facts.append(cell('p', `匹配条件：${candidate.matchedConditions.map(item =>
      typeof item === 'string' ? item : `${item.metric ?? '条件'}：${item.observed ?? '未知'} ${item.operator ?? ''} ${item.threshold ?? ''}`).join('；')}`));
    if (candidate.against?.length) facts.append(cell('p', `反证：${candidate.against.join('；')}`));
    if (candidate.timeOrder) {
      const order = {
        before_event: '先于事件', same_window: '事件同窗', after_event: '事件开始后',
        baseline_before_event_and_log_within_event: '事前基线、事件与同期慢日志'
      }[candidate.timeOrder.relation] ?? '时间关系待核对';
      facts.append(cell('p', `时间顺序：${order} · 首次观测 ${candidate.timeOrder.firstObservedAt
        ? formatTime(candidate.timeOrder.firstObservedAt) : '未知'} · 事件开始 ${formatTime(candidate.timeOrder.eventStartedAt)}`));
    }
    if (candidate.missing.length) facts.append(cell('p', `缺失：${candidate.missing.join('；')}`));
    candidateCard.append(facts);
    if (candidate.review) candidateCard.append(cell('p', `人工${candidate.review.decision === 'confirmed' ? '确认' : '驳回'} · ${formatTime(candidate.review.at)} · ${candidate.review.reason}`, 'evidence'));
    const reviewActions = cell('div', '', 'rca-candidate-actions');
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
      reviewActions.append(button);
    }
    candidateCard.append(reviewActions);
    diagnosis.append(candidateCard);
  }
  if (analysis.missingSignals.length) diagnosis.append(cell('small', `信号缺口：${analysis.missingSignals.join('；')}`));
  evidencePanel.append(diagnosis, window.DbSphereObservability.eventGraph(analysis.graph, event), coverageDetails, graphDetails);
  evidencePanel.append(evidenceReviewPanel(event, id => {
    activate('timeline');
    const record = timelinePanel.querySelector('#event-evidence-' + id);
    if (record) {
      const timeline = record.closest('details');
      if (timeline) timeline.open = true;
      record.tabIndex = -1;
      record.focus({preventScroll: true});
      record.scrollIntoView({block:'center'});
    }
  }));
  timelinePanel.append(window.DbSphereObservability.incidentTimeline(event));
  const reports = document.createElement('div'); reports.className = 'actions';
  for (const [format, label] of [['md', '下载事件报告 · Markdown'], ['html', '下载事件报告 · HTML']]) {
    const button = cell('button', label, 'ghost small-button');
    button.addEventListener('click', () => { window.location.href = `/api/events/${event.id}/report?format=${format}`; });
    reports.append(button);
  }
  timelinePanel.append(reports);
  if (canRespond) timelinePanel.append(eventSharingPanel(event.id));
  if (handoff) {
    const panel = cell('section', '', 'response-form');
    panel.append(cell('h4', '值班交接'));
    const assignment = handoff.assignment;
    panel.append(cell('p', assignment ? `${assignment.assignedBy} 于 ${formatTime(assignment.assignedAt)} 交接给 ${assignment.username}` : '当前事件暂无接班人。', 'hint'));
    if (event.status === 'open' && handoff.eligible?.length) {
      const form = cell('form', '', 'response-form operator-action');
      const assignee = accessSelect(form, '接班人', 'accountId', handoff.eligible.map(account => [account.id, account.username]), [assignment?.accountId]);
      const note = Object.assign(document.createElement('textarea'), { required: true, minLength: 5, maxLength: 500, rows: 2 });
      accessField(form, '交接记录', note);
      const submit = cell('button', '交接当前事件', 'ghost small-button');
      submit.type = 'submit';
      form.append(submit);
      form.addEventListener('submit', async formEvent => {
        formEvent.preventDefault();
        submit.disabled = true;
        try {
          await api(`/api/events/${event.id}/handoff`, 'POST', { accountId: assignee.value, note: note.value.trim() });
          await showEvent(event.id);
        } catch (error) { notice(error.message, true); submit.disabled = false; }
      });
      panel.append(form);
    }
    responsePanel.append(panel);
  }
  if (canReviewOrders) {
    const panel = document.createElement('section'); panel.className = 'response-form';
    panel.append(cell('h4', '受控作业 · 双人审批'));
    panel.append(cell('p', '先预检作业范围和期限，独立审批通过后由管理员执行。', 'hint'));
    if (canRespond && event.status === 'open' && approvalEnabled) {
      const form = document.createElement('form'); form.className = 'response-form operator-action';
      const label = cell('label', '申请原因（5–300 字）');
      const reason = document.createElement('textarea'); reason.required = true; reason.minLength = 5;
      reason.maxLength = 300; reason.rows = 2; reason.placeholder = '说明该作业如何帮助处理当前事件';
      label.append(reason);
      const actions = new Set(['collect_once', 'inspect_once', ...(workOrders.profiles || []).map(item => item.action)]);
      const action = accessSelect(form, '作业类型', 'action', [...actions].map(id => [id, workActionLabels[id]]), ['collect_once']);
      const template = accessInput(form, '巡检模板 ID', 'templateId', 'pg_daily');
      const profile = accessSelect(form, '执行配置', 'profile', [], []);
      const expiry = accessInput(form, '审批有效期（分钟）', 'expiresMinutes', '15', { type: 'number', min: 1, max: 60, required: true });
      const timeout = accessInput(form, '最长执行时间（秒）', 'timeoutSeconds', '35', { type: 'number', min: 1, max: 35, required: true });
      const preview = cell('button', '预检与预演', 'ghost small-button'); preview.type = 'button';
      const description = cell('p', '', 'hint'); description.setAttribute('aria-live', 'polite');
      const submit = cell('button', '提交作业审批', 'ghost small-button write-action operator-action'); submit.type = 'submit';
      const path = `/api/events/${event.id}/work-orders`;
      const draft = () => ({ action: action.value, reason: reason.value.trim(),
        templateId: template.disabled ? '' : template.value.trim(), profile: profile.disabled ? '' : profile.value,
        expiresMinutes: expiry.valueAsNumber, timeoutSeconds: timeout.valueAsNumber });
      let idempotencyKey = '';
      const invalidate = () => { idempotencyKey = ''; description.textContent = ''; submit.disabled = true; };
      const configureAction = () => {
        template.disabled = action.value !== 'inspect_once';
        template.parentElement.hidden = template.disabled;
        profile.replaceChildren(...(workOrders.profiles || []).filter(item => item.action === action.value)
          .map(item => new Option(item.name, item.name)));
        profile.disabled = !profile.options.length;
        profile.parentElement.hidden = profile.disabled;
        invalidate();
      };
      action.addEventListener('change', configureAction);
      form.addEventListener('input', invalidate);
      configureAction();
      preview.addEventListener('click', async () => {
        if (!expiry.reportValidity() || !timeout.reportValidity()) return;
        const input = draft();
        preview.disabled = true;
        try {
          const result = await api(`${path}/preview`, 'POST', input);
          if (JSON.stringify(input) !== JSON.stringify(draft())) return;
          description.textContent = `${result.plan.description} · 审批有效 ${result.expiresMinutes} 分钟 · 最长执行 ${result.timeoutSeconds} 秒`;
          idempotencyKey ||= Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, '0')).join('');
          submit.disabled = false;
        } catch (error) { notice(error.message, true); }
        finally { preview.disabled = false; }
      });
      form.append(label, preview, description, submit);
      form.addEventListener('submit', async e => {
        e.preventDefault(); submit.disabled = true;
        if (!idempotencyKey) return;
        try { await api(path, 'POST', { ...draft(), idempotencyKey }); await showEvent(event.id); }
        catch (error) { notice(error.message, true); submit.disabled = false; }
      });
      panel.append(form);
    }
    if (currentRole === 'admin' && !approvalEnabled) panel.append(cell('p', '启用独立审批账号后可申请受控只读采样。', 'hint'));
    if (!workOrders.orders?.length) panel.append(cell('p', '当前事件暂无工单。', 'empty'));
    for (const order of workOrders.orders || []) {
      const row = cell('div', '', 'evidence work-order-entry');
      const states = { requested: '待审批', approved: '已批准待执行', rejected: '已驳回',
        executing: '执行中', completed: '已完成', failed: '执行失败', expired: '已过期', interrupted: '执行中断，待复核' };
      const actionLabel = workActionLabels[order.action] || order.action;
      row.append(cell('p', `${states[order.status] || order.status} · ${actionLabel} · ${order.reason}`),
        cell('small', `工单 ${order.id} · 申请人 ${order.requestedBy} · 申请时间 ${formatTime(order.requestedAt)} · 到期 ${formatTime(order.expiresAt)}`));
      if (order.plan) row.append(cell('small', `${order.plan.description} · ${order.timeoutSeconds} 秒上限`));
      if (order.approvedBy) row.append(cell('small', `审批人 ${order.approvedBy}`));
      if (order.result?.sampleId) {
        const sample = cell('a', `查看原始样本 ${order.result.sampleId}`);
        sample.href = `/api/assets/${event.assetId}/samples/${order.result.sampleId}`;
        row.append(sample);
        if (order.result.sampleStatus) row.append(cell('small', `采样状态 ${order.result.sampleStatus}`));
      }
      if (order.result?.inspectionRunId) {
        const report = cell('a', '下载巡检报告');
        report.href = `/api/inspection-runs/${order.result.inspectionRunId}/report`;
        row.append(report);
      }
      if (order.result?.restoredDatabase) row.append(cell('small',
        `恢复库 ${order.result.restoredDatabase} · 恢复点前标记${order.result.expectedPresent ? '存在' : '缺失'} · 后续标记${order.result.laterMarkerAbsent ? '未出现' : '存在'}`));
      if (order.result?.errorCode) row.append(cell('small', workErrorLabels[order.result.errorCode] || order.result.errorCode));
      if (currentRole === 'approver' && order.status === 'requested' && order.requestedBy !== currentUsername) {
        for (const [action, label] of [['approve', '批准'], ['reject', '驳回']]) {
          const button = cell('button', label, 'ghost small-button approval-action');
          button.addEventListener('click', async () => {
            button.disabled = true;
            try { await api(`/api/work-orders/${order.id}/${action}`, 'POST'); await showEvent(event.id); }
            catch (error) { notice(error.message, true); button.disabled = false; }
          });
          row.append(button);
        }
      }
      if (currentRole === 'admin' && order.status === 'approved') {
        const execute = cell('button', `执行已审批的${actionLabel}`, 'ghost small-button write-action');
        execute.addEventListener('click', async () => {
          execute.disabled = true;
          try { await api(`/api/work-orders/${order.id}/execute`, 'POST'); await showEvent(event.id); await refresh(); }
          catch (error) { notice(error.message, true); execute.disabled = false; }
        });
        row.append(execute);
      }
      panel.append(row);
    }
    responsePanel.append(panel);
  }
  if (currentRole === 'admin') {
    const auditDetails = document.createElement('details'); auditDetails.className = 'settings-details';
    auditDetails.append(cell('summary', '查看该事件的操作记录'));
    const auditList = document.createElement('div'); auditList.className = 'list';
    renderAdminAudit(eventAudit?.entries, auditList);
    auditDetails.append(auditList); timelinePanel.append(auditDetails);
  }
  if (aiStatus.enabled && currentRole === 'admin') {
    const aiResult = document.createElement('div'); aiResult.className = 'evidence';
    const showAIResult = result => {
      aiResult.replaceChildren(cell('h4', 'AI 建议 · 待人工核验'),
        cell('p', result.summary), cell('p', `证据 ID：${result.evidenceIds.join('、') || '未引用'}`),
        cell('p', `不确定性：${result.uncertainty || '模型未提供'}`));
      for (const id of result.evidenceIds) {
        const point = replay?.points.find(item => item.evidenceId === id);
        if (!point?.sampleId) continue;
        const item = cell('p', `${replayLayers[point.layer]} · ${formatTime(point.at)} · `);
        item.append(replaySampleLink(event.assetId, point.sampleId));
        aiResult.append(item);
      }
      for (const check of result.suggestedChecks) aiResult.append(cell('p', `待检查：${check}`));
      aiResult.append(cell('small', result.notice));
    };
    const analyze = cell('button', `AI 辅助分析 · ${aiStatus.model} · 每日最多 ${aiStatus.dailyLimit ?? 20} 次`, 'ghost small-button write-action');
    analyze.addEventListener('click', async () => {
      analyze.disabled = true;
      aiResult.replaceChildren(cell('p', '正在分析当前事件的脱敏证据…'));
      try {
        const { result } = await api(`/api/events/${event.id}/ai-summary`, 'POST');
        showAIResult(result);
      } catch (error) { aiResult.replaceChildren(cell('p', error.message, 'warning')); }
      finally { analyze.disabled = false; }
    });
    const questionForm = document.createElement('form'); questionForm.className = 'response-form';
    const questionLabel = cell('label', '向 AI 询问当前事件（仅发送脱敏证据，5–400 字）');
    const question = document.createElement('textarea'); question.required = true;
    question.minLength = 5; question.maxLength = 400; question.rows = 2;
    question.placeholder = '例如：哪些证据支持锁等待，哪些信号仍缺失？';
    questionLabel.append(question);
    const ask = cell('button', '基于证据提问', 'ghost small-button write-action'); ask.type = 'submit';
    questionForm.append(questionLabel, ask);
    questionForm.addEventListener('submit', async e => {
      e.preventDefault(); ask.disabled = true;
      aiResult.replaceChildren(cell('p', '正在根据当前事件的脱敏证据回答…'));
      try {
        const { result } = await api(`/api/events/${event.id}/ai-question`, 'POST', { question: question.value.trim() });
        showAIResult(result);
      } catch (error) { aiResult.replaceChildren(cell('p', error.message, 'warning')); }
      finally { ask.disabled = false; }
    });
    evidencePanel.append(analyze, questionForm, aiResult);
  }
  if (event.acknowledgedAt) responsePanel.prepend(cell('p', `事件已于 ${formatTime(event.acknowledgedAt)} 确认。`));
  else if (event.status === 'open' && canRespond) {
    const acknowledge = cell('button', '确认事件', 'ghost small-button write-action operator-action');
    acknowledge.addEventListener('click', async () => {
      try { await api(`/api/events/${event.id}/ack`, 'POST'); await showEvent(event.id); await refresh(); }
      catch (error) { notice(error.message, true); }
    });
    responsePanel.prepend(acknowledge);
  }
  const responseForm = document.createElement('form'); responseForm.className = 'response-form operator-action';
  responseForm.append(cell('h4', '处置与恢复验证'));
  const phaseLabel = cell('label', '记录类型');
  const phase = document.createElement('select');
  for (const [value, label] of [['diagnosis', '诊断记录'], ['mitigation', '处置记录'], ['handoff', '值班交接'], ['false_positive', '误报复核'],
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
  if (canRespond) responsePanel.append(responseForm);
  if (currentRole === 'viewer' && !event.acknowledgedAt)
    responsePanel.append(cell('p', '此账号可以查看已记录的处置和恢复验证。', 'hint'));
  const condensedTimeline = event.evidence.length > 12;
  const highlights = document.createElement('div'); highlights.className = 'event-highlights';
  if (condensedTimeline) highlights.append(cell('h4', '关键时间节点'));
  const timeline = document.createElement('details'); timeline.className = 'settings-details event-timeline';
  timeline.open = !condensedTimeline;
  timeline.append(cell('summary', `完整证据时间线（${event.evidence.length} 条）`));
  const timelineEntries = document.createElement('div'); timeline.append(timelineEntries);
  if (condensedTimeline) timelinePanel.append(highlights);
  timelinePanel.append(timeline);
  let highlightCount = 0;
  let highlightedNoLeader = false;
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
    if (evidence.kind === 'patroni_state') text = evidence.payload.status === 'ok'
      ? `leader ${evidence.payload.leaderCount} 个；其他成员 ${evidence.payload.replicaCount} 个`
      : evidence.payload.reason || evidence.payload.status;
    if (evidence.kind === 'routing_observed') text = `${routingStatus[evidence.payload.detail?.status] || '入口状态更新'} · ${evidence.payload.sampleId}`;
    if (evidence.kind.startsWith('alert_')) {
      const alert = evidence.payload;
      const prefix = `${alert.ruleId || '规则'} · ${alert.source || '未知来源'}`;
      if (evidence.kind === 'alert_firing') {
        text = `${prefix} · ${formatAlertValue(alert.metric, alert.value)} ${alert.operator || ''} ${formatAlertValue(alert.metric, alert.threshold)}`;
      } else if (evidence.kind === 'alert_recovered') {
        text = `${prefix} · 已恢复；先前告警值 ${formatAlertValue(alert.metric, alert.value)}`;
      } else {
        const labels = { alert_signal_missing: '指标暂不可用', alert_disabled: '规则已停用',
          alert_suppressed: '通知已静默', alert_unsuppressed: '通知静默结束',
          alert_recovery_pending: '开始观察恢复', alert_recovery_cancelled: '异常再次出现' };
        text = `${prefix} · ${labels[evidence.kind] || evidence.kind}`;
      }
      if (alert.sampleId) text += ` · 样本 ${alert.sampleId}`;
      if (alert.detail?.status && alert.metric === 'routing.entry_fault')
        text += ` · ${routingStatus[alert.detail.status] || alert.detail.status}`;
      if (alert.reason) text += ` · ${alert.reason}`;
      if (alert.silenceId) text += ` · 静默窗 ${alert.silenceId}`;
    }
    if (evidence.kind === 'event_acknowledged') text = `${evidence.payload.actor || '管理员'} 已确认事件`;
    if (evidence.kind === 'notification_delivery' || evidence.kind === 'notification_retry') {
      const delivery = evidence.payload;
      const status = {pending: '等待重试', delivered: '已送达', skipped: '已跳过', failed: '失败'}[delivery.status] || delivery.status;
      text = `${delivery.channel} · ${status} · 投递 ID ${delivery.deliveryId}`;
    }
    const external = ['external_alert_firing', 'external_alert_resolved', 'external_ticket_linked'].includes(evidence.kind);
    const risk = evidence.kind === 'risk_forecast' || evidence.kind === 'risk_forecast_cleared';
    if (risk) text = evidence.payload.title || riskMetricLabels[evidence.payload.metric] || '风险信号';
    if (external) {
      const input = evidence.payload;
      text = [input.source, input.externalId, input.title].filter(Boolean).join(' · ');
    }
    if (evidence.kind === 'rca_decision') text = `${evidence.payload.actor || '管理员'} ${evidence.payload.decision === 'confirmed' ? '确认' : '驳回'} ${evidence.payload.code}：${evidence.payload.reason}`;
    if (evidence.kind === 'ai_invocation') text = `${evidence.payload.actor === 'auto' ? '自动' : '手动'}${evidence.payload.purpose === 'question' ? '提问' : '摘要'} · ${evidence.payload.model || '模型'} ${evidence.payload.status === 'success' ? '已给出待核验建议' : evidence.payload.status === 'pending' ? '分析中' : '分析失败'} · 引用证据 ID：${evidence.payload.evidenceIds?.join('、') || '无'}`;
    if (evidence.kind === 'ai_advisory') text = `AI 待核验建议：${evidence.payload.summary || ''} · 不确定性：${evidence.payload.uncertainty || ''} · 待检查：${evidence.payload.suggestedChecks?.join('；') || '无'} · 证据 ID：${evidence.payload.evidenceIds?.join('、') || '无'}`;
    if (evidence.kind === 'ai_evidence_review') text = (evidence.payload.result?.model || '模型') + ' · ' +
      (evidence.payload.result?.claims?.length || 0) + ' 条带引用的审核结论';
    if (evidence.kind === 'event_handoff') text = `${evidence.payload.actor} 交接给 ${evidence.payload.assignedTo} · ${evidence.payload.note}`;
    if (evidence.kind === 'work_order_requested') text = `${evidence.payload.actor || '管理员'} 申请${workActionLabels[evidence.payload.action] || '受控作业'} ${evidence.payload.workOrderId} · ${evidence.payload.reason}`;
    if (evidence.kind === 'work_order_decision') text = `${evidence.payload.actor || '独立审批账号'} ${evidence.payload.decision === 'approved' ? '批准' : '驳回'}工单 ${evidence.payload.workOrderId}`;
    if (evidence.kind === 'work_order_executed') text = `${evidence.payload.actor || '管理员'} 执行工单 ${evidence.payload.workOrderId} · ${evidence.payload.status === 'completed' ? '已完成' : workErrorLabels[evidence.payload.errorCode] || '执行失败'}${evidence.payload.sampleId ? ' · 样本 ' + evidence.payload.sampleId : ''}`;
    if (evidence.kind === 'operator_response' || evidence.kind === 'recovery_verified') {
      const responseLabels = {diagnosis: '诊断', mitigation: '处置', handoff: '值班交接', false_positive: '误报复核', recovery_verification: '恢复验证'};
      text = `${evidence.payload.actor || '管理员'} · ${responseLabels[evidence.payload.phase] || '处置'} · ${evidence.payload.note}${evidence.payload.sampleId ? ` · 成功采样 ${evidence.payload.sampleId}` : ''}`;
      responsePanel.append(cell('p', `${formatTime(evidence.observedAt)} · ${text}`, 'evidence'));
    }
    const label = external || risk || evidence.kind === 'ai_evidence_review' ? replayKinds[evidence.kind] : evidence.kind;
    const row = cell('p', `${formatTime(evidence.observedAt)} · ${label} · ${text}`, 'evidence');
    row.id = 'event-evidence-' + evidence.id;
    if (risk && evidence.payload.sampleId) row.append(document.createTextNode(' · '), replaySampleLink(event.assetId, evidence.payload.sampleId));
    if (external && evidence.payload.url?.startsWith('https://')) {
      const link = cell('a', '打开外部记录', 'monitor-link');
      link.href = evidence.payload.url;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      row.append(document.createTextNode(' · '), link);
    }
    timelineEntries.append(row);
    const noLeader = evidence.kind === 'patroni_state' && evidence.payload?.leaderCount === 0;
    if (condensedTimeline && highlightCount < 8 && (
      evidence.kind === 'alert_firing' || evidence.kind === 'alert_recovered' || evidence.kind === 'collection_recovered' ||
      evidence.kind === 'recovery_verified' || evidence.kind === 'root_cause_candidate' ||
      external || risk || (noLeader && !highlightedNoLeader))) {
      const highlight = row.cloneNode(true);
      highlight.removeAttribute('id');
      highlights.append(highlight);
      highlightCount++;
      if (noLeader) highlightedNoLeader = true;
    }
  }
  if (condensedTimeline && !highlightCount) highlights.append(cell('p', '暂无可提炼的关键节点，请展开完整证据时间线。', 'empty'));
}

async function boot() {
  const session = await api('/api/session');
  csrf = session.csrf;
  currentRole = session.authenticated ? (session.role || 'admin') : null;
  currentUsername = session.username || '';
  currentOrganization = session.organizationId || '';
  oidcEnabled = session.oidcEnabled === true;
  approvalEnabled = session.approvalEnabled === true;
  viewerScoped = session.authenticated && session.viewerScoped === true;
  document.body.dataset.role = currentRole || '';
  document.body.dataset.organizationScope = String(!!currentOrganization);
  $('role-label').textContent = [currentUsername, session.organizationName, accessRoleLabels[currentRole]].filter(Boolean).join(' · ');
  $('oidc-login').hidden = !oidcEnabled || session.authenticated;
  $('scoped-asset-export').hidden = !currentUsername || !viewerScoped;
  $('role-label').hidden = !session.authenticated;
  $('login-panel').hidden = session.authenticated;
  $('workspace').hidden = !session.authenticated;
  $('logout').hidden = !session.authenticated;
  const ssoError = new URLSearchParams(location.search).get('sso');
  if (ssoError && !session.authenticated) {
    $('login-error').textContent = ssoError === 'unlinked' ? '该身份尚未绑定 DbSphere 账号。' :
      ssoError === 'unavailable' ? '企业身份源暂不可用，可使用本地账号登录。' : '单点登录未通过验证。';
  }
  if (session.authenticated) {
    if (currentRole === 'admin') await loadOrganizations(false);
    setView(location.hash.slice(1), false);
    await refresh();
  }
}

for (const button of document.querySelectorAll('[data-nav]')) button.addEventListener('click', () => setView(button.dataset.nav));
for (const button of document.querySelectorAll('[data-open-view]')) button.addEventListener('click', () => setView(button.dataset.openView));
for (const bar of document.querySelectorAll('.section-nav')) {
  for (const button of bar.querySelectorAll('[data-section]')) {
    button.addEventListener('click', () => showSection(bar.dataset.view, button.dataset.section));
  }
}
$('asset-add').addEventListener('click', () => {
  resetEdit();
  const field = $('asset-organization-field');
  const select = field.querySelector('select');
  field.hidden = !!currentOrganization || organizations.length < 2;
  select.disabled = field.hidden;
  select.replaceChildren(...organizationOptions().map(([id, name]) => new Option(name, id, id === defaultOrganizationID, id === defaultOrganizationID)));
  $('asset-dialog').showModal();
});
$('asset-close').addEventListener('click', resetEdit);
$('asset-dialog').addEventListener('cancel', event => {
  event.preventDefault();
  resetEdit();
});
$('asset-search').addEventListener('input', resetAssetPage);
$('asset-runtime').addEventListener('change', resetAssetPage);
$('asset-prev').addEventListener('click', () => {
  if (assetPageIndex > 0) { assetPageIndex--; loadAssetPage().catch(error => notice(error.message, true)); }
});
$('asset-next').addEventListener('click', () => {
  if (assetCursors[assetPageIndex + 1]) {
    assetPageIndex++; loadAssetPage().catch(error => notice(error.message, true));
  }
});
$('event-search').addEventListener('input', resetEventPage);
$('event-status').addEventListener('change', resetEventPage);
$('event-prev').addEventListener('click', () => {
  if (eventPageIndex > 0) { eventPageIndex--; loadEventPage().catch(error => notice(error.message, true)); }
});
$('event-next').addEventListener('click', () => {
  if (eventCursors[eventPageIndex + 1]) {
    eventPageIndex++; loadEventPage().catch(error => notice(error.message, true));
  }
});
$('event-search').addEventListener('keydown', event => {
  const id = event.currentTarget.value.trim().toLowerCase();
  if (event.key !== 'Enter' || !/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(id)) return;
  event.preventDefault();
  openEvent(id);
});
window.addEventListener('hashchange', () => setView(location.hash.slice(1), false));

$('login-form').addEventListener('submit', async (event) => {
  event.preventDefault();
  $('login-error').textContent = '';
  try { const data = await api('/api/login', 'POST', Object.fromEntries(new FormData(event.target))); event.target.reset(); csrf = data.csrf; await boot(); }
  catch (e) { $('login-error').textContent = e.message; }
});

$('logout').addEventListener('click', async () => {
  await api('/api/logout', 'POST');
  $('login-form').reset();
  location.reload();
});
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
  const linux = event.target.value === 'linux';
  const mysql = event.target.value === 'mysql';
  const wasMySQL = event.target.dataset.previousRuntime === 'mysql';
  event.target.dataset.previousRuntime = event.target.value;
  const form = $('asset-form');
  if (!editingAsset && mysql && !wasMySQL && form.elements.port.value === '5432') form.elements.port.value = '3306';
  if (!editingAsset && !mysql && wasMySQL && form.elements.port.value === '3306') form.elements.port.value = '5432';
  if (!editingAsset && mysql && !wasMySQL && form.elements.database.value === 'postgres') form.elements.database.value = '';
  if (!editingAsset && !mysql && wasMySQL && form.elements.database.value === '') form.elements.database.value = 'postgres';
  $('asset-integrations').hidden = mysql;
  for (const input of $('asset-integrations').querySelectorAll('input')) input.disabled = mysql;
  $('asset-runtime-hint').textContent = mysql ? '数据库名称用于筛选 SQL 指纹。使用专用账号读取 Performance Schema。'
    : 'Linux 主机指标需要 node_exporter。Kubernetes 采集使用只读 API 凭据。数据库建议使用专用只读监控账号。';
  $('k8s-fields').hidden = !k8s;
  for (const field of document.querySelectorAll('.linux-field')) {
    field.hidden = !linux;
    field.querySelector('input').disabled = !linux;
  }
  for (const field of $('k8s-fields').querySelectorAll('input')) field.disabled = !k8s;
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
  if (editingAsset) delete data.organizationId;
  data.port = Number(data.port);
  data.tags = data.tags.split(/[,，]/).map(tag => tag.trim()).filter(Boolean);
  data.poolerClear = data.poolerClear === 'on';
  data.patroniClear = data.patroniClear === 'on';
  $('asset-error').textContent = '';
  $('asset-submit').disabled = true;
  try {
    await api(editingAsset ? `/api/assets/${editingAsset}` : '/api/assets', editingAsset ? 'PATCH' : 'POST', data);
    resetEdit(); notice('资产已保存。点击“采集”验证连接。'); await refresh();
  }
  catch (e) { $('asset-error').textContent = e.message; }
  finally { $('asset-submit').disabled = false; }
});

$('fleet-filters').addEventListener('submit', event => { event.preventDefault(); fleetPage = 1; void loadFleetOverview(); });
$('fleet-prev').addEventListener('click', () => { if (fleetPage > 1) { fleetPage--; void loadFleetOverview(); } });
$('fleet-next').addEventListener('click', () => { fleetPage++; void loadFleetOverview(); });
$('platform-refresh').addEventListener('click', loadPlatformStatus);
$('organization-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = event.currentTarget;
  try {
    await api('/api/admin/organizations', 'POST', { name: form.elements.name.value.trim() });
    form.reset();
    await loadOrganizations();
  } catch (error) { notice(error.message, true); }
});

boot().catch(e => { $('login-panel').hidden = false; $('login-error').textContent = e.message; });
window.setTimeout(pollStorageAvailability, 5000);
setInterval(() => { if (csrf && !longTermStoreUnavailable) refresh().catch(() => {}); }, 30000);
