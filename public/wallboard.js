(function () {
  'use strict';

  const minute = 60000;
  const windows = [15, 60, 360, 1440];
  const pageSize = 24;
  const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/;
  const number = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 });
  const clock = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
  const chartSpecs = [
    ['CPU 与内存', '主机 · %', [['cpu', 'CPU'], ['memory', '内存']], 100],
    ['磁盘吞吐', '主机 · MiB/s', [['diskReadBps', '读'], ['diskWriteBps', '写']]],
    ['网络收发', '主机 · MiB/s', [['networkReceiveBps', '接收'], ['networkTransmitBps', '发送']]],
    ['磁盘 I/O 延迟', '主机 · ms', [['diskReadMs', '读'], ['diskWriteMs', '写']]],
    ['连接占用', '实例 · %', [['connections', '占用']], 100],
    ['事务吞吐', '当前数据库 · 次/s', [['tps', 'TPS']]],
    ['SQL 平均耗时', '可比较 SQL · ms', [['sqlMs', '执行']]],
    ['等待与阻塞', '实例 · 会话', [['waiting', '等待'], ['blockedSessions', '阻塞']]],
    ['长事务与空闲事务', '实例 · 会话', [['longTransactions', '超 5 分钟'], ['idleInTransaction', '事务中空闲']]],
    ['复制差距', '主库所见 · MiB', [['lag', '最大差距']]],
    ['PgBouncer 排队', '关联连接池 · 客户端', [['poolWaiting', '排队']]],
    ['容器文件缓存', '数据库容器 · MiB', [['activeFile', 'Active file'], ['inactiveFile', 'Inactive file']]]
  ];
  const mebi = new Set(['diskReadBps', 'diskWriteBps', 'networkReceiveBps', 'networkTransmitBps', 'lag', 'activeFile', 'inactiveFile']);
  const statuses = { ok: '正常', stale: '陈旧', error: '失联', pending: '待首采', degraded: '部分缺采' };
  const roles = { primary: '主库', replica: '备库', standby: '备库', standalone: '单实例' };
  const scopeFields = { organization: 36, environment: 80, cluster: 256 };
  const finite = v => typeof v === 'number' && Number.isFinite(v);
  const iso = v => new Date(v).toISOString();
  const short = v => clock.format(new Date(v));
  const metric = (key, raw) => !finite(raw) ? null : mebi.has(key) ? raw / 1048576 : raw;
  const countLabel = v => finite(v) ? number.format(v) : '—';
  const id = v => uuid.test(v || '') ? v : '';
  const currentState = () => ({ organization: '', environment: '', cluster: '', window: 60, page: 0, asset: '', event: '', cursor: null, fixed: null });
  const historyPath = asset => '/api/assets/' + asset + '/wallboard-history';

  function node(tag, className, label) {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (label != null) element.textContent = label;
    return element;
  }

  function makeIcon(kind) {
    const icon = node('span', 'wallboard-icon is-' + kind);
    icon.setAttribute('aria-hidden', 'true');
    return icon;
  }

  function create(root, { api, openEvent, formatTime }) {
    const get = key => root.querySelector('[data-board="' + key + '"]');
    let selection = currentState();
    let snapshot, historyData, currentRange, pending, timer, active = false, paused = false;
    let snapshotKey = '', historyKey = '', renderedAssets = '', renderedEvents = '';
    let lastStatus = '', integrationsReady = false, checkIntegrationAfter = 0;

    const charts = chartSpecs.map(([title, source, series, maximum]) => {
      const card = node('section', 'wallboard-chart');
      const caption = node('header', 'wallboard-chart-title');
      const info = node('div');
      info.append(node('h3', '', title), node('span', 'wallboard-chart-source', source));
      const values = node('div', 'wallboard-chart-values');
      caption.append(info, values);
      const plot = node('button', 'wallboard-plot');
      plot.type = 'button';
      plot.setAttribute('aria-label', title + '，用左右方向键移动同步游标');
      const canvas = node('canvas');
      canvas.setAttribute('aria-hidden', 'true');
      const empty = node('span', 'wallboard-chart-empty');
      const axis = node('div', 'wallboard-axis');
      axis.setAttribute('aria-hidden', 'true');
      const start = node('span'), end = node('span');
      axis.append(start, end);
      plot.append(canvas, empty);
      card.append(caption, plot, axis);
      get('charts').append(card);
      plot.addEventListener('click', ev => {
        if (!currentRange) return;
        const fraction = ev.detail ? Math.max(0, Math.min(1, ev.offsetX / plot.clientWidth)) : 0.5;
        setCursor(currentRange.from + fraction * (currentRange.to - currentRange.from));
      });
      plot.addEventListener('keydown', ev => {
        if (!currentRange || !['ArrowLeft', 'ArrowRight', 'Escape'].includes(ev.key)) return;
        ev.preventDefault();
        if (ev.key === 'Escape') return setCursor(null);
        const step = (historyData?.bucketMinutes || 1) * minute;
        setCursor((selection.cursor ?? currentRange.to) + (ev.key === 'ArrowLeft' ? -step : step));
      });
      return { card, canvas, values, empty, start, end, source: info.lastChild, series, maximum };
    });

    const resize = new ResizeObserver(() => { if (active) drawAll(); });
    resize.observe(get('charts'));
    const theme = new MutationObserver(() => { if (active) drawAll(); });
    theme.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });

    function scope() {
      const params = new URLSearchParams();
      for (const key of Object.keys(scopeFields)) if (selection[key]) params.set(key, selection[key]);
      return params;
    }

    function route(pin = false, asset = selection.asset, range = selection.fixed || (pin ? currentRange : null), event = selection.event, cursor = selection.cursor) {
      const params = scope();
      if (asset) params.set('asset', asset);
      if (event) params.set('event', event);
      if (selection.page) params.set('page', String(selection.page));
      params.set('window', String(selection.window));
      if (range) {
        params.set('from', iso(range.from));
        params.set('end', iso(range.to));
      }
      if (finite(cursor)) params.set('cursor', iso(cursor));
      return 'wallboard?' + params;
    }

    function restore(params) {
      const next = currentState();
      for (const [key, limit] of Object.entries(scopeFields)) next[key] = (params.get(key) || '').slice(0, limit);
      const window = Number(params.get('window'));
      if (windows.includes(window)) next.window = window;
      next.page = Math.max(0, Math.min(10000, Number.parseInt(params.get('page'), 10) || 0));
      next.asset = id(params.get('asset'));
      next.event = id(params.get('event'));
      const from = Date.parse(params.get('from')), to = Date.parse(params.get('end'));
      if (finite(from) && to > from && to - from <= 1440 * minute) next.fixed = { from, to };
      const cursor = Date.parse(params.get('cursor'));
      next.cursor = finite(cursor) ? cursor : null;
      if (JSON.stringify(selection) !== JSON.stringify(next)) {
        selection = next;
        if (active) refreshSelection();
      }
    }

    function saveRoute() {
      window.history.replaceState(null, '', '#' + route());
    }

    function status(kind, message) {
      lastStatus = message;
      get('pulse').dataset.state = kind;
      if (get('status').textContent !== message) get('status').textContent = message;
    }

    function setVersion(version) {
      get('version').textContent = version ? 'v' + version : '';
      get('fullscreen-version').textContent = version ? 'DbSphere v' + version : '';
    }

    function setVisible(visible) {
      if (visible === active) return;
      active = visible;
      if (visible) {
        void refresh(true);
        return;
      }
      clearTimeout(timer);
      pending?.abort();
      pending = null;
      if (document.fullscreenElement === root) void document.exitFullscreen().catch(() => {});
    }

    function reset() {
      setVisible(false);
      selection = currentState();
      snapshot = historyData = currentRange = undefined;
      renderedAssets = renderedEvents = snapshotKey = historyKey = '';
      integrationsReady = paused = false;
      checkIntegrationAfter = 0;
      delete get('dates').dataset.editing;
      get('assets').replaceChildren();
      get('events').replaceChildren();
      render();
    }

    function refreshSelection() {
      const scopeKey = scope().toString() + '/' + selection.page;
      if (scopeKey !== snapshotKey) {
        snapshot = undefined;
        renderedAssets = renderedEvents = '';
        get('assets').replaceChildren();
        get('events').replaceChildren();
        status('pending', '正在读取选定范围');
      }
      historyData = currentRange = undefined;
      historyKey = '';
      render();
      void refresh(true);
    }

    async function refresh(force) {
      clearTimeout(timer);
      if (!active || (paused && !force)) return;
      pending?.abort();
      const request = new AbortController();
      pending = request;
      const signal = request.signal;
      const query = scope();
      const scopeKey = query.toString() + '/' + selection.page;
      query.set('board', 'wallboard');
      query.set('page', String(selection.page));
      if (selection.asset) query.set('asset', selection.asset);
      try {
        const result = await api('/api/live/overview?' + query, 'GET', undefined, signal);
        if (signal.aborted) return;
        snapshot = result;
        snapshotKey = scopeKey;
        if (selection.page && !snapshot.assets.length) {
          selection.page = 0;
          selection.asset = '';
          saveRoute();
          return refreshSelection();
        }
        if (!snapshot.assets.some(asset => asset.id === selection.asset)) selection.asset = snapshot.assets[0]?.id || '';
        status(paused ? 'paused' : 'ok', (paused ? '已暂停 · ' : '更新于 ') + short(snapshot.observedAt));
        const end = Date.parse(snapshot.observedAt);
        const range = selection.fixed || { from: end - selection.window * minute, to: end };
        const key = selection.asset + '/' + range.from + '/' + range.to;
        if (!selection.asset) {
          historyData = undefined;
          currentRange = range;
        }
        render();
        if (selection.asset && key !== historyKey) {
          const rangeQuery = new URLSearchParams({ from: iso(range.from), to: iso(range.to) });
          const data = await api(historyPath(selection.asset) + '?' + rangeQuery, 'GET', undefined, signal);
          if (signal.aborted) return;
          currentRange = range;
          historyData = data;
          historyKey = key;
          render();
        }
        if (Date.now() >= checkIntegrationAfter && document.body.dataset.role === 'admin' &&
            document.body.dataset.organizationScope !== 'true') {
          checkIntegrationAfter = Date.now() + minute;
          try {
            const integration = await api('/api/live/integrations', 'GET', undefined, signal);
            if (signal.aborted) return;
            integrationsReady = false;
            if (integration.grafana === true && integration.prometheus === 'ready') {
              const identity = await api('/grafana/api/user', 'GET', undefined, signal);
              if (signal.aborted) return;
              integrationsReady = /^dbsphere-(?:local-admin|admin-[a-z0-9-]+)$/.test(identity.login || '') && !identity.isGrafanaAdmin;
            }
          } catch {
            if (signal.aborted) return;
            integrationsReady = false;
          }
          renderLinks();
        }
      } catch (error) {
        if (!signal.aborted) status('error', '读取中断 · ' + (snapshot?.observedAt ? '保留 ' + short(snapshot.observedAt) + ' 的快照' : error.message));
      } finally {
        if (pending === request) {
          pending = null;
          if (active && !paused) timer = setTimeout(refresh, 15000);
        }
      }
    }

    function fillOptions(key, entries) {
      const select = get(key);
      if (selection[key] && !entries.some(([value]) => value === selection[key])) entries.push([selection[key], selection[key]]);
      const options = JSON.stringify(entries);
      if (select.dataset.options !== options) {
        select.replaceChildren(...entries.map(([value, label]) => new Option(label, value)));
        select.dataset.options = options;
      }
      select.value = selection[key];
    }

    function renderAssets(assets) {
      const nodes = snapshot?.unassignedHosts || [];
      const key = JSON.stringify([assets, nodes, selection.asset]);
      if (key === renderedAssets) return;
      renderedAssets = key;
      const hostGroups = new Map();
      for (const asset of assets) {
        const key = asset.wallHost?.id || 'unlinked/' + asset.organizationId + '/' + asset.environment;
        if (!hostGroups.has(key)) hostGroups.set(key, []);
        hostGroups.get(key).push(asset);
      }
      const views = [...hostGroups.values()].map(group => {
        const readings = group.filter(asset => asset.wallHost?.status === 'ok').sort((a, b) =>
          (Date.parse(b.wallHost.observedAt) || 0) - (Date.parse(a.wallHost.observedAt) || 0));
        const hostAsset = readings[0] || group[0];
        const host = hostAsset.wallHost;
        const card = node('div', 'wallboard-node');
        const heading = node('button', 'wallboard-host');
        heading.type = 'button';
        const title = node('span', 'wallboard-host-name', host?.name || '未关联主机');
        title.title = title.textContent;
        const detail = node('span', 'wallboard-host-meta',
          [host?.kind, group[0].environment, group.length + ' 个实例'].filter(Boolean).join(' · '));
        const hostTitle = node('span', 'wallboard-host-text');
        hostTitle.append(title, detail);
        const observed = host?.status === 'ok';
        const percent = v => finite(v) ? number.format(v) + '%' : '—';
        const io = observed && finite(host.diskRead) && finite(host.diskWrite)
          ? number.format((host.diskRead + host.diskWrite) / 1048576) + ' MiB/s' : '—';
        const values = node('span', 'wallboard-host-values', observed
          ? 'CPU ' + percent(host.cpu) + ' · 内存 ' + percent(host.memory) + ' · I/O ' + io : '主机指标缺采');
        if (observed && host.observedAt) values.title = (host.source || '主机采集') + ' · ' + formatTime(host.observedAt);
        heading.append(makeIcon('host'), hostTitle, values);
        heading.setAttribute('aria-pressed', String(group.some(asset => asset.id === selection.asset)));
        heading.addEventListener('click', () => selectAsset(hostAsset.id));
        const dbs = node('div', 'wallboard-databases');
        for (const asset of group) {
          const button = node('button', 'wallboard-database');
          button.type = 'button';
          button.dataset.asset = asset.id;
          button.dataset.state = asset.unavailable ? 'error' : asset.freshness;
          button.setAttribute('aria-pressed', String(asset.id === selection.asset));
          const name = node('strong', '', asset.name);
          const role = roles[asset.role] || (asset.engine === 'mysql' ? 'MySQL' : '数据库');
          const availability = asset.unavailable ? '故障' : statuses[asset.freshness] || '缺采';
          const connection = finite(asset.connections) && asset.maxConnections > 0
            ? '连接 ' + asset.connections + '/' + asset.maxConnections : '连接缺采';
          const blockers = finite(asset.blocked) ? '阻塞 ' + asset.blocked : '';
          const text = node('span', 'wallboard-database-text');
          name.title = asset.name;
          text.append(name, node('span', '', [availability, role, asset.cluster, connection, blockers].filter(Boolean).join(' · ')));
          button.append(makeIcon('database'), text);
          if (asset.container) button.append(makeIcon('container'));
          button.addEventListener('click', () => selectAsset(asset.id));
          dbs.append(button);
        }
        card.append(heading, dbs);
        return card;
      });
      for (const host of nodes) {
        const hostName = node('div', 'wallboard-host');
        hostName.append(makeIcon('host'), node('span', 'wallboard-host-name', host.name), node('span', 'wallboard-host-meta', '未关联数据库 · ' + host.source));
        const card = node('div', 'wallboard-node');
        card.append(hostName);
        views.push(card);
      }
      if (!views.length && snapshot) {
        const empty = node('div', 'wallboard-empty', '当前范围内没有可见资产');
        if (document.body.dataset.role === 'admin') {
          const link = node('a', '', '查看纳管资产 ↗');
          link.href = '#assets';
          empty.append(link);
        }
        views.push(empty);
      }
      const container = get('assets');
      const scroll = container.scrollTop;
      const focused = container.contains(document.activeElement) ? document.activeElement.dataset.asset : '';
      container.replaceChildren(...views);
      container.scrollTop = scroll;
      if (id(focused)) container.querySelector('[data-asset="' + focused + '"]')?.focus({ preventScroll: true });
    }

    function openPriority(event) {
      const started = Date.parse(event.startedAt);
      const now = Date.parse(snapshot.observedAt);
      let range = currentRange || selection.fixed || { from: now - selection.window * minute, to: now };
      if (finite(started) && (started < range.from || started > range.to)) {
        range = { from: started - 5 * minute, to: Math.max(started + minute, Math.min(now, started + 15 * minute)) };
      }
      const cursor = finite(started) ? started : selection.cursor;
      const back = new URLSearchParams(route(true, event.assetId, range, event.id, cursor).split('?')[1]);
      back.delete('page');
      void openEvent(event.id, true, 'wallboard?' + back);
    }

    function renderEvents(events) {
      const key = JSON.stringify([events, selection.asset, selection.event, Math.floor(Date.parse(snapshot?.observedAt) / minute)]);
      if (key === renderedEvents) return;
      renderedEvents = key;
      const ranked = events.filter(event => event.id === selection.event)
        .concat(events.filter(event => event.status === 'open' && event.id !== selection.event)).slice(0, 4);
      if (!ranked.length) {
        get('events').replaceChildren(node('div', 'wallboard-empty', snapshot ? '当前无待处理事件' : '正在读取事件'));
        return;
      }
      const duration = event => {
        const elapsed = Math.max(0, Math.floor(((Date.parse(event.endedAt) || Date.parse(snapshot.observedAt)) - Date.parse(event.startedAt)) / minute));
        if (!finite(elapsed)) return '';
        return elapsed >= 60 ? Math.floor(elapsed / 60) + ' 小时 ' + (elapsed % 60) + ' 分钟' : elapsed + ' 分钟';
      };
      get('events').replaceChildren(...ranked.map(event => {
        const button = node('button', 'wallboard-event');
        button.type = 'button';
        button.dataset.event = event.id;
        button.dataset.severity = event.status === 'resolved' ? 'resolved' : event.severity || (event.group === 'availability' ? 'critical' : 'warning');
        button.dataset.related = String(event.assetId === selection.asset);
        const caption = node('span', 'wallboard-event-text');
        const severity = { critical: '严重', warning: '警告', info: '提示', resolved: '已恢复' }[button.dataset.severity];
        caption.append(node('strong', '', event.title), node('span', '', [event.assetName, severity,
          event.status === 'resolved' ? '' : '持续 ' + duration(event)].filter(Boolean).join(' · ')));
        button.append(caption, node('span', 'wallboard-event-arrow', '↗'));
        button.addEventListener('click', () => openPriority(event));
        return button;
      }));
    }

    function renderLinks() {
      const asset = snapshot?.assets.find(item => item.id === selection.asset);
      const selectedPoint = nearestPoint();
      const raw = get('raw'), live = get('live'), grafana = get('grafana');
      raw.hidden = !asset || !selectedPoint?.sampleId;
      if (!raw.hidden) raw.href = '/api/assets/' + asset.id + '/samples/' + selectedPoint.sampleId;
      live.hidden = !asset;
      if (asset) {
        const range = currentRange && currentRange.to - currentRange.from <= 360 * minute ? currentRange : null;
        const query = new URLSearchParams(route(true, asset.id, range).split('?')[1]);
        query.delete('page');
        if (!range) query.set('window', '360');
        live.href = '#live?' + query;
      }
      grafana.hidden = !asset || !integrationsReady || !currentRange ||
        document.body.dataset.role !== 'admin' || document.body.dataset.organizationScope === 'true';
      if (!grafana.hidden) {
        grafana.href = window.DbSphereLive.grafanaPath({ asset: asset.id, organization: asset.organizationId, cluster: asset.cluster,
          engine: asset.engine, event: selection.event, from: currentRange.from, to: currentRange.to,
          section: asset.engine === 'mysql' ? 'mysql' : 'postgres', back: route(true) });
      }
    }

    function render() {
      const assets = snapshot?.assets || [];
      const options = snapshot?.filters || [];
      const sorted = entries => [...new Map(entries)].sort((a, b) => a[1].localeCompare(b[1], 'zh-CN'));
      const orgOptions = options.filter(item => !selection.organization || item.organizationId === selection.organization);
      const envOptions = orgOptions.filter(item => !selection.environment || item.environment === selection.environment);
      fillOptions('organization', [['', '全部组织'], ...sorted(options.map(item => [item.organizationId, item.organization]))]);
      fillOptions('environment', [['', '全部环境'], ...sorted(orgOptions.filter(item => item.environment).map(item => [item.environment, item.environment]))]);
      fillOptions('cluster', [['', '全部集群'], ...sorted(envOptions.map(item => [item.clusterKey, item.cluster]))]);
      get('window').value = selection.fixed ? 'fixed' : String(selection.window);
      get('dates').hidden = !selection.fixed && get('dates').dataset.editing !== 'true';
      if (!get('dates').hidden && selection.fixed && get('dates').dataset.editing !== 'true') fillDates(selection.fixed);
      get('pause').textContent = paused ? '继续' : '暂停';
      get('pause').setAttribute('aria-pressed', String(paused));
      const kpis = [['可见主机', snapshot && snapshot.hostCount + (snapshot.unassignedHosts?.length || 0), 'host'],
        ['数据库实例', snapshot?.total, 'database'], ['处理中异常', snapshot?.openEvents, 'incident'],
        ['陈旧或缺采实例', snapshot?.missingCount, 'missing']];
      get('kpis').replaceChildren(...kpis.map(([name, count, kind]) => {
        const card = node('div', 'wallboard-kpi');
        card.append(makeIcon(kind), node('span', '', name), node('strong', '', countLabel(count)));
        return card;
      }));
      const pages = Math.max(1, Math.ceil((snapshot?.total || 0) / pageSize));
      get('page').textContent = (selection.page + 1) + ' / ' + pages;
      get('prev').disabled = selection.page === 0;
      get('next').disabled = selection.page + 1 >= pages;
      renderAssets(assets);
      renderEvents(snapshot?.events || []);
      const selected = assets.find(asset => asset.id === selection.asset);
      get('focus').textContent = selected ? selected.name : '';
      renderContext();
      renderLinks();
      drawAll();
    }

    function nearestPoint() {
      const points = historyData?.points || [];
      if (!finite(selection.cursor)) return points.at(-1);
      const maximum = (historyData?.bucketMinutes || 1) * minute;
      let closest, distance = maximum + 1;
      for (const point of points) {
        const delta = Math.abs(Date.parse(point.at) - selection.cursor);
        if (delta < distance) {
          closest = point;
          distance = delta;
        }
      }
      return closest;
    }

    function renderContext() {
      const context = get('context');
      context.replaceChildren();
      if (currentRange) context.append(node('span', '', short(currentRange.from) + ' – ' + short(currentRange.to)));
      const point = nearestPoint();
      if (point) context.append(node('span', '', (selection.cursor == null ? '最近样本 ' : '游标样本 ') + formatTime(point.at)));
      if (finite(selection.cursor) && !point) context.append(node('span', '', '游标处缺采 ' + formatTime(iso(selection.cursor))));
      if (point?.hostAt) {
        const hostTime = node('span', '', '主机采样 ' + formatTime(point.hostAt));
        context.append(hostTime);
      }
      const events = historyData?.events || [];
      const event = events.find(item => item.id === selection.event);
      if (event) {
        const button = node('button', 'ghost', '事件 ↗ ' + event.title);
        button.type = 'button';
        button.addEventListener('click', () => void openEvent(event.id, true, route(true)));
        context.append(button);
      }
      if (events.length || historyData?.changes?.length) {
        const markers = node('select');
        markers.setAttribute('aria-label', '跃转到事件或变更');
        markers.append(new Option('事件 / 变更标记', ''));
        for (const item of events) {
          if (Date.parse(item.startedAt) >= currentRange.from) markers.append(new Option(short(item.startedAt) + ' 故障 · ' + item.title, item.startedAt + '|' + item.id));
          if (item.endedAt && Date.parse(item.endedAt) < currentRange.to) markers.append(new Option(short(item.endedAt) + ' 恢复 · ' + item.title, item.endedAt + '|' + item.id));
        }
        for (const item of historyData.changes || []) markers.append(new Option(short(item.at) + ' 变更 · ' + item.label, item.at));
        markers.addEventListener('change', () => {
          const [time, event] = markers.value.split('|');
          if (!time) return;
          selection.event = event || '';
          setCursor(Date.parse(time));
        });
        context.append(markers);
      }
      get('clear').hidden = selection.cursor == null;
    }

    function paint(chart, colors) {
      const { card, canvas, values, empty, start, end, source, series, maximum } = chart;
      const points = historyData?.points || [];
      const point = nearestPoint();
      const entries = series.map(([key, label], i) => {
        const entry = node('span', 'wallboard-series', label + ' ' + countLabel(metric(key, point?.values[key])));
        entry.dataset.series = String(i);
        return entry;
      });
      values.replaceChildren(...entries);
      if (series[0][0] === 'sqlMs') source.textContent = point?.sqlCount ? '可比较 SQL · ' + point.sqlCount + ' 条 · ms' : '可比较 SQL · ms';
      if (series[0][0] === 'activeFile') {
        const container = historyData?.container;
        source.textContent = container ? container + ' · MiB' : '数据库容器 · MiB';
        source.title = source.textContent;
      }
      const bounds = canvas.getBoundingClientRect();
      const width = bounds.width, height = bounds.height;
      if (!width || !height) return;
      const ratio = Math.min(2, devicePixelRatio || 1);
      canvas.width = Math.round(width * ratio);
      canvas.height = Math.round(height * ratio);
      const ctx = canvas.getContext('2d');
      ctx.scale(ratio, ratio);
      ctx.strokeStyle = colors.grid;
      for (const tick of [0.16, 0.5, 0.84]) {
        ctx.beginPath();
        ctx.moveTo(0, height * tick);
        ctx.lineTo(width, height * tick);
        ctx.stroke();
      }
      start.textContent = currentRange ? short(currentRange.from) : '';
      end.textContent = currentRange ? short(currentRange.to) : '';
      const hasData = points.some(row => series.some(([key]) => finite(row.values[key])));
      card.dataset.available = String(hasData);
      empty.hidden = hasData;
      empty.textContent = !snapshot ? '等待读取' : !selection.asset ? '请选择实例' : !historyData ? '等待趋势数据'
        : series[0][0] === 'activeFile' && !historyData.containerObserved ? '未观测到数据库容器'
        : series[0][0] === 'sqlMs' ? '无连续且可比较的 SQL 统计'
        : series[0][0] === 'diskReadMs' ? '无连续设备采样或已完成 I/O' : '此时段缺采';
      if (!currentRange) return;
      const x = at => Math.max(0, Math.min(width, width * (at - currentRange.from) / (currentRange.to - currentRange.from)));
      const max = Math.max(maximum || 0, ...points.flatMap(row => series.map(([key]) => metric(key, row.values[key]) ?? 0)), 0.01);
      function marker(at, color, dash = []) {
        if (!finite(at) || at < currentRange.from || at > currentRange.to) return;
        ctx.beginPath();
        ctx.setLineDash(dash);
        ctx.strokeStyle = color;
        ctx.moveTo(x(at), 0);
        ctx.lineTo(x(at), height);
        ctx.stroke();
        ctx.setLineDash([]);
      }
      for (const event of historyData?.events || []) {
        marker(Date.parse(event.startedAt), colors.error, [3, 3]);
        marker(Date.parse(event.endedAt), colors.ok, [3, 3]);
      }
      for (const change of historyData?.changes || []) marker(Date.parse(change.at), colors.change, [2, 3]);
      const gap = (historyData?.bucketMinutes || 1) * 1.05 * minute;
      for (const [index, [key]] of series.entries()) {
        let previous = null;
        ctx.strokeStyle = ctx.fillStyle = colors.series[index];
        ctx.lineWidth = 1.65;
        for (const row of points) {
          const value = metric(key, row.values[key]);
          if (!finite(value)) {
            previous = null;
            continue;
          }
          const at = Date.parse(row.at);
          const bucket = Date.parse(row.bucket);
          const y = height * (0.9 - 0.78 * value / max);
          const left = x(at);
          ctx.beginPath();
          if (previous && bucket - previous.bucket <= gap) {
            ctx.moveTo(previous.left, previous.y);
            ctx.lineTo(left, y);
            ctx.stroke();
          } else {
            ctx.arc(left, y, 1.75, 0, Math.PI * 2);
            ctx.fill();
          }
          previous = { left, y, bucket };
        }
      }
      ctx.lineWidth = 1;
      marker(selection.cursor, colors.cursor);
    }

    function drawAll() {
      const style = getComputedStyle(root);
      const variable = name => style.getPropertyValue('--wall-' + name).trim();
      const colors = { grid: variable('grid'), error: variable('error'), ok: variable('ok'), change: variable('change'),
        cursor: variable('text'), series: [variable('a'), variable('b')] };
      for (const chart of charts) paint(chart, colors);
    }

    function selectAsset(value) {
      if (selection.asset === value) return;
      Object.assign(selection, { asset: value, event: '', cursor: null });
      saveRoute();
      refreshSelection();
    }

    function setCursor(at) {
      if (finite(at) && currentRange) {
        selection.cursor = Math.min(currentRange.to, Math.max(currentRange.from, at));
        selection.fixed = currentRange;
      } else selection.cursor = null;
      saveRoute();
      render();
    }

    function fillDates(range) {
      const local = at => iso(at - new Date(at).getTimezoneOffset() * minute).slice(0, 16);
      get('from').value = local(range.from);
      get('to').value = local(range.to);
    }

    for (const key of Object.keys(scopeFields)) {
      get(key).addEventListener('change', () => {
        selection[key] = get(key).value;
        if (key === 'organization') selection.environment = '';
        if (key !== 'cluster') selection.cluster = '';
        Object.assign(selection, { page: 0, asset: '', cursor: null, event: '' });
        saveRoute();
        refreshSelection();
      });
    }
    for (const [action, delta] of [['prev', -1], ['next', 1]]) {
      get(action).addEventListener('click', () => {
        selection.page += delta;
        Object.assign(selection, { asset: '', event: '', cursor: null });
        saveRoute();
        refreshSelection();
      });
    }
    get('window').addEventListener('change', () => {
      const value = get('window').value;
      if (value === 'fixed') {
        const to = Date.now();
        fillDates(currentRange || { from: to - selection.window * minute, to });
        get('dates').hidden = false;
        get('dates').dataset.editing = 'true';
        get('from').focus();
        return;
      }
      delete get('dates').dataset.editing;
      Object.assign(selection, { window: Number(value), fixed: null, cursor: null, event: '' });
      saveRoute();
      refreshSelection();
    });
    get('dates').addEventListener('submit', ev => {
      ev.preventDefault();
      const from = Date.parse(get('from').value), to = Date.parse(get('to').value);
      if (!(to > from && to - from <= 1440 * minute)) return status('error', '请选择有效时段，跨度最多 24 小时');
      delete get('dates').dataset.editing;
      Object.assign(selection, { fixed: { from, to }, cursor: null, event: '' });
      saveRoute();
      refreshSelection();
    });
    get('clear').addEventListener('click', () => setCursor(null));
    get('pause').addEventListener('click', () => {
      paused = !paused;
      if (paused) {
        clearTimeout(timer);
        pending?.abort();
        status('paused', '已暂停' + (snapshot ? ' · ' + short(snapshot.observedAt) : ''));
        render();
      } else {
        status('pending', lastStatus);
        render();
        void refresh(true);
      }
    });
    get('fullscreen').addEventListener('click', () => {
      const action = document.fullscreenElement === root ? document.exitFullscreen() : root.requestFullscreen();
      void action.catch(() => status('error', '当前浏览器无法进入全屏'));
    });
    document.addEventListener('fullscreenchange', () => {
      const expanded = document.fullscreenElement === root;
      get('fullscreen').textContent = expanded ? '退出全屏' : '全屏';
      get('fullscreen').setAttribute('aria-pressed', String(expanded));
      if (active) drawAll();
    });
    return { route, restore, setVisible, setVersion, reset };
  }

  window.DbSphereWallboard = { create };
})();
