'use strict';

window.DbSphereManagedSites = (() => {
  const endpoint = '/api/managed-sites';
  const states = {pending:'待上报',connected:'已连接',disconnected:'已断联',over_capacity:'超出容量'};
  const upgrades = {scheduled:'已排期',upgrading:'升级中',verified:'升级已核验',failed:'升级失败',rolling_back:'回退中',rolled_back:'回退已核验'};
  const nextStates = {scheduled:['upgrading'],upgrading:['verified','failed','rolling_back'],
    failed:['rolling_back'],rolling_back:['rolled_back'],verified:['rolling_back','scheduled'],rolled_back:['scheduled']};

  function render(root, {api, formatTime, admin, platform, organizations, defaultOrganization}) {
    function node(tag, text = '', cls = '') {
      const element = document.createElement(tag);
      element.textContent = text;
      if (cls) element.className = cls;
      return element;
    }
    function field(form, label, name, value = '', options = {}) {
      const wrapper = node('label', label);
      const input = Object.assign(node('input'), {name, value, required:true}, options);
      wrapper.append(input);
      form.append(wrapper);
      return input;
    }
    function select(form, label, name, choices) {
      const wrapper = node('label', label);
      const control = Object.assign(node('select'), {name});
      for (const [value, text] of choices) control.add(new Option(text, value));
      wrapper.append(control);
      form.append(wrapper);
      return control;
    }
    function action(text, click) {
      const button = Object.assign(node('button', text, 'ghost'), {type:'button'});
      button.addEventListener('click', click);
      return button;
    }
    const message = node('p', '', 'hint');
    message.setAttribute('role', 'status');
    const list = node('div', '', 'list');
    let request = 0;
    let busy = false;
    const expanded = new Set();
    const selectedOrganization = platform ? select(node('div'), '客户组织', 'organization',
      [['', '全部可见组织'], ...(organizations.length ? organizations : [[defaultOrganization, '默认组织']])]) : null;
    const controls = node('div', '', 'actions');
    if (selectedOrganization) controls.append(selectedOrganization.parentElement);
    controls.append(action('刷新站点', () => void load()));
    root.replaceChildren(controls, message, list);
    if (selectedOrganization) selectedOrganization.addEventListener('change', () => void load());

    function submit(form, title, save) {
      const button = node('button', title);
      form.append(button);
      form.addEventListener('submit', async event => {
        event.preventDefault();
        if (busy) return;
        busy = true;
        button.disabled = true;
        try {
          await save();
          await load('已保存。');
        } catch (error) { message.textContent = error.message; }
        finally { busy = false; button.disabled = false; }
      });
    }
    function registry(site) {
      const panel = node('details', '', 'settings-details');
      panel.append(node('summary', site ? '编辑站点登记' : '登记站点'));
      const form = node('form', '', 'response-form');
      const code = field(form, '站点标识', 'site', site?.site, {pattern:'[a-z0-9][a-z0-9\\-]{0,62}', readOnly:!!site});
      const org = select(form, '归属组织', 'organization', organizations.length ? organizations : [[defaultOrganization,'默认组织']]);
      org.value = site?.organizationId || selectedOrganization?.value || defaultOrganization;
      org.disabled = !!site;
      const label = field(form, '站点名称', 'label', site?.label, {maxLength:100});
      const consoleURL = field(form, '站点控制台 HTTPS 地址', 'consoleUrl', site?.consoleUrl, {type:'url',maxLength:512});
      const cap = field(form, '资产容量上限', 'assetCap', site?.assetCap ?? 100, {type:'number',min:1,max:10000});
      const delegate = field(form, '委派值班员账号 ID', 'delegateAccountId', site?.delegateAccountId ?? '', {maxLength:36, required:false});
      submit(form, '保存站点', () => api(endpoint, 'PUT', {site:code.value.trim(), organization:org.value,
        label:label.value.trim(), consoleUrl:consoleURL.value.trim(), assetCap:cap.valueAsNumber, delegateAccountId:delegate.value.trim()}));
      panel.append(form);
      return panel;
    }
    function siteLink(site, event) {
      const anchor = node('a', event ? `原站点事件 ${event.id}` : '打开原站点', 'monitor-link');
      const url = new URL(site.consoleUrl);
      if (url.protocol === 'https:') {
        if (event) url.hash = 'incidents?' + new URLSearchParams({event:event.id});
        anchor.href = url.href;
        anchor.target = '_blank';
        anchor.rel = 'noopener noreferrer';
      }
      return anchor;
    }
    function upgradePanel(site) {
      const panel = node('details', '', 'settings-details');
      const path = `${endpoint}/${encodeURIComponent(site.site)}/upgrades`;
      panel.append(node('summary', '升级与回退记录'));
      const latest = site.upgrade;
      const options = (nextStates[latest?.state] ?? ['scheduled']).filter(state => admin || state !== 'scheduled');
      if (options.length) {
        const form = node('form', '', 'response-form');
        const state = select(form, '登记状态', 'state', options.map(key => [key,upgrades[key]]));
        const batch = field(form, '批次标识', 'batch', '', {maxLength:63});
        const wave = field(form, '批次顺序', 'wave', 1, {type:'number', min:1, max:100});
        const version = field(form, '目标版本', 'targetVersion', '', {placeholder:'4.8.0', maxLength:11});
        const note = field(form, '核对说明', 'note', '', {maxLength:500});
        function sync() {
          for (const input of [batch,wave,version]) {
            input.parentElement.hidden = state.value !== 'scheduled';
            input.disabled = state.value !== 'scheduled';
          }
        }
        state.addEventListener('change', sync);
        sync();
        submit(form, '保存升级记录', () => api(path, 'POST', {revision:latest?.revision || 0, state:state.value,
          batch:batch.value.trim(), wave:wave.valueAsNumber, targetVersion:version.value.trim(), note:note.value.trim()}));
        panel.append(form);
      }
      const history = node('div', '', 'list');
      panel.append(history);
      let loaded = false;
      panel.addEventListener('toggle', async () => {
        if (!panel.open || loaded) return;
        loaded = true;
        try {
          const result = await api(path);
          history.replaceChildren(...result.history.map(record => node('p',
            `${record.batch} / ${record.wave} · ${upgrades[record.state]} · ${record.fromVersion} → ${record.targetVersion} · ${record.actor} · ${formatTime(record.changedAt)} · ${record.note}`, 'evidence')));
          if (!result.history.length) history.append(node('p', '暂无升级记录。', 'hint'));
        } catch (error) { history.replaceChildren(node('p', error.message, 'warning')); loaded = false; }
      });
      return panel;
    }
    function card(site) {
      const article = node('article', '', 'evidence');
      article.dataset.managedSite = site.site;
      article.append(node('h3', `${site.organization} / ${site.label}`),
        node('p', `${site.site} · ${states[site.siteStatus]}${site.summary ? ` · v${site.summary.version}` : ''}`), siteLink(site));
      if (site.siteStatus === 'over_capacity' && site.rejectedAt) article.append(node('p',
        `最近超限上报 ${site.rejectedAssets} 个资产 · ${formatTime(site.rejectedAt)}`, 'warning'));
      if (site.summary) {
        const summary = site.summary;
        article.append(node('p', `资产 ${summary.assetCount} / ${site.assetCap} · 正常 ${summary.healthy} · 失败 ${summary.failed} · 缺采 ${summary.missing}`),
          node('p', `处理中 ${summary.openEvents} · 24 小时恢复 ${summary.resolvedEventsLast24h} · 30 天签收 ${summary.signedReportsLast30d}`),
          node('p', `站点观测 ${formatTime(summary.observedAt)} · 中心接收 ${formatTime(site.receivedAt)}`, 'hint'));
        if (summary.events.length) {
          const evidence = node('details', '', 'settings-details');
          evidence.open = expanded.has(site.site);
          evidence.addEventListener('toggle', () => { if (evidence.open) expanded.add(site.site); else expanded.delete(site.site); });
          evidence.append(node('summary', '原站点事件'));
          for (const event of summary.events) {
            const row = node('p', `${event.status === 'open' ? '处理中' : '已恢复'} · ${formatTime(event.startedAt)} · `);
            row.append(siteLink(site,event));
            evidence.append(row);
          }
          article.append(evidence);
        }
      } else article.append(node('p', `资产容量上限 ${site.assetCap}`, 'hint'));
      if (site.upgrade) article.append(node('p', `${site.upgrade.batch} / ${site.upgrade.wave} · ${upgrades[site.upgrade.state]}`));
      article.append(upgradePanel(site));
      if (platform) article.append(registry(site));
      return article;
    }
    async function load(text = '') {
      const active = ++request;
      message.textContent = '正在读取站点…';
      try {
        const result = await api(endpoint);
        if (request !== active) return;
        const org = selectedOrganization?.value;
        const sites = result.sites.filter(site => !org || site.organizationId === org);
        list.replaceChildren(...sites.map(card));
        if (!sites.length) list.append(node('p', '暂无可见站点。', 'hint'));
        if (platform) list.prepend(registry());
        message.textContent = text;
      } catch (error) { if (request === active) message.textContent = error.message; }
    }
    void load();
  }
  return {render};
})();
