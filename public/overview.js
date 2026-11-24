(() => {
  const minute = 60_000, hour = 60 * minute, pageSize = 36;
  const validWindow = (from, to) => Number.isFinite(from) && Number.isFinite(to) &&
    to <= Date.now() + minute && to - from >= minute && to - from <= 168 * hour;
  const iso = value => new Date(value).toISOString();
  const timeLabel = value => new Date(value).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' });
  const localInput = value => iso(value - new Date(value).getTimezoneOffset() * minute).slice(0, 16);
  const phases = { down: '不可用', alert: '有告警', stale: '数据过期', pending: '等待采样', error: '采集失败', ok: '采集正常' };
  const roles = { primary: '主库', replica: '副本', standby: '副本', leader: '主库' };
  const runtimes = { linux: 'Linux', k8s: 'Kubernetes', managed: '托管 PG', mysql: 'MySQL' };
  const element = (tag, text, cls) => {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    if (cls) node.className = cls;
    return node;
  };
  const link = (label, href, cls) => Object.assign(element('a', label, cls), { href });

  function create(root, { api, openEvent, formatTime }) {
    const get = key => root.querySelector(`[data-overview="${key}"]`);
    const filterNames = ['organization', 'environment', 'cluster'];
    let visible = false, enabled = false, paused = false, custom = null, lastWindow = null;
    let minutes = 60, offset = 0, timer, request, brush = null;
    let filters = {};

    function window() {
      if (custom) return custom;
      const to = paused && lastWindow ? lastWindow.to : Date.now();
      return { from: to - minutes * minute, to };
    }
    function route(fixed = false) {
      const params = new URLSearchParams();
      for (const name of filterNames) if (filters[name]) params.set(name, filters[name]);
      if (minutes !== 60) params.set('range', String(minutes));
      if (get('interval').value !== '30') params.set('refresh', get('interval').value);
      if (offset) params.set('offset', String(offset));
      const time = fixed ? lastWindow || window() : custom;
      if (time) { params.set('from', iso(time.from)); params.set('to', iso(time.to)); }
      return params.size ? `overview?${params}` : 'overview';
    }
    function save() {
      if (visible && /^#(?:overview(?:\?|$)|$)/.test(location.hash))
        history.replaceState(history.state, '', '#' + route());
    }
    function restore(params) {
      for (const name of filterNames) filters[name] = params.get(name) || '';
      minutes = ['15', '60', '360', '1440'].includes(params.get('range')) ? Number(params.get('range')) : 60;
      get('interval').value = ['15', '30', '60'].includes(params.get('refresh')) ? params.get('refresh') : '30';
      offset = /^\d{1,5}$/.test(params.get('offset') || '') ? Math.min(36000, Number(params.get('offset'))) : 0;
      const from = Date.parse(params.get('from')), to = Date.parse(params.get('to'));
      custom = validWindow(from, to) ? { from, to } : null;
      get('range').value = custom ? 'custom' : String(minutes);
      get('dates').hidden = true;
      if (visible) void load();
    }
    function show(value, supported = enabled) {
      const changed = visible !== (value && supported);
      enabled = supported;
      visible = value && enabled;
      if (!visible) { clearTimeout(timer); request?.abort(); }
      else if (changed) void load();
    }
    function choices(name, values, placeholder) {
      const select = get(name);
      const next = [['', placeholder], ...values];
      if (filters[name] && !next.some(([value]) => value === filters[name])) next.push([filters[name], filters[name]]);
      if (select.options.length !== next.length || next.some(([value, label], index) =>
        select.options[index]?.value !== value || select.options[index]?.textContent !== label)) {
        select.replaceChildren(...next.map(([value, label]) => new Option(label, value)));
      }
      select.value = filters[name] || '';
    }
    function monitoringPath(asset) {
      const time = lastWindow || window();
      const params = new URLSearchParams({ from: iso(time.from), to: iso(time.to), cluster: 'all' });
      if (asset?.runtime === 'mysql') return '#live?' + new URLSearchParams({ asset: asset.id, end: iso(time.to), from: iso(time.from), engine: 'mysql' });
      if (asset) {
        params.set('instance', asset.id);
        if (asset.events) params.set('tab', 'waits');
      }
      return '#monitoring?' + params;
    }
    function render(data) {
      lastWindow = { from: Date.parse(data.from), to: Date.parse(data.to) };
      choices('organization', data.organizations.map(row => [row.id, row.name]), '全部组织');
      choices('environment', data.environments.map(value => [value, value]), '全部环境');
      choices('cluster', data.clusters.map(value => [value, value]), '全部集群');
      get('status').textContent = `更新于 ${timeLabel(data.observedAt)} · ${formatTime(data.from)} — ${formatTime(data.to)}`;
      const s = data.summary;
      get('kpis').replaceChildren(...[
        ['数据库集群', s.clusters, `${s.assets} 个实例`, 'clusters'],
        ['采集正常', s.fresh, `${s.stale} 个待采样或过期`, 'fresh'],
        ['不可用实例', s.unavailable, '当前可用性告警', 'down'],
        ['当前事件', s.events, '按实际纳管范围统计', 'events-kpi']
      ].map(([label, value, hint, cls]) => {
        const card = element('div', '', `operations-kpi operations-${cls}`);
        card.append(element('span', label), element('strong', String(value)), element('small', hint));
        return card;
      }));
      const grouped = new Map();
      for (const asset of data.assets) {
        const group = [asset.organization, asset.environment, asset.cluster || '独立实例'].filter(Boolean).join(' / ');
        if (!grouped.has(group)) grouped.set(group, []);
        grouped.get(group).push(asset);
      }
      get('matrix').replaceChildren(...[...grouped].map(([group, assets]) => {
        const section = element('section', '', 'operations-group');
        section.append(element('h3', group));
        const tiles = element('div', '', 'operations-tiles');
        for (const asset of assets) {
          const phase = asset.unavailable ? 'down' : asset.events ? 'alert' : asset.freshness;
          const tile = link(asset.name, monitoringPath(asset), `operations-tile is-${phase}`);
          tile.prepend(element('span', '', 'operations-dot'));
          tile.append(element('small', [roles[asset.role] || runtimes[asset.runtime], phases[phase] || '状态未知'].filter(Boolean).join(' · ')));
          tile.title = `最近采样 ${formatTime(asset.sampleAt)}`;
          tiles.append(tile);
        }
        section.append(tiles);
        return section;
      }));
      if (!grouped.size) get('matrix').append(element('p', '没有符合条件的实例。', 'empty'));
      get('matrix-caption').textContent = `当前状态 · ${data.assets.length ? offset + 1 : 0}–${Math.min(offset + pageSize, s.assets)} / ${s.assets}`;
      get('prev').disabled = offset === 0;
      get('next').disabled = offset + pageSize >= s.assets;
      get('alerts').replaceChildren(...data.alerts.map(alert => {
        const button = element('button', '', 'operations-alert');
        button.type = 'button';
        button.append(element('strong', alert.title), element('small', `${alert.assetName} · ${formatTime(alert.startedAt)}`));
        button.addEventListener('click', () => openEvent(alert.id, true, route(true)));
        return button;
      }));
      if (!data.alerts.length) get('alerts').append(element('p', '当前没有开放告警。', 'empty'));
      const history = new URLSearchParams({ mode: 'history', from: data.from, to: data.to });
      get('shortcuts').replaceChildren(link('PostgreSQL 看板', monitoringPath()),
        link('实时观测', `#live?${new URLSearchParams({ from: data.from, end: data.to, organization: filters.organization || '', cluster: filters.cluster || '' })}`),
        link(`主机与容器 · Linux ${s.linux} / K8S ${s.kubernetes}`, '#storage?' + history), link('采集系统', '#settings?section=collector'));
      drawTimeline(data.timeline);
    }
    function drawTimeline(rows) {
      const chart = get('timeline');
      chart.dataset.series = JSON.stringify(rows.map(row => row.start));
      const maximum = Math.max(1, ...rows.flatMap(row => [row.opened, row.recovered, row.availability]));
      chart.replaceChildren(...rows.map((row, i) => {
        const button = element('button', '', 'operations-bin');
        button.type = 'button'; button.dataset.bin = String(i);
        button.title = `${timeLabel(row.start)} · 新开 ${row.opened} · 恢复 ${row.recovered} · 可用性告警 ${row.availability}`;
        button.setAttribute('aria-label', button.title);
        for (const name of ['opened', 'recovered', 'availability']) {
          const bar = element('span', '', `bin-${name}`);
          bar.style.setProperty('--height', (row[name] / maximum * 100) + '%');
          button.append(bar);
        }
        button.addEventListener('focus', () => { get('bucket').textContent = button.title; });
        button.addEventListener('pointerenter', () => { get('bucket').textContent = button.title; });
        button.addEventListener('click', event => { if (event.detail === 0) selectRange(i, i, 3); });
        return button;
      }));
      get('axis').replaceChildren(...[0, 9, 18, 27].filter(i => rows[i]).map(i => element('span', timeLabel(rows[i].start))),
        element('span', timeLabel(lastWindow.to)));
    }
    function selectRange(first, last, padding = 0) {
      const starts = JSON.parse(get('timeline').dataset.series || '[]');
      const from = Date.parse(starts[Math.max(0, Math.min(first, last) - padding)]);
      const to = Date.parse(starts[Math.max(first, last) + 1 + padding]) || lastWindow?.to;
      if (!validWindow(from, to)) return;
      custom = { from, to }; get('range').value = 'custom';
      save(); void load();
    }
    async function load() {
      if (!visible) return;
      clearTimeout(timer);
      request?.abort();
      const current = new AbortController();
      request = current;
      const { from, to } = window();
      const params = new URLSearchParams({ ...filters, from: iso(from), to: iso(to), offset });
      try {
        const data = await api('/api/monitoring/overview?' + params, 'GET', undefined, current.signal);
        if (request === current && visible) render(data);
      } catch (error) {
        if (request === current && visible && error.name !== 'AbortError') get('status').textContent = error.message;
      } finally {
        if (request === current && visible && !paused) timer = setTimeout(load, Number(get('interval').value) * 1000);
      }
    }
    for (const name of filterNames) get(name).addEventListener('change', () => {
      filters[name] = get(name).value;
      if (name === 'organization') filters.environment = '';
      if (name !== 'cluster') filters.cluster = '';
      offset = 0; save(); void load();
    });
    get('range').addEventListener('change', () => {
      if (get('range').value === 'custom') {
        const { from, to } = lastWindow || window();
        get('from').value = localInput(from); get('to').value = localInput(to);
        get('dates').hidden = false; get('from').focus(); return;
      }
      minutes = Number(get('range').value); custom = null; lastWindow = null; get('dates').hidden = true;
      save(); void load();
    });
    get('dates').addEventListener('submit', event => {
      event.preventDefault();
      const from = Date.parse(get('from').value), to = Date.parse(get('to').value);
      if (!validWindow(from, to)) { get('status').textContent = '选择 1 分钟至 7 天内的有效历史时段。'; return; }
      custom = { from, to }; get('dates').hidden = true; save(); void load();
    });
    get('interval').addEventListener('change', () => { save(); void load(); });
    get('pause').addEventListener('click', () => {
      paused = !paused;
      get('pause').setAttribute('aria-pressed', String(paused));
      get('pause').textContent = paused ? '恢复刷新' : '暂停刷新';
      if (paused) { clearTimeout(timer); request?.abort(); } else void load();
    });
    get('fullscreen').addEventListener('click', () => {
      if (document.fullscreenElement) void document.exitFullscreen();
      else root.requestFullscreen?.().catch(() => { get('status').textContent = '当前浏览器无法进入全屏。'; });
    });
    document.addEventListener('fullscreenchange', () => get('fullscreen').setAttribute('aria-pressed', String(document.fullscreenElement === root)));
    get('prev').addEventListener('click', () => { offset = Math.max(0, offset - pageSize); save(); void load(); });
    get('next').addEventListener('click', () => { offset += pageSize; save(); void load(); });
    get('timeline').addEventListener('pointerdown', event => { brush = event.target.closest('[data-bin]')?.dataset.bin ?? null; });
    document.addEventListener('pointerup', event => {
      if (brush === null) return;
      const target = event.target.closest('[data-bin]');
      if (target && get('timeline').contains(target) && brush !== target.dataset.bin) selectRange(Number(brush), Number(target.dataset.bin));
      brush = null;
    });
    return { show, restore };
  }

  window.DbSphereOverview = { create };
})();
