(() => {
  'use strict';

  const metrics = [
    { key: 'averageConnectionUsagePercent', title: '连接使用率', unit: '%', ceiling: 100, source: '连接数 / 配置上限' },
    { key: 'transactionsPerSecond', title: 'TPS', unit: '/s', source: '提交与回滚计数差值' },
    { key: 'waiting', title: '活跃等待', unit: '', source: 'pg_stat_activity · 采样均值' },
    { key: 'peakBlockedSessions', title: '阻塞会话', unit: '', source: 'pg_blocking_pids · 桶内峰值' },
    { key: 'averageReplicationLagBytes', title: '复制延迟', unit: ' B', source: 'WAL 差值 · 桶内均值' }
  ];
  const minute = 60000;
  const validId = value => /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(value || '');
  const freshnessLabels = { ok: '采样正常', error: '采集失败', pending: '待采样', stale: '采样陈旧' };
  const roleLabels = { primary: '主库', replica: '备库', standby: '备库' };
  const shortTime = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
  const number = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 });
  const finite = value => typeof value === 'number' && Number.isFinite(value);

  function element(tag, text, className) {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    if (className) node.className = className;
    return node;
  }

  function age(value, now) {
    const at = Date.parse(value);
    if (!Number.isFinite(at)) return '暂无样本';
    const seconds = Math.max(0, Math.round((now - at) / 1000));
    return seconds < 60 ? seconds + ' 秒前' : Math.floor(seconds / 60) + ' 分钟前';
  }

  function clusterKey(asset) {
    return asset.cluster ? [asset.organizationId, asset.engine, asset.cluster].join('/') : asset.id;
  }

  function create(root, { api, openEvent, formatTime, assetRuntimeLabel }) {
    const get = name => root.querySelector('[data-live="' + name + '"]');
    const filters = { organization: get('organization'), cluster: get('cluster'), engine: get('engine'), minutes: get('window') };
    const canvas = get('chart');
    let active = false;
    let source, fallback, retryTimer, integrationTimer, session = 0, request = 0;
    let selection = { organization: '', cluster: '', engine: '', minutes: 60, asset: '', end: null };
    let snapshot, sampleKey = '', filterKey = '', trendKey = '', feedKey = '', points = [], timelineEvents = [];
    let selectedMetric = metrics[0], from = 0, to = 0, focused = -1, chartGeometry, chartStatus = '';

    function route(anchor = false, assetId = selection.asset) {
      const params = new URLSearchParams();
      for (const key of ['organization', 'cluster', 'engine']) {
        if (selection[key]) params.set(key, selection[key]);
      }
      if (assetId) params.set('asset', assetId);
      params.set('window', String(selection.minutes));
      if (selection.end || (anchor && to)) params.set('end', new Date(selection.end || to).toISOString());
      return 'live?' + params;
    }

    function restore(params) {
      const minutes = Number(params.get('window'));
      const end = Date.parse(params.get('end'));
      const engine = params.get('engine');
      selection = {
        organization: validId(params.get('organization')) ? params.get('organization') : '',
        cluster: (params.get('cluster') || '').slice(0, 160),
        engine: ['postgres', 'mysql'].includes(engine) ? engine : '',
        minutes: [5, 15, 60, 360].includes(minutes) ? minutes : 60,
        asset: validId(params.get('asset')) ? params.get('asset') : '',
        end: Number.isFinite(end) && end <= Date.now() + minute && end >= Date.now() - 7 * 86400000 ? end : null
      };
      if (active && snapshot) render();
    }

    function updateRoute() {
      history.replaceState(null, '', '#' + route());
    }

    function detail(event) {
      if (document.fullscreenElement === root) void document.exitFullscreen().catch(() => {});
      void openEvent(event.id, true, route(true, event.assetId));
    }

    function connection(state, text) {
      root.dataset.connection = state;
      get('connection').textContent = text;
    }

    function setVisible(value) {
      if (active === value) return;
      active = value;
      session++;
      if (active) {
        connection('pending', '正在连接');
        if (snapshot) render();
        connect();
        void loadIntegrations();
        integrationTimer = setInterval(loadIntegrations, 15000);
      } else {
        source?.close();
        source = null;
        clearInterval(fallback);
        clearTimeout(retryTimer);
        clearInterval(integrationTimer);
        fallback = retryTimer = null;
        request++;
        trendKey = sampleKey = '';
      }
    }

    async function loadIntegrations() {
      const generation = session;
      try {
        const { prometheus } = await api('/api/live/integrations');
        if (!active || generation !== session) return;
        const label = get('prometheus');
        label.hidden = prometheus === 'disabled';
        label.dataset.state = prometheus;
        label.textContent = prometheus === 'ready' ? 'Prometheus 就绪' : 'Prometheus 暂不可用';
      } catch {
        // Live samples and event details continue to use the native store.
      }
    }

    function accept(value) {
      snapshot = value;
      render();
    }

    function connect() {
      if (!active || source) return;
      if (typeof EventSource !== 'function') {
        startFallback();
        return;
      }
      const stream = new EventSource('/api/live/stream');
      source = stream;
      stream.addEventListener('observation', event => {
        if (stream !== source) return;
        clearInterval(fallback);
        fallback = null;
        connection('live', '实时同步');
        accept(JSON.parse(event.data));
      });
      stream.addEventListener('reconnect', () => {
        if (stream !== source) return;
        stream.close();
        source = null;
        connect();
      });
      stream.onerror = () => {
        if (stream !== source) return;
        stream.close();
        source = null;
        startFallback();
        clearTimeout(retryTimer);
        retryTimer = setTimeout(connect, 10000);
      };
    }

    function startFallback() {
      if (fallback) return;
      const generation = session;
      async function poll() {
        try {
          const value = await api('/api/live/overview');
          if (!active || generation !== session || root.dataset.connection === 'live') return;
          connection('fallback', '轮询更新');
          accept(value);
        } catch {
          if (active && generation === session && root.dataset.connection !== 'live') connection('error', '连接中断');
        }
      }
      connection('fallback', '正在重连');
      void poll();
      fallback = setInterval(poll, 15000);
    }

    function fillSelect(select, values, selected) {
      const key = JSON.stringify(values);
      if (select.dataset.options !== key) {
        select.replaceChildren(...values.map(([value, label]) => new Option(label, value)));
        select.dataset.options = key;
      }
      select.value = selected;
    }

    function orderedEntries(map) {
      return [...map].sort((a, b) => a[1].localeCompare(b[1], 'zh-CN'));
    }

    function render() {
      const { assets = [], events = [] } = snapshot;
      const now = Date.parse(snapshot.observedAt);
      get('recency').textContent = '数据读于 ' + formatTime(snapshot.observedAt) + ' · ' +
        (snapshot.staleAfter ? '采样超过 ' + snapshot.staleAfter + ' 秒标记为陈旧' : '当前按手动采样显示');
      const organizations = new Map(assets.map(asset => [asset.organizationId, asset.organization]));
      if (!organizations.has(selection.organization)) selection.organization = '';
      const withinEngine = assets.filter(asset => (!selection.organization || selection.organization === asset.organizationId) &&
        (!selection.engine || selection.engine === asset.engine));
      const clusters = new Map(withinEngine.map(asset => [clusterKey(asset), asset.cluster || asset.name]));
      if (!clusters.has(selection.cluster)) selection.cluster = '';
      fillSelect(filters.organization, [['', '全部组织'], ...orderedEntries(organizations)], selection.organization);
      fillSelect(filters.cluster, [['', '全部集群'], ...orderedEntries(clusters)], selection.cluster);
      filters.engine.value = selection.engine;
      filters.minutes.value = String(selection.minutes);
      get('follow').hidden = !selection.end;
      const visible = withinEngine.filter(asset => !selection.cluster || clusterKey(asset) === selection.cluster);
      const selected = visible.find(asset => asset.id === selection.asset) || visible[0];
      selection.asset = selected?.id || '';

      const eventAssets = new Set(visible.map(asset => asset.id));
      const isFiltered = !!(selection.organization || selection.cluster || selection.engine);
      const visibleEvents = isFiltered ? events.filter(event => eventAssets.has(event.assetId)) : events;
      const nextFilter = JSON.stringify(visible.map(asset => [asset.id, asset.name, asset.freshness, asset.openEvents, asset.role, asset.pod, asset.node, clusterKey(asset)])) + selection.asset;
      if (nextFilter !== filterKey) {
        filterKey = nextFilter;
        renderTopology(visible, selected);
      }
      get('asset-count').textContent = isFiltered || snapshot.total === assets.length ? visible.length + ' 个实例' : '展示 ' + visible.length + ' / ' + snapshot.total;
      const nextFeed = JSON.stringify(visibleEvents);
      if (nextFeed !== feedKey) {
        feedKey = nextFeed;
        renderFeed(visibleEvents);
      }
      get('asset-title').textContent = selected?.name || '实例趋势';
      get('sample-age').textContent = selected ? freshnessLabels[selected.freshness] + ' · ' + age(selected.collectedAt, now) : '';
      renderKPIs(visible, selected, isFiltered);

      const nextKey = [selected?.id, selected?.sampleId, selected?.freshness, selection.minutes, selection.end,
        ...visibleEvents.filter(event => event.assetId === selected?.id).map(event => event.id + event.status)].join(':');
      if (nextKey !== trendKey) {
        trendKey = nextKey;
        void loadTrend(selected, now);
      }
      const nextSample = [selected?.id, selected?.sampleId, selected?.freshness, selection.end].join(':');
      if (nextSample !== sampleKey) {
        sampleKey = nextSample;
        renderCoverage(selected);
        void loadSQL(selected);
      }
    }

    function renderKPIs(assets, selected, isFiltered) {
      const total = isFiltered ? assets.length : snapshot.total;
      const healthy = isFiltered ? assets.filter(asset => asset.freshness === 'ok').length : snapshot.fresh;
      const issues = isFiltered ? assets.reduce((sum, asset) => sum + asset.openEvents, 0) : snapshot.openEvents;
      const current = selected?.freshness === 'ok';
      const connections = current && finite(selected.connections) && selected.maxConnections > 0
        ? number.format(selected.connections / selected.maxConnections * 100) + '%' : '—';
      const blocked = current && finite(selected.blocked) ? number.format(selected.blocked) : '—';
      const cards = [
        ['正常采样', healthy + ' / ' + total, '可见实例', 'cyan'],
        ['处理中事件', String(issues), '当前监控范围', issues > 0 ? 'alert' : 'cyan'],
        ['连接使用率', connections, selected?.name || '选择实例', 'blue'],
        ['阻塞会话', blocked, selected?.name || '选择实例', blocked !== '—' && selected.blocked > 0 ? 'alert' : 'violet']
      ];
      const list = get('kpis');
      for (const [index, [label, value, description, tone]] of cards.entries()) {
        let card = list.children[index];
        if (!card) {
          card = element('article', '', 'live-kpi');
          card.append(element('span'), element('strong'), element('small'));
          list.append(card);
        }
        card.dataset.tone = tone;
        card.children[0].textContent = label;
        card.children[1].textContent = value;
        card.children[2].textContent = description;
      }
    }

    function renderTopology(assets, selected) {
      const parent = get('topology');
      const groups = new Map();
      for (const asset of assets) {
        const key = clusterKey(asset);
        if (!groups.has(key)) groups.set(key, []);
        groups.get(key).push(asset);
      }
      const focusId = parent.contains(document.activeElement) ? document.activeElement.dataset.asset : null;
      parent.replaceChildren();
      if (!groups.size) parent.append(element('p', '当前范围暂无纳管实例。', 'live-empty'));
      for (const members of groups.values()) {
        const group = element('section', '', 'live-group');
        const asset = members[0];
        const heading = element('div', '', 'live-group-heading');
        heading.append(element('strong', asset.cluster || '独立实例'), element('small', asset.organization));
        group.append(heading);
        for (const member of members) {
          const button = element('button', '', 'live-member');
          button.type = 'button';
          button.dataset.asset = member.id;
          button.dataset.freshness = member.freshness;
          button.setAttribute('aria-pressed', String(member.id === selected?.id));
          const name = element('strong', member.name);
          const runtime = assetRuntimeLabel(member);
          const label = [roleLabels[member.role], member.pod && [member.namespace, member.pod].filter(Boolean).join('/'),
            member.node, runtime, freshnessLabels[member.freshness]].filter(Boolean);
          button.append(name, element('small', label.join(' · ')));
          if (member.openEvents) button.append(element('span', String(member.openEvents), 'live-member-events'));
          button.onclick = () => {
            selection.asset = member.id;
            updateRoute();
            render();
          };
          group.append(button);
        }
        parent.append(group);
      }
      if (focusId) parent.querySelector('[data-asset="' + focusId + '"]')?.focus({ preventScroll: true });
    }

    function renderFeed(events) {
      const parent = get('feed');
      parent.replaceChildren();
      if (!events.length) parent.append(element('p', '当前范围暂无处理中或近期恢复的事件。', 'live-empty'));
      for (const event of events) {
        const button = element('button', '', 'live-event');
        button.type = 'button';
        button.dataset.eventId = event.id;
        button.dataset.status = event.status;
        button.append(element('span', event.status === 'open' ? '处理中' : '已恢复', 'live-event-state'),
          element('strong', event.title), element('small', event.assetName + ' · ' + formatTime(event.endedAt || event.startedAt)));
        button.onclick = () => detail(event);
        parent.append(button);
      }
    }

    function renderCoverage(asset) {
      const parent = get('coverage');
      parent.replaceChildren();
      if (!asset) {
        parent.append(element('p', '选择实例查看采集信号。', 'live-empty'));
        return;
      }
      const values = [
        [asset.engine === 'mysql' ? 'MySQL' : 'PostgreSQL', freshnessLabels[asset.freshness]],
        ['Linux', asset.linux === 'ok' ? '已采样' : asset.linux ? '信号不可用' : '暂无信号'],
        ['Kubernetes', asset.k8s === 'ok' ? '已采样' : asset.k8s ? '信号不可用' : '暂无信号']
      ];
      for (const [label, value] of values) {
        const row = element('div', '', 'live-coverage-row');
        row.append(element('span', label), element('strong', value));
        parent.append(row);
      }
    }

    async function loadSQL(asset) {
      const parent = get('sql');
      const label = get('sql-source');
      const key = sampleKey;
      parent.replaceChildren();
      label.textContent = '';
      if (!asset || !asset.sampleId || asset.engine !== 'postgres') {
        parent.append(element('p', asset?.engine === 'mysql' ? '在资产详情查看 MySQL SQL 指纹。' : '暂无 PostgreSQL SQL 指纹样本。', 'live-empty'));
        return;
      }
      parent.append(element('p', '正在读取 SQL 指纹…', 'live-empty'));
      try {
        const end = selection.end;
        const params = end ? '?' + new URLSearchParams({ from: new Date(end - selection.minutes * minute).toISOString(), to: new Date(end).toISOString() }) : '';
        const { performance } = await api('/api/assets/' + asset.id + '/sql' + params);
        if (!active || key !== sampleKey) return;
        parent.replaceChildren();
        if (performance?.status !== 'ok' || !performance.statements?.length) {
          parent.append(element('p', '当前时间范围暂无可用的 pg_stat_statements 指纹。', 'live-empty'));
          return;
        }
        label.textContent = 'pg_stat_statements · ' + (performance.comparison === 'interval' ? '相邻样本增量' : '累计') + ' · ' + formatTime(performance.collectedAt);
        for (const row of performance.statements.slice(0, 5)) {
          const line = element('div', '', 'live-sql-row');
          const value = row.delta || row;
          line.append(element('code', row.fingerprint || row.queryId),
            element('span', number.format(value.calls) + ' 次'), element('strong', number.format(value.totalExecMs) + ' ms'));
          parent.append(line);
        }
      } catch {
        if (active && key === sampleKey) parent.replaceChildren(element('p', 'SQL 指纹暂不可读取。', 'live-empty'));
      }
    }

    async function loadTrend(asset, now) {
      const current = ++request;
      const selectedTimescale = selection.minutes;
      const bucketMinutes = selectedTimescale === 360 ? 5 : 1;
      const bucketMs = bucketMinutes * minute;
      to = selection.end || now;
      from = to - selectedTimescale * minute;
      const list = get('metrics');
      const enabled = asset?.engine === 'mysql' ? metrics.slice(0, 1) : metrics;
      if (!enabled.includes(selectedMetric)) selectedMetric = enabled[0];
      const metricKey = enabled.map(metric => metric.key).join();
      if (list.dataset.metrics !== metricKey) {
        list.replaceChildren(...enabled.map(metric => {
          const button = element('button', metric.title);
          button.type = 'button';
          button.dataset.metric = metric.key;
          button.onclick = () => {
            selectedMetric = metric;
            focused = -1;
            draw();
          };
          return button;
        }));
        list.dataset.metrics = metricKey;
      }
      if (!asset?.sampleId) {
        points = [];
        timelineEvents = [];
        draw('暂无时间序列');
        return;
      }
      const q = { from: new Date(from).toISOString(), to: new Date(to).toISOString(), bucketMinutes: String(bucketMinutes) };
      const trend = new URLSearchParams({ ...q, asset: asset.id });
      const waits = new URLSearchParams({ ...q, group: 'type' });
      if (points[0]?.assetId !== asset.id || points.range !== selectedTimescale || points.anchor !== selection.end) {
        points = [];
        timelineEvents = [];
        draw('正在读取趋势…');
      }
      try {
        const [{ series, events }, { buckets }] = await Promise.all([
          api('/api/monitoring/series?' + trend),
          asset.engine === 'postgres' ? api('/api/assets/' + asset.id + '/wait-history?' + waits).catch(() => ({ buckets: [] })) : { buckets: [] }
        ]);
        if (!active || current !== request) return;
        const observed = new Map(series.map(row => [Date.parse(row.bucket), row]));
        const waiting = new Map(buckets.map(row => [Date.parse(row.bucket), row.observed > 0
          ? row.groups.reduce((sum, group) => sum + group.averageSessions, 0) : null]));
        const start = Math.floor(from / bucketMs) * bucketMs;
        points = Array.from({ length: Math.floor((to - start) / bucketMs) + 1 }, (_, i) => {
          const at = start + i * bucketMs;
          return { ...observed.get(at), at: Math.max(at, from), waiting: waiting.get(at) };
        });
        points[0].assetId = asset.id;
        points.range = selectedTimescale;
        points.anchor = selection.end;
        timelineEvents = events;
        focused = -1;
        draw('');
      } catch {
        if (active && current === request) draw('趋势暂不可读取');
      }
    }

    function draw(message = chartStatus) {
      if (!active) return;
      chartStatus = message;
      const chartEmpty = get('chart-empty');
      const { width, height } = canvas.getBoundingClientRect();
      if (!width || !height) return;
      const dpr = Math.min(2, window.devicePixelRatio || 1);
      canvas.width = Math.round(width * dpr);
      canvas.height = Math.round(height * dpr);
      const ctx = canvas.getContext('2d');
      ctx.scale(dpr, dpr);
      const color = key => getComputedStyle(root).getPropertyValue('--live-' + key).trim();
      for (const button of get('metrics').children)
        button.setAttribute('aria-pressed', String(button.dataset.metric === selectedMetric.key));
      const values = points.map(point => point[selectedMetric.key]).filter(finite);
      chartEmpty.textContent = message || '此时间范围暂无该指标';
      chartEmpty.hidden = !message && values.length > 0;
      const top = 22, left = 54, bottom = height - 37, right = width - 14;
      const peak = Math.max(1, ...values);
      const magnitude = Math.pow(10, Math.floor(Math.log10(peak)));
      const max = selectedMetric.ceiling || Math.ceil(peak / magnitude) * magnitude;
      const x = at => left + (Math.min(to, Math.max(from, at)) - from) / Math.max(1, to - from) * (right - left);
      const y = value => bottom - Math.min(max, value) / max * (bottom - top);
      chartGeometry = { x, from, to, left, right };
      ctx.font = '11px system-ui';
      ctx.textBaseline = 'middle';
      ctx.lineWidth = 1;
      for (let i = 0; i <= 4; i++) {
        const value = max * i / 4;
        ctx.strokeStyle = color('line');
        ctx.beginPath(); ctx.moveTo(left, y(value)); ctx.lineTo(right, y(value)); ctx.stroke();
        ctx.fillStyle = color('muted');
        ctx.textAlign = 'right';
        ctx.fillText(value >= 10000 ? number.format(value / 1000) + 'k' : number.format(value), left - 7, y(value));
        ctx.textAlign = i === 0 ? 'left' : i === 4 ? 'right' : 'center';
        if (from) ctx.fillText(shortTime.format(from + (to - from) * i / 4), left + (right - left) * i / 4, bottom + 20);
      }
      if (!message) for (const event of timelineEvents.slice(0, 10)) {
        const start = x(Date.parse(event.startedAt));
        const end = x(event.endedAt ? Date.parse(event.endedAt) : to);
        ctx.fillStyle = color(event.status === 'open' ? 'event-fill' : 'recovery-fill');
        ctx.fillRect(start, top, Math.max(2, end - start), bottom - top);
      }
      if (!message && values.length) {
        const gradient = ctx.createLinearGradient(0, top, 0, bottom);
        gradient.addColorStop(0, color('area'));
        gradient.addColorStop(1, color('area-end'));
        let run = [];
        const finish = () => {
          if (!run.length) return;
          ctx.beginPath();
          run.forEach(([px, py], index) => index ? ctx.lineTo(px, py) : ctx.moveTo(px, py));
          ctx.lineTo(run.at(-1)[0], bottom); ctx.lineTo(run[0][0], bottom); ctx.closePath();
          ctx.fillStyle = gradient; ctx.fill();
          ctx.strokeStyle = color('signal'); ctx.fillStyle = color('signal'); ctx.lineWidth = 2;
          ctx.beginPath();
          run.forEach(([px, py], index) => index ? ctx.lineTo(px, py) : ctx.moveTo(px, py));
          ctx.stroke();
          ctx.beginPath(); ctx.arc(run.at(-1)[0], run.at(-1)[1], 3, 0, Math.PI * 2); ctx.fill();
          run = [];
        };
        for (const point of points) {
          const value = point[selectedMetric.key];
          if (finite(value)) run.push([x(point.at), y(value)]);
          else finish();
        }
        finish();
        if (points[focused]) {
          const point = points[focused];
          const px = x(point.at);
          ctx.strokeStyle = color('signal'); ctx.setLineDash([3, 4]); ctx.beginPath();
          ctx.moveTo(px, top); ctx.lineTo(px, bottom); ctx.stroke();
          ctx.setLineDash([]);
        }
      }
      renderChartReadout();
    }

    function renderChartReadout() {
      const point = points[focused] || points.findLast(row => finite(row[selectedMetric.key]));
      const value = point?.[selectedMetric.key];
      const note = selectedMetric.source + (point ? ' · ' + shortTime.format(point.at) + ' · ' +
        (finite(value) ? number.format(value) + selectedMetric.unit : '缺采') : '');
      get('chart-note').textContent = note;
      canvas.setAttribute('aria-label', selectedMetric.title + '。' + note);
      const matching = focused >= 0 && point ? timelineEvents.find(event => Date.parse(event.startedAt) <= point.at &&
        (!event.endedAt || Date.parse(event.endedAt) >= point.at)) : timelineEvents[0];
      const actions = get('aligned-event');
      if (actions.dataset.event !== (matching?.id || '')) {
        actions.dataset.event = matching?.id || '';
        actions.replaceChildren();
        if (matching) {
          const button = element('button', focused >= 0 ? '打开此时段事件' : '查看同窗事件', 'live-event-link');
          button.type = 'button';
          button.dataset.eventId = matching.id;
          button.onclick = () => detail(matching);
          actions.append(button);
        }
      }
    }

    for (const [key, select] of Object.entries(filters)) {
      select.addEventListener('change', () => {
        selection[key] = key === 'minutes' ? Number(select.value) : select.value;
        if (key === 'organization' || key === 'engine') selection.cluster = '';
        if (key === 'minutes') selection.end = null;
        if (snapshot) render();
        updateRoute();
      });
    }
    get('follow').onclick = () => {
      selection.end = null;
      if (snapshot) render();
      updateRoute();
    };
    get('night').onclick = () => {
      const enabled = root.dataset.night !== 'true';
      root.dataset.night = String(enabled);
      get('night').setAttribute('aria-pressed', String(enabled));
      draw();
    };
    const fullscreen = get('fullscreen');
    fullscreen.hidden = !document.fullscreenEnabled;
    fullscreen.onclick = () => {
      if (document.fullscreenElement) void document.exitFullscreen().catch(() => {});
      else void root.requestFullscreen().catch(() => {});
    };
    document.addEventListener('fullscreenchange', () => {
      const full = document.fullscreenElement === root;
      fullscreen.setAttribute('aria-pressed', String(full));
      fullscreen.textContent = full ? '退出全屏' : '全屏';
    });
    canvas.tabIndex = 0;
    canvas.addEventListener('pointermove', event => {
      if (!chartGeometry || !points.length) return;
      const { left, right } = chartGeometry;
      const index = Math.max(0, Math.min(points.length - 1, Math.round((event.offsetX - left) / (right - left) * (points.length - 1))));
      if (focused !== index) { focused = index; draw(); }
    });
    canvas.addEventListener('pointerleave', () => { focused = -1; draw(); });
    canvas.addEventListener('keydown', event => {
      if (!points.length || !['ArrowLeft', 'ArrowRight'].includes(event.key)) return;
      event.preventDefault();
      focused = Math.max(0, Math.min(points.length - 1, (focused < 0 ? points.length - 1 : focused) + (event.key === 'ArrowLeft' ? -1 : 1)));
      draw();
    });
    new ResizeObserver(() => draw()).observe(canvas.parentElement);
    new MutationObserver(() => draw()).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return { route, restore, setVisible };
  }

  window.DbSphereLive = { create };
})();
