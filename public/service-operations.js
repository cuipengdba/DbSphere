'use strict';

window.DbSphereServiceOperations = (() => {
  const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
  const reportPath = '/api/service-reports';
  const routesPath = '/api/admin/notification-routes';
  const states = {draft:'草稿', rejected:'已驳回', archived:'已归档'};
  const channelNames = {webhook:'Webhook', email:'邮件', operations_webhook:'组织运维 Webhook'};
  const groupNames = {availability:'可用性', telemetry:'采集', capacity:'容量', storage:'存储', latency:'延迟', replication:'复制', maintenance:'维护'};
  const onlyText = t => String(t ?? '');
  const node = (tag, text, cls) => {
    const el = document.createElement(tag);
    el.textContent = onlyText(text);
    if (cls) el.className = cls;
    return el;
  };
  const button = (text, callback) => {
    const el = node('button', text, 'ghost');
    el.type = 'button';
    el.addEventListener('click', callback);
    return el;
  };
  const status = () => {
    const el = node('p', '', 'hint');
    el.setAttribute('role', 'status');
    return el;
  };
  const link = (text, route) => {
    const el = node('a', text, 'monitor-link');
    el.href = '#' + route;
    return el;
  };
  const route = (view, params = {}) => view + '?' + new URLSearchParams(params);
  function field(parent, title, control) {
    const label = node('label', title);
    label.append(control);
    parent.append(label);
    return control;
  }
  const input = (type, value, options = {}) => Object.assign(node('input'), {type, value}, options);
  function select(parent, title, choices, selected) {
    const el = node('select');
    for (const [value, label] of choices) el.add(new Option(label, value));
    if (choices.some(([value]) => selected === value)) el.value = selected;
    return field(parent, title, el);
  }
  function orgSelect(parent, context, selected, optional) {
    const options = context.organization ? [[context.organization,
      context.options.find(([id]) => id === context.organization)?.[1] || '当前组织']] : [...context.options];
    if (!options.length && !optional) options.push([context.defaultOrganization, '默认组织']);
    if (optional && !context.organization) options.unshift(['', '全部可见资产']);
    return select(parent, '客户组织', options, selected);
  }
  const duration = seconds => seconds == null ? '缺采' : `${Number(seconds.toFixed(1))} 秒`;
  const percent = number => number == null ? '无可计算样本' : `${number}%`;
  const metrics = report => [
    ['观测可用率', percent(report.observedAvailabilityPercent)], ['采样覆盖率', percent(report.coveragePercent)],
    ['可用资产时长', duration(report.availableSeconds)], ['故障资产时长', duration(report.unavailableSeconds)],
    ['计划维护', duration(report.maintenanceSeconds)], ['缺采', duration(report.missingSeconds)]
  ];
  const terms = values => {
    const el = node('dl', '', 'sample-fields');
    for (const [term, value] of values) el.append(node('dt', term), node('dd', value));
    return el;
  };
  function download(title, text) {
    const url = URL.createObjectURL(new Blob([text], {type:'text/plain;charset=utf-8'}));
    const a = node('a');
    a.href = url;
    a.download = title;
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  function exportReport(report, saved) {
    const lines = ['DbSphere 服务报告', `${report.from} — ${report.to}`,
      `资产 ${report.assetCount} 个 · 事件 ${report.incidentCount} 个`,
      '可用率按有效观测计算。维护和缺采单列。发现用时为有连续样本时的上界。',
      ...metrics(report).map(([name, value]) => `${name} ${value}`)];
    if (saved) lines.push(`报告 ID ${saved.id} · ${saved.signedAt ? `签收 ${saved.signedBy} ${saved.signedAt} ${saved.signoffNote}` : '待签收'}`);
    for (const asset of report.assets) {
      lines.push('', `${asset.organization} / ${asset.name} / ${asset.id}`,
        `可用 ${duration(asset.availableSeconds)} · 故障 ${duration(asset.unavailableSeconds)} · 维护 ${duration(asset.maintenanceSeconds)} · 缺采 ${duration(asset.missingSeconds)}`);
      for (const point of asset.capacity) lines.push(`容量 ${point.at} ${point.bytes} 字节 · 样本 ${point.sampleId}`);
    }
    for (const event of report.incidents) lines.push('', `${event.title} · 事件 ${event.id} · 资产 ${event.assetId}`,
      `发现 ${event.detectedAt} · 上界 ${duration(event.detectionUpperBoundSeconds)} · 确认 ${event.acknowledgedAt || '缺采'} · 恢复 ${event.recoveredAt || '缺采'}`,
      `最后正常样本 ${event.lastGoodSampleId || '缺采'} · 触发样本 ${event.triggerSampleId || '缺采'} · 恢复样本 ${event.recoverySampleId || '缺采'}`);
    if (report.incidentCount > report.incidents.length) lines.push('报告附带最近 100 个事件的明细。');
    download(`dbsphere-service-report${saved ? '-' + saved.id : ''}.txt`, lines.join('\n') + '\n');
  }
  function reports(root, params, context) {
    const {api, role, formatTime} = context;
    const canWrite = ['admin', 'operator'].includes(role);
    const message = status();
    const form = node('form', '', 'response-form');
    const history = node('div', '', 'list');
    const output = node('div', '', 'list');
    const local = date => new Date(date - date.getTimezoneOffset() * 60000).toISOString().slice(0, 19);
    const now = Date.now() - 10000;
    const dateField = (key, title, fallback) => {
      const at = Date.parse(params.get(key));
      return field(form, title, input('datetime-local', local(new Date(Number.isFinite(at) ? at : fallback)), {required:true, step:1}));
    };
    const from = dateField('from', '起始时间', now - 86400000);
    const to = dateField('to', '截止时间', now);
    const organization = orgSelect(form, context, params.get('organization') ?? (canWrite ? context.defaultOrganization : ''), true);
    const service = field(form, '服务名（service: 标签）', input('text', params.get('service') || '', {maxLength:56}));
    const assetGroup = field(form, '资产组（集群名）', input('text', params.get('assetGroup') || '', {maxLength:150}));
    const tag = field(form, '资产标签', input('text', params.get('tag') || '', {maxLength:64}));
    const submit = node('button', '生成规则报告');
    submit.type = 'submit';
    const save = button('保存报告快照', () => { if (form.reportValidity()) void request(true); });
    form.append(submit);
    if (canWrite) form.append(save);
    const filters = () => ({from:new Date(from.value).toISOString(), to:new Date(to.value).toISOString(),
      organization:organization.value, service:service.value.trim(), assetGroup:assetGroup.value.trim(), tag:tag.value.trim()});
    let busy = false;
    function render(report, saved, origin) {
      const result = document.createDocumentFragment();
      result.append(node('h3', '可用性与采样覆盖'), node('p', `${formatTime(report.from)} 至 ${formatTime(report.to)} · ${report.assetCount} 个资产`),
        terms(metrics(report)), node('p', '统计使用资产时长。可用率按有效观测计算。维护和缺采单列。', 'hint'),
        button('导出文本报告', () => exportReport(report, saved)));
      if (saved) {
        result.append(node('p', `报告 ${saved.id} · ${saved.creator} · ${formatTime(saved.createdAt)}`));
        if (saved.signedAt) result.append(node('p', `已签收 · ${saved.signedBy} · ${formatTime(saved.signedAt)} · ${saved.signoffNote}`));
        else if (canWrite) {
          const signoff = node('form', '', 'response-form');
          const note = field(signoff, '签收说明', input('text', '', {required:true,maxLength:500}));
          signoff.append(node('button', '人工签收'));
          signoff.addEventListener('submit', event => {
            event.preventDefault();
            void perform(async () => {
              const updated = await api(`${reportPath}/${saved.id}/signoff`, 'POST', {note:note.value.trim()});
              render(updated.report, updated, origin);
              await list();
              message.textContent = '报告已签收。';
            });
          });
          result.append(signoff);
        }
      }
      const sampleLink = (label, asset, sample, back = origin) => uuid.test(sample || '') ?
        link(label, route('sample', {asset, sample, return:back})) : node('small', label + '缺采', 'hint');
      for (const asset of report.assets) {
        const detail = node('details', '', 'settings-details');
        detail.append(node('summary', `${asset.organization} / ${asset.name}${asset.assetGroup ? ' / ' + asset.assetGroup : ''}`),
          terms([['可用', duration(asset.availableSeconds)], ['故障', duration(asset.unavailableSeconds)],
            ['维护', duration(asset.maintenanceSeconds)], ['缺采', duration(asset.missingSeconds)]]));
        if (!asset.capacity.length) detail.append(node('p', '窗口内无容量样本。', 'hint'));
        for (const point of asset.capacity) {
          const row = node('p', `${formatTime(point.at)} · ${(point.bytes / 2**30).toFixed(2)} GiB · `);
          row.append(sampleLink('容量原始采样', asset.id, point.sampleId));
          detail.append(row);
        }
        result.append(detail);
      }
      result.append(node('h3', `事件与恢复证据（${report.incidentCount}）`));
      if (report.incidentCount > report.incidents.length) result.append(node('p', '此处展示最近 100 个事件。', 'hint'));
      for (const event of report.incidents) {
        const eventRoute = route('incidents', {event:event.id, return:origin});
        const item = node('article', '', 'evidence');
        const actions = node('div', '', 'actions');
        actions.append(link('查看原事件', eventRoute), sampleLink('最后正常采样', event.assetId, event.lastGoodSampleId, eventRoute),
          sampleLink('触发原始采样', event.assetId, event.triggerSampleId, eventRoute), sampleLink('恢复原始采样', event.assetId, event.recoverySampleId, eventRoute));
        item.append(node('strong', event.title), node('p', `发现 ${formatTime(event.detectedAt)} · 事件 ID ${event.id}`),
          node('p', `发现用时上界 ${duration(event.detectionUpperBoundSeconds)} · 确认用时 ${duration(event.acknowledgeSeconds)} · 恢复用时 ${duration(event.recoverySeconds)}`), actions);
        result.append(item);
      }
      output.replaceChildren(result);
    }
    async function perform(task) {
      if (busy) return;
      busy = true;
      message.textContent = '正在读取服务报告…';
      try { await task(); }
      catch (error) { message.textContent = error.message; }
      finally { busy = false; }
    }
    function request(persist, restore = false) {
      return perform(async () => {
        const query = filters();
        if (persist && !query.organization) throw new Error('保存前请选择客户组织。');
        const result = await (persist ? api(reportPath, 'POST', query) : api(reportPath + '?' + new URLSearchParams(query)));
        if (!root.contains(output)) return;
        const origin = route('service-reports', persist ? {id:result.id} : query);
        if (!restore && document.body.dataset.activeView === 'service-reports') window.history.replaceState(null, '', '#' + origin);
        render(persist ? result.report : result, persist ? result : null, origin);
        if (persist) await list();
        message.textContent = persist ? '已保存待签收快照。' : '已生成规则报告。';
      });
    }
    async function list() {
      const selected = organization.value;
      try {
        const data = await api(reportPath + '/saved?' + new URLSearchParams({organization:selected}));
        if (organization.value !== selected) return;
        history.replaceChildren(node('h3', '最近保存的报告'), ...(data.reports || []).map(record => {
          const item = node('p');
          item.append(link(`${formatTime(record.createdAt)} · ${record.query.service || '全部服务'} · ${record.signedAt ? '已签收' : '待签收'}`,
            route('service-reports', {id:record.id})));
          return item;
        }));
        if (!data.reports?.length) history.append(node('p', '尚无已保存报告。', 'hint'));
      } catch (error) { history.replaceChildren(node('p', error.message, 'warning')); }
    }
    form.addEventListener('submit', event => { event.preventDefault(); void request(false); });
    organization.addEventListener('change', () => void list());
    root.replaceChildren(form, message, output, history);
    void list();
    const id = params.get('id');
    if (uuid.test(id || '')) {
      void perform(async () => {
        const saved = await api(`${reportPath}/${id}`);
        render(saved.report, saved, route('service-reports', {id}));
        message.textContent = '';
      });
    } else if (params.has('from') && params.has('to')) void request(false, true);
  }

  function routes(root, _params, context) {
    const {api, formatTime} = context;
    const header = node('div', '', 'response-form');
    const organization = orgSelect(header, context, context.organization || context.defaultOrganization, false);
    const message = status();
    const content = node('div');
    let request = 0;
    let saving = false;
    async function load() {
      const current = ++request;
      content.replaceChildren();
      message.textContent = '正在读取组织通知路由…';
      try {
        const data = await api(routesPath + '?' + new URLSearchParams({organization:organization.value}));
        if (current !== request) return;
        const form = node('form', '', 'response-form');
        const group = select(form, '告警分组', data.groups.map(name => [name, groupNames[name] || name]));
        const timeout = field(form, '未确认升级（分钟）', input('number', '15', {required:true,min:1,max:1440}));
        const options = (legend, names) => {
          const set = node('fieldset');
          set.append(node('legend', legend));
          form.append(set);
          return names.map(channel => field(set, channelNames[channel], input('checkbox', channel)));
        };
        const initial = options('初始通知', data.channels);
        const escalation = options('关键告警升级', data.channels);
        const detail = node('p', '', 'hint');
        const submit = node('button', '保存通知路由');
        const reset = button('恢复默认路由', () => void save(true));
        const read = controls => controls.filter(control => control.checked).map(control => control.value);
        async function save(toDefault) {
          if (saving) return;
          saving = true;
          [organization, group, submit, reset].forEach(control => { control.disabled = true; });
          try {
            const selected = group.value;
            await api(routesPath, 'PUT', {organization:data.organization, group:selected, reset:toDefault,
              afterMinutes:timeout.valueAsNumber, initial:read(initial), escalation:read(escalation)});
            await load();
            if (current + 1 === request) {
              const latest = content.querySelector('select');
              if (latest) { latest.value = selected; latest.dispatchEvent(new Event('change')); }
              message.textContent = '已更新通知路由。';
            }
          } catch (error) { message.textContent = error.message; }
          finally { saving = false; [organization, group, submit, reset].forEach(control => { control.disabled = false; }); }
        }
        function update() {
          const saved = (data.routes || []).find(item => item.group === group.value);
          timeout.value = saved?.afterMinutes ?? data.defaultAfterMinutes;
          for (const [key, controls] of [['initial', initial], ['escalation', escalation]]) {
            for (const control of controls) control.checked = (saved?.[key] ?? data.channels).includes(control.value);
          }
          detail.textContent = saved ? `已配置 · ${saved.updatedBy} · ${formatTime(saved.updatedAt)}` : '沿用默认路由。';
        }
        group.addEventListener('change', update);
        form.addEventListener('submit', event => { event.preventDefault(); void save(false); });
        form.append(submit, reset);
        content.append(form, detail);
        update();
        message.textContent = '';
      } catch (error) { if (current === request) message.textContent = error.message; }
    }
    organization.addEventListener('change', () => void load());
    root.replaceChildren(node('h2', '通知路由'), node('p', '按组织与告警分组选择现有投递通道。收件地址及密钥由部署配置提供。', 'hint'), header, message, content);
    void load();
  }

  function postmortem(event, {api, role, formatTime, showSource}) {
    const canWrite = ['admin', 'operator'].includes(role);
    const path = `/api/events/${event.id}/postmortem`;
    const aiPath = `/api/events/${event.id}/ai-evidence-review`;
    const panel = node('details', '', 'settings-details');
    panel.dataset.postmortem = '';
    const message = status(), content = node('div');
    panel.append(node('summary', '事件复盘与人工复核'), message, content);
    let loaded = false, busy = false;
    async function perform(callback) {
      if (busy) return;
      busy = true;
      panel.setAttribute('aria-busy', 'true');
      try { await callback(); }
      catch (error) {
        message.textContent = error.message;
        if (!loaded) content.replaceChildren(button('重新读取复盘', () => void perform(load)));
      } finally {
        busy = false;
        panel.removeAttribute('aria-busy');
      }
    }
    async function load() {
      const {revisions} = await api(path);
      loaded = true;
      const latest = revisions[0];
      content.replaceChildren();
      if (canWrite && latest?.state !== 'archived') {
        const form = node('form', '', 'response-form');
        const body = field(form, '人工复盘内容', Object.assign(node('textarea'), {value:latest?.body || '', rows:8, maxLength:5000, required:true}));
        async function save(action, modelInvocationId = '') {
          await api(path, 'POST', {revision:latest?.revision || 0, action,
            body:modelInvocationId ? '' : body.value.trim(), modelInvocationId});
          await load();
          message.textContent = action === 'archive' ? '复盘已人工签收并归档。' : action === 'reject' ? '草稿已驳回。' : '复盘草稿已保存。';
        }
        const submit = node('button', '保存人工草稿');
        form.append(submit);
        form.addEventListener('submit', event => { event.preventDefault(); void perform(() => save('draft')); });
        if (latest?.state === 'draft') form.append(button('驳回复盘', () => void perform(() => save('reject'))),
          button('签收并归档', () => { if (form.reportValidity()) void perform(() => save('archive')); }));
        content.append(form);
        const preview = node('div', '', 'list');
        const getPreview = button('预览 AI 送审证据', () => void perform(async () => {
          const review = await api(aiPath);
          preview.replaceChildren();
          if (!review.canRequest) {
            message.textContent = '当前无法使用模型。可继续编辑和保存人工复盘。';
            return;
          }
          preview.append(node('p', `模型 ${review.endpointModel} · 同事件证据 ${review.sources.length} 项`, 'hint'),
            node('pre', JSON.stringify(review.sources, null, 2), 'ai-evidence-preview'),
            button('生成 AI 复盘草稿', () => void perform(async () => {
              const result = await api(aiPath, 'POST', {preview:review.preview,
                question:'请根据已送审的事件证据整理影响、处置和恢复记录，分别指出候选解释、反证和缺采。'});
              if (result.status === 'model' && uuid.test(result.invocationId || '')) await save('draft', result.invocationId);
              else message.textContent = '模型未能返回可核验的复盘内容。可继续使用人工复盘。';
            })));
          message.textContent = '核对送审范围后可生成草稿。';
        }));
        content.append(getPreview, preview);
        if (latest) content.append(button('重新读取修订记录', () => void perform(load)));
      }
      if (!revisions.length) content.append(node('p', '暂无复盘修订。', 'hint'));
      for (const revision of revisions) {
        const detail = node('details', '', 'settings-details');
        detail.open = revision === latest;
        detail.append(node('summary', `修订 ${revision.revision} · ${states[revision.state]} · ${revision.actor} · ${formatTime(revision.createdAt)}`),
          node('pre', revision.body, 'ai-evidence-preview'));
        if (uuid.test(revision.aiEvidenceId || '')) {
          const ids = new Set([revision.aiEvidenceId, ...(revision.body.match(/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}/g) || [])]);
          for (const id of ids) detail.append(button('核对证据 ' + id.slice(0,8), () => {
            if (!showSource(id)) message.textContent = '该证据已超出当前时间线的展示或保留范围。';
          }));
        }
        content.append(detail);
      }
      message.textContent = '';
    }
    panel.addEventListener('toggle', () => { if (panel.open && !loaded) void perform(load); });
    return panel;
  }

  return {reports, routes, postmortem};
})();
