(() => {
  'use strict';

  const metrics = [
    { key: 'averageConnectionUsagePercent', title: '连接使用率', unit: '%', ceiling: 100, source: '连接数 / 配置上限' },
    { key: 'transactionsPerSecond', title: 'TPS', unit: '/s', source: '提交与回滚计数差值' },
    { key: 'waiting', title: '等待组成', unit: '', source: 'pg_stat_activity · 各类活跃会话采样均值' },
    { key: 'peakBlockedSessions', title: '阻塞会话', unit: '', source: 'pg_blocking_pids · 桶内峰值' },
    { key: 'averageReplicationLagBytes', title: '复制延迟', unit: ' B', source: 'WAL 差值 · 桶内均值' },
    { key: 'ioPressure', title: '存储 I/O 压力', unit: '%', source: '容器 I/O PSI · 选区内分桶均值' }
  ];
  const dm8Metrics = [
    { key: 'dm8Sessions', title: '会话', unit: '', source: 'V$SESSIONS · 窗口均值' },
    { key: 'dm8Active', title: '活动会话', unit: '', source: 'V$SESSIONS · 窗口均值' },
    { key: 'dm8Transactions', title: '事务', unit: '', source: 'V$TRX · 窗口均值' },
    { key: 'dm8Waits', title: '已观测锁等待', unit: '', source: 'V$TRXWAIT · 窗口峰值' }
  ];
  const kingbaseMetrics = [
    { key: 'kingbaseSessions', title: '会话', unit: '', source: 'sys_catalog.sys_stat_activity · 桶内均值' },
    { key: 'kingbaseActive', title: '活动会话', unit: '', source: 'sys_catalog.sys_stat_activity · 桶内均值' },
    { key: 'kingbaseTransactions', title: '事务', unit: '', source: 'sys_catalog.sys_stat_activity · 桶内均值' },
    { key: 'kingbaseWaits', title: '阻塞关系', unit: '', source: 'sys_catalog.sys_locks · 桶内峰值' },
    { key: 'kingbaseSize', title: '数据库容量', unit: ' B', source: 'sys_catalog.sys_database_size · 桶内均值' },
    { key: 'kingbaseReplayLag', title: '复制回放差距', unit: ' B', source: 'sys_catalog.sys_stat_replication · 桶内峰值' }
  ];
  const redisMetrics = [
    { key:'redisOps', title:'OPS', unit:' /s', source:'INFO stats · 每秒操作数的桶内均值' },
    { key:'redisClients', title:'连接', unit:'', source:'INFO clients · 桶内均值' },
    { key:'redisMemory', title:'内存', unit:' B', source:'INFO memory · 桶内均值' },
    { key:'redisHitPercent', title:'命中率', unit:'%', ceiling:100, source:'相邻采样命中与未命中差值 · 桶内均值' },
    { key:'redisEvicted', title:'淘汰键数', unit:'', source:'同进程相邻采样淘汰计数差 · 桶内和' },
    { key:'redisRejected', title:'拒绝连接', unit:'', source:'同进程相邻采样拒绝连接差 · 桶内和' },
    { key:'redisLatency', title:'观测往返', unit:' ms', source:'含连接与 INFO server · 桶内峰值' },
    { key:'redisSlots', title:'正常槽位', unit:'', source:'CLUSTER INFO · 桶内最小值' }
  ];
  const engineNames = {postgres:'PostgreSQL', mysql:'MySQL', dm8:'达梦 DM8', kingbase:'金仓 V9', redis:'Redis'};
  const minute = 60000;
  const validId = value => /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(value || '');
  const freshnessLabels = { ok: '采样正常', error: '采集失败', pending: '待采样', stale: '采样陈旧' };
  const roleLabels = { primary: '主库', replica: '备库', standby: '备库' };
  const shortTime = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
  const number = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 });
  const finite = value => typeof value === 'number' && Number.isFinite(value);
  const waitColors = { CPU: 'blue', IO: 'hot', Lock: 'violet', LWLock: 'signal', Client: 'gold' };
  const severityLabels = { critical: '严重', warning: '警告', info: '提示' };
  const bytes = value => !finite(value) ? '—' : value >= 1048576 ? number.format(value / 1048576) + ' MiB' : number.format(value) + ' B';

  const grafanaPages = { overview: 'dbsphere-overview', postgres: 'dbsphere-postgres', waits: 'dbsphere-waits',
    replication: 'dbsphere-replication', infrastructure: 'dbsphere-infrastructure', mysql: 'dbsphere-mysql',
    dm8: 'dbsphere-dm8',
    kingbase: 'dbsphere-kingbase',
    redis: 'dbsphere-redis',
    containerIO: 'dbsphere-container-io', containerCache: 'dbsphere-container-cache', containerVolumes: 'dbsphere-container-volumes', containerRCA: 'dbsphere-container-rca' };

  function grafanaPath({ asset, organization, cluster, engine, event, eventContext, container, device, pvc, pvcUid, from, to, section, night, back }) {
    const params = new URLSearchParams({ panel: grafanaPages[section] || grafanaPages.overview });
    if (back) params.set('back', back);
    for (const [key, value] of Object.entries({ asset, organization, cluster, engine, event, container })) {
      if (value) params.set('var-' + key, key === 'cluster' ? value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') : value);
    }
    if (['containerIO', 'containerVolumes'].includes(section) && /^\d{1,10}:\d{1,10}$/.test(device || '')) {
      params.set('var-device', device);
    }
    if (section === 'containerVolumes' && /^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/.test(pvc || '')) {
      params.set('var-pvc', pvc.replaceAll('.', '\\\\.'));
    }
    if (section === 'containerVolumes' && /^[a-zA-Z0-9_-]{1,128}$/.test(pvcUid || '')) params.set('var-pvc_uid', pvcUid);
    if (typeof eventContext === 'string' && eventContext.length <= 5200) params.set('var-dbsphere_context', eventContext);
    if (finite(from) && from > 0) params.set('from', String(Math.trunc(from)));
    if (finite(to) && to > 0) params.set('to', String(Math.trunc(to)));
    if (night || document.documentElement.dataset.theme === 'dark') params.set('theme', 'dark');
    return '#grafana?' + params;
  }

  function grafanaFrameURL(route) {
    const panel = route.get('panel');
    if (!Object.values(grafanaPages).includes(panel)) return '';
    const params = new URLSearchParams({ kiosk: '' });
    for (const [key, value] of route) {
      if (/^(?:from|to|theme|var-(?:asset|organization|cluster|engine|event|container|device|pvc|pvc_uid|dbsphere_context))$/.test(key)) {
        params.set(key, value);
      }
    }
    if (!params.has('theme')) params.set('theme', document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light');
    return '/grafana/d/' + panel + '?' + params;
  }

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

  function create(root, { api, openEvent, formatTime, assetRuntimeLabel, openAsset }) {
    const get = name => root.querySelector('[data-live="' + name + '"]');
    const filters = { organization: get('organization'), environment: get('environment'), cluster: get('cluster'),
      engine: get('engine'), minutes: get('window') };
    const canvas = get('chart');
    let active = false, paused = false, compare = false;
    let source, fallback, retryTimer, integrationTimer, session = 0, request = 0;
    let selection = { organization: '', environment: '', cluster: '', engine: '', minutes: 60, asset: '', event: '', start: null, end: null, cursor: null };
    let snapshot, sampleKey = '', filterKey = '', trendKey = '', feedKey = '', points = [], timelineEvents = [];
    let selectedMetric = metrics[0], from = 0, to = 0, focused = -1, chartGeometry, chartStatus = '';
    let trendRequest, pathKey = '', priorityKey = '', markers = [], coverageKey = '';
    let inventory = [];

    function scope() {
      const params = new URLSearchParams();
      for (const key of ['organization', 'environment', 'cluster', 'engine']) if (selection[key]) params.set(key, selection[key]);
      return params;
    }

    function route(anchor = false, assetId = selection.asset) {
      const params = scope();
      if (assetId) params.set('asset', assetId);
      if (selection.event) params.set('event', selection.event);
      params.set('window', String(selection.minutes));
      if (selection.start || (anchor && from)) params.set('from', new Date(selection.start || from).toISOString());
      if (selection.end || (anchor && to)) params.set('end', new Date(selection.end || to).toISOString());
      const cursor = points[focused]?.at || selection.cursor;
      if (cursor && (anchor || selection.cursor)) params.set('cursor', new Date(cursor).toISOString());
      if (compare) params.set('compare', '1');
      return 'live?' + params;
    }

    function restore(params) {
      const previous = scope().toString();
      const start = Date.parse(params.get('from'));
      const end = Date.parse(params.get('end'));
      const now = Date.now();
      const validEnd = Number.isFinite(end) && end <= now + minute && end >= now - 7 * 86400000;
      const validRange = validEnd && Number.isFinite(start) && end > start && end - start <= 360 * minute;
      const minutes = Number(params.get('window')) || (validRange ? Math.round((end - start) / minute) : 60);
      const engine = params.get('engine');
      compare = params.get('compare') === '1';
      selection = {
        organization: validId(params.get('organization')) ? params.get('organization') : '',
        environment: (params.get('environment') || '').slice(0, 80),
        cluster: (params.get('cluster') || '').slice(0, 256),
        engine: Object.hasOwn(engineNames, engine) ? engine : '',
        minutes: [5, 15, 60, 360].includes(minutes) ? minutes : 60,
        asset: validId(params.get('asset')) ? params.get('asset') : '',
        event: validId(params.get('event')) ? params.get('event') : '',
        start: validRange ? start : null,
        end: validEnd ? end : null,
        cursor: validRange && Date.parse(params.get('cursor')) >= start && Date.parse(params.get('cursor')) <= end ? Date.parse(params.get('cursor')) : null
      };
      if (previous !== scope().toString()) rescope();
      if (active && snapshot) render();
    }

    function updateRoute() {
      history.replaceState(null, '', '#' + route());
      get('storage').href = '#storage?' + new URLSearchParams(selection.asset ? { asset: selection.asset } : {});
    }

    function detail(event) {
      if (document.fullscreenElement === root) void document.exitFullscreen().catch(() => {});
      const params = new URLSearchParams(route(true, event.assetId).split('?')[1]);
      params.set('event', event.id);
      const started = Date.parse(event.startedAt);
      if (finite(started) && (started < from || started > to)) {
        const left = started - 5 * minute;
        const right = Math.min(Date.now(), started + 15 * minute);
        params.set('from', new Date(left).toISOString());
        params.set('end', new Date(right).toISOString());
        params.set('cursor', new Date(started).toISOString());
      }
      void openEvent(event.id, true, 'live?' + params);
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
        connection(paused ? 'paused' : 'pending', paused ? '已暂停' : '正在连接');
        if (snapshot) render();
        if (!paused) connect();
        void loadIntegrations();
        integrationTimer = setInterval(loadIntegrations, 15000);
      } else {
        stopStream();
        clearInterval(integrationTimer);
        trendKey = sampleKey = '';
      }
    }

    function stopStream() {
      source?.close();
      source = null;
      clearInterval(fallback);
      clearTimeout(retryTimer);
      fallback = retryTimer = null;
      trendRequest?.abort();
      request++;
    }

    function rescope() {
      stopStream();
      session++;
      snapshot = undefined;
      points = timelineEvents = markers = [];
      trendKey = sampleKey = filterKey = feedKey = pathKey = priorityKey = coverageKey = '';
      for (const key of ['kpis', 'priority', 'feed', 'topology', 'markers', 'coverage', 'path-nodes', 'path-reading']) get(key).replaceChildren();
      if (active) {
        paused = false;
        get('pause').setAttribute('aria-pressed', 'false');
        get('pause').textContent = '暂停刷新';
        connection('pending', '正在读取选定范围');
        draw('正在读取选定范围…');
        connect();
      }
    }

    async function loadIntegrations() {
      const generation = session;
      try {
        const { prometheus, grafana } = await api('/api/live/integrations');
        if (!active || generation !== session) return;
        const label = get('prometheus');
        label.hidden = prometheus === 'disabled';
        label.dataset.state = prometheus;
        label.textContent = prometheus === 'ready' ? 'Prometheus 就绪' : 'Prometheus 暂不可用';
        get('grafana').hidden = !grafana;
      } catch {
        // Live samples and event details continue to use the native store.
      }
    }

    function accept(value) {
      snapshot = value;
      inventory = value.filters || value.assets || [];
      render();
    }

    function syncGrafanaLink() {
      const asset = snapshot?.assets.find(item => item.id === selection.asset);
      const incident = snapshot?.events.find(item => item.assetId === asset?.id && item.status === 'open');
      get('storage').href = '#storage?' + new URLSearchParams(asset?.id ? { asset: asset.id } : {});
      get('grafana').href = grafanaPath({ asset: asset?.id, organization: asset?.organizationId || selection.organization,
        cluster: asset?.cluster, engine: asset?.engine, event: incident?.id, from, to, night: root.dataset.night === 'true',
        back: route(true),
        section: asset?.engine || 'overview' });
    }

    function connect() {
      if (!active || paused || source) return;
      if (typeof EventSource !== 'function') {
        startFallback();
        return;
      }
      const stream = new EventSource('/api/live/stream?' + scope());
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
          const value = await api('/api/live/overview?' + scope());
          if (!active || paused || generation !== session || root.dataset.connection === 'live') return;
          connection('fallback', '轮询更新');
          accept(value);
        } catch {
          if (active && !paused && generation === session && root.dataset.connection !== 'live') connection('error', '连接中断');
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
      const organizations = new Map(inventory.map(asset => [asset.organizationId, asset.organization]));
      const withinOrg = inventory.filter(asset => (!selection.organization || selection.organization === asset.organizationId) &&
        (!selection.engine || selection.engine === asset.engine));
      const environments = [...new Set(withinOrg.map(asset => asset.environment).filter(Boolean))].sort().map(value => [value, value]);
      const clusters = new Map(withinOrg.filter(asset => !selection.environment || asset.environment === selection.environment)
        .map(asset => [asset.clusterKey || clusterKey(asset), asset.cluster || asset.name]));
      fillSelect(filters.organization, [['', '全部组织'], ...orderedEntries(organizations)], selection.organization);
      fillSelect(filters.environment, [['', '全部环境'], ...environments], selection.environment);
      fillSelect(filters.cluster, [['', '全部集群'], ...orderedEntries(clusters)], selection.cluster);
      filters.engine.value = selection.engine;
      filters.minutes.value = String(selection.minutes);
      get('follow').hidden = !selection.end;
      const visible = assets;
      const firstEvent = events.find(event => event.status === 'open');
      const selected = visible.find(asset => asset.id === selection.asset) || visible.find(asset => asset.id === firstEvent?.assetId) || visible[0];
      selection.asset = selected?.id || '';

      const nextFilter = JSON.stringify(visible.map(asset => [asset.id, asset.name, asset.freshness, asset.openEvents, asset.unavailable,
        asset.role, asset.pod, asset.node, clusterKey(asset)])) + selection.asset;
      if (nextFilter !== filterKey) {
        filterKey = nextFilter;
        renderTopology(visible, selected);
      }
      get('asset-count').textContent = snapshot.total === assets.length ? visible.length + ' 个实例' : '展示 ' + visible.length + ' / ' + snapshot.total;
      const nextFeed = JSON.stringify(events);
      if (nextFeed !== feedKey) {
        feedKey = nextFeed;
        renderFeed(events);
      }
      renderPriority(firstEvent, selected, now);
      get('asset-title').textContent = selected?.name || '实例趋势';
      get('sample-age').textContent = selected ? freshnessLabels[selected.freshness] + ' · ' + age(selected.collectedAt, now) : '';
      renderKPIs(selected, now);

      const nextKey = [selected?.id, selected?.sampleId, selected?.freshness, selection.minutes, selection.start, selection.end,
        ...events.filter(event => event.assetId === selected?.id).map(event => event.id + event.status)].join(':');
      if (nextKey !== trendKey) {
        trendKey = nextKey;
        void loadTrend(selected, now);
      }
      const nextSample = [selected?.id, selected?.sampleId, selected?.freshness, selection.start, selection.end].join(':');
      renderCoverage(selected);
      if (get('sql-panel').open && nextSample !== sampleKey) { sampleKey = nextSample; void loadSQL(selected); }
      if (get('path').open) void loadPath(selected);
      syncGrafanaLink();
    }

    function renderPriority(event, selected, now) {
      const asset = snapshot.assets.find(row => row.id === event?.assetId) || selected;
      const key = [event?.id, selected?.id, snapshot.openEvents, snapshot.unavailable, asset?.freshness].join(':');
      const board = get('priority');
      if (key === priorityKey) {
        const duration = board.querySelector('[data-live-duration]');
        if (duration && event) duration.textContent = age(event.startedAt, now);
        return;
      }
      priorityKey = key;
      const title = event?.title || (snapshot.total ? '当前范围暂无处理中事件' : '当前范围暂无实例');
      const summary = element('div', '', 'live-priority-copy');
      const caption = element('small', event ? (severityLabels[event.severity] || (event.group === 'availability' ? '可用性告警' : '处理中')) : '当前状态');
      summary.append(caption, element('strong', title));
      const context = element('div', '', 'live-priority-facts');
      if (event) {
        context.append(element('span', [asset?.cluster, event.assetName].filter(Boolean).join(' / ')));
        const duration = element('span', age(event.startedAt, now));
        duration.dataset.liveDuration = '';
        context.append(duration);
      }
      context.append(element('span', (snapshot.unavailable || 0) + ' 个实例有可用性告警'));
      board.replaceChildren(summary, context);
      board.dataset.alert = String(!!event);
      if (event) {
        const button = element('button', '调查事件 →');
        button.type = 'button';
        button.onclick = () => detail(event);
        board.append(button);
      }
    }

    function renderKPIs(selected, now) {
      const current = selected?.freshness === 'ok';
      const connections = current && finite(selected.connections) && selected.maxConnections > 0
        ? number.format(selected.connections / selected.maxConnections * 100) + '%' : '—';
      const count = key => current && finite(selected[key]) ? number.format(selected[key]) : '—';
      const observed = selected ? age(selected.collectedAt, now) : '暂无样本';
      const availability = ['可用性', snapshot.fresh + ' / ' + snapshot.total, `采样正常 · 不可用 ${snapshot.unavailable}`, snapshot.unavailable ? 'alert' : 'cyan', 'pgAvailable'];
      const native = current ? selected.engineMetrics || {} : {};
      const nativeCount = key => finite(native[key]) ? number.format(native[key]) : '—';
      const cards = selected?.engine === 'redis' && native.mode === 'sentinel' ? [
        availability,
        ['主节点组',native.sentinel?.group || '—',observed,'blue','redisLatency'],
        ['当前主节点',native.sentinel?.master || '—',native.sentinel?.state === 'ok' ? '可用' : '存在缺口','violet','redisLatency'],
        ['副本',finite(native.sentinel?.replicas) ? number.format(native.sentinel.replicas) : '—',observed,'cyan','redisLatency'],
        ['观测往返',nativeCount('latency') === '—' ? '—' : nativeCount('latency') + ' ms',observed,'blue','redisLatency']
      ] : selected?.engine === 'redis' ? [
        availability,
        ['操作 / 秒',nativeCount('ops'),`连接 ${nativeCount('clients')} · ${observed}`,'blue','redisOps'],
        ['命中率',nativeCount('hitPercent') === '—' ? '—' : nativeCount('hitPercent') + '%',observed,'cyan','redisHitPercent'],
        ['窗口淘汰',nativeCount('evicted'),observed,native.evicted > 0 ? 'alert' : 'violet','redisEvicted'],
        ['内存',bytes(native.memory),observed,'cyan','redisMemory']
      ] : selected?.engine === 'kingbase' ? [
        availability,
        ['会话', nativeCount('sessions'), `活动 ${nativeCount('active')} · ${observed}`, 'blue', 'kingbaseSessions'],
        ['事务', nativeCount('transactions'), observed, 'violet', 'kingbaseTransactions'],
        ['阻塞关系', nativeCount('waits'), observed, native.waits > 0 ? 'alert' : 'violet', 'kingbaseWaits'],
        ['数据库容量', bytes(native.size), observed, 'cyan', 'kingbaseSize']
      ] : [
        availability,
        ['连接与排队', connections, `池等待 ${count('poolWaiting')} · ${observed}`, 'blue', 'averageConnectionUsagePercent'],
        ['活跃与等待', count('active'), `阻塞 ${count('blocked')} · ${observed}`, current && selected.blocked > 0 ? 'alert' : 'violet', 'waiting'],
        ['复制差距', bytes(current ? selected.replicationLag : null), `观测字节差 · ${observed}`, 'blue', 'averageReplicationLagBytes'],
        ['存储压力', count('ioPressure') === '—' ? '—' : count('ioPressure') + '%', `I/O PSI · ${observed}`, 'cyan', 'ioPressure']
      ];
      const list = get('kpis');
      for (const [index, [label, value, description, tone, metric]] of cards.entries()) {
        let card = list.children[index];
        if (!card) {
          card = element('article', '', 'live-kpi');
          const spark = element('canvas');
          spark.width = 90;
          spark.height = 24;
          spark.setAttribute('aria-hidden', 'true');
          card.append(element('span'), element('strong'), element('small'), spark);
          list.append(card);
        }
        card.dataset.tone = tone;
        card.dataset.metric = metric;
        card.children[0].textContent = label;
        card.children[1].textContent = value;
        card.children[2].textContent = description;
      }
      drawSparks();
    }

    function drawSparks() {
      for (const card of get('kpis').children) {
        const spark = card.querySelector('canvas');
        const ctx = spark.getContext('2d');
        ctx.clearRect(0, 0, spark.width, spark.height);
        const values = points.slice(-24).map(point => point[card.dataset.metric]);
        const max = Math.max(1, ...values.filter(finite));
        ctx.strokeStyle = getComputedStyle(card).color;
        ctx.lineWidth = 1.6;
        ctx.beginPath();
        let prior = false;
        values.forEach((value, i) => {
          if (!finite(value)) { prior = false; return; }
          const x = i * spark.width / Math.max(1, values.length - 1);
          const y = 22 - 19 * value / max;
          if (prior) ctx.lineTo(x, y); else ctx.moveTo(x, y);
          prior = true;
        });
        ctx.stroke();
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
        if (member.unavailable) button.dataset.unavailable = 'true';
        if (member.openEvents) button.append(element('span', String(member.openEvents), 'live-member-events'));
          button.onclick = () => {
            selection.asset = member.id;
            selection.event = '';
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
        button.append(element('span', event.status === 'open' ? severityLabels[event.severity] || '处理中' : '已恢复', 'live-event-state'),
          element('strong', event.title), element('small', event.assetName + ' · ' + formatTime(event.endedAt || event.startedAt)));
        button.onclick = () => detail(event);
        parent.append(button);
      }
    }

    function renderCoverage(asset) {
      const key = [asset?.id, asset?.freshness, asset?.linux, asset?.k8s].join(':');
      if (key === coverageKey) return;
      coverageKey = key;
      const parent = get('coverage');
      parent.replaceChildren();
      if (!asset) {
        parent.append(element('p', '选择实例查看采集信号。', 'live-empty'));
        return;
      }
      const values = [
        [engineNames[asset.engine] || '数据库', freshnessLabels[asset.freshness]],
        ['Linux', asset.linux === 'ok' ? '已采样' : asset.linux ? '信号不可用' : '暂无信号'],
        ['Kubernetes', asset.k8s === 'ok' ? '已采样' : asset.k8s ? '信号不可用' : '暂无信号']
      ];
      for (const [label, value] of values) {
        const row = element('div', '', 'live-coverage-row');
        row.append(element('span', label), element('strong', value));
        parent.append(row);
      }
    }

    async function loadPath(asset) {
      const point = points[focused] || points.findLast(row => row.latestSampleId);
      const sampleId = selection.end ? point?.latestSampleId : point?.latestSampleId || asset?.sampleId;
      const next = `${asset?.id}:${sampleId}`;
      if (next === pathKey) return;
      pathKey = next;
      const list = get('path-nodes'), detail = get('path-reading');
      list.replaceChildren();
      detail.textContent = sampleId ? '正在读取所选时刻的采样…' : '此时段暂无实例依赖证据。';
      if (!validId(sampleId)) return;
      try {
        const endpoint = `/api/assets/${asset.id}/samples/${sampleId}`;
        const { sample } = await api(endpoint);
        if (next !== pathKey || !active || !get('path').open) return;
        const m = sample?.metrics || {}, route = m.routing, storage = m.containerStorage, node = storage?.node || m.k8s?.pod?.node;
        const groups = [];
        if (route?.entry) groups.push(['访问入口', route.entry, route.probe?.backend ? `入口 → ${route.probe.backend} · ${route.probe.role || '角色未确认'}` : '本次未观测到入口后端']);
        if (route?.kind === 'pgbouncer' || m.pgbouncer?.status === 'ok') groups.push(['PgBouncer', '连接池',
          finite(asset.poolWaiting) && sampleId === asset.sampleId ? `观察到 ${asset.poolWaiting} 个等待客户端` : '查看该采样的连接池记录']);
        groups.push(['PostgreSQL', asset.name, roleLabels[m.role] || '主备角色未确认']);
        if (route?.direct?.backend) groups.push(['直连后端', route.direct.backend, roleLabels[route.direct.role] || '主备角色未确认']);
        if (m.k8s?.pod?.uid || storage?.podUID) groups.push(['Pod', [storage?.namespace || asset.namespace, storage?.pod || asset.pod].filter(Boolean).join('/'), `Pod UID ${storage?.podUID || m.k8s.pod.uid}`]);
        if (storage?.primary && storage.containers?.[storage.primary]?.id) groups.push(['容器', storage.primary, `容器 ID ${storage.containers[storage.primary].id}`]);
        if (node) groups.push(['节点', node, storage?.nodeUID ? `Node UID ${storage.nodeUID}` : '本次采样的运行节点']);
        for (const volume of (storage?.volumeLinks || []).filter(item => item.status === 'claim_verified' && item.pvcUID).slice(0, 4))
          groups.push(['PVC', volume.pvc, `${volume.container} → ${volume.pvcUID}`]);
        for (const [layer, name, reading] of groups) {
          const button = element('button', '', 'live-path-node');
          button.type = 'button';
          button.append(element('small', layer), element('strong', name));
          button.onclick = () => display(reading);
          list.append(button);
        }
        function display(reading) {
          const raw = element('a', '原始采样');
          raw.href = endpoint;
          raw.target = '_blank';
          raw.rel = 'noopener';
          detail.replaceChildren(element('span', `${reading} · ${formatTime(sample?.collectedAt)}`), raw);
        }
        display('采样已记录的身份与连接路径');
      } catch {
        if (active && next === pathKey) detail.textContent = '依赖与原始采样暂不可读取。';
      }
    }

    async function loadSQL(asset) {
      const parent = get('sql');
      const label = get('sql-source');
      const key = sampleKey;
      parent.replaceChildren();
      label.textContent = '';
      if (!asset || !asset.sampleId || asset.engine !== 'postgres') {
        if (asset?.engine === 'kingbase' || asset?.engine === 'redis') {
          const name = asset.engine === 'redis' ? '查看 Redis 慢命令和拓扑' : '查看金仓 SQL 指纹与锁链';
          const button = element('button', name, 'ghost small-button');
          button.type = 'button';
          button.onclick = () => openAsset(asset.id);
          parent.append(button);
          return;
        }
        const message = asset?.engine === 'dm8' ? '达梦实例和原始锁等待请查看资产详情。' :
          asset?.engine === 'mysql' ? '在资产详情查看 MySQL SQL 指纹。' : '暂无 PostgreSQL SQL 指纹样本。';
        parent.append(element('p', message, 'live-empty'));
        return;
      }
      parent.append(element('p', '正在读取 SQL 指纹…', 'live-empty'));
      try {
        const end = selection.end;
        const params = end ? '?' + new URLSearchParams({ from: new Date(selection.start || end - selection.minutes * minute).toISOString(), to: new Date(end).toISOString() }) : '';
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
      trendRequest?.abort();
      const controller = new AbortController();
      trendRequest = controller;
      const read = path => api(path, 'GET', undefined, controller.signal);
      const selectedTimescale = selection.minutes;
      const bucketMinutes = selectedTimescale === 360 || (selection.end - selection.start) * (compare ? 2 : 1) > 360 * minute ? 5 : 1;
      const bucketMs = bucketMinutes * minute;
      to = selection.end || now;
      from = (selection.end && selection.start) || to - selectedTimescale * minute;
      const list = get('metrics');
      const enabled = asset?.engine === 'redis' ? asset.engineMetrics?.mode === 'sentinel' ?
        redisMetrics.filter(metric => metric.key === 'redisLatency') : redisMetrics
        : asset?.engine === 'kingbase' ? kingbaseMetrics :
        asset?.engine === 'dm8' ? dm8Metrics : asset?.engine === 'mysql' ? metrics.slice(0, 1) : metrics;
      if (!enabled.includes(selectedMetric)) selectedMetric = enabled[0];
      const metricKey = enabled.map(metric => metric.key).join();
      if (list.dataset.metrics !== metricKey) {
        list.replaceChildren(...enabled.map(metric => {
          const button = element('button', metric.title);
          button.type = 'button';
          button.dataset.metric = metric.key;
          button.onclick = () => {
            selectedMetric = metric;
            draw();
          };
          return button;
        }));
        list.dataset.metrics = metricKey;
      }
      if (!asset?.sampleId) {
        points = [];
        timelineEvents = [];
        markers = [];
        renderMarkers();
        draw('暂无时间序列');
        return;
      }
      const comparisonStart = from - (to - from);
      const q = { from: new Date(compare ? comparisonStart : from).toISOString(), to: new Date(to).toISOString(),
        bucketMinutes: String(bucketMinutes) };
      const trend = new URLSearchParams({ ...q, asset: asset.id, changes: 'true' });
      if (asset.engine === 'dm8' || asset.engine === 'kingbase' || asset.engine === 'redis') trend.set('metricProfile', asset.engine);
      const waits = new URLSearchParams({ ...q, group: 'type' });
      if (points[0]?.assetId !== asset.id || points.range !== selectedTimescale || points.anchor !== selection.end || points.start !== selection.start) {
        points = [];
        timelineEvents = [];
        draw('正在读取趋势…');
      }
      try {
        const base = `/api/assets/${asset.id}/`;
        const [{ series, events, changes }, { buckets }, storage] = await Promise.all([
          read('/api/monitoring/series?' + trend),
          asset.engine === 'postgres' ? read(base + 'wait-history?' + waits).catch(() => ({ buckets: [] })) : { buckets: [] },
          ['linux', 'k8s'].includes(asset.runtime) ? read(base + 'container-storage?' + new URLSearchParams(q)).catch(() => null) : null
        ]);
        if (!active || current !== request) return;
        const observed = new Map(series.map(row => [Date.parse(row.bucket), row]));
        const waiting = new Map(buckets.filter(row => row.observed > 0).map(row => [Date.parse(row.bucket), row.groups]));
        const incoming = new Map((storage?.buckets || []).map(row => [Date.parse(row.at), row.series?.ioPressure?.mean]));
        const start = Math.floor(from / bucketMs) * bucketMs;
        const priorStart = Math.floor(comparisonStart / bucketMs) * bucketMs;
        const pointAt = at => ({ ...observed.get(at), waitGroups: waiting.get(at),
          waiting: waiting.get(at)?.reduce((sum, group) => sum + group.averageSessions, 0), ioPressure: incoming.get(at) });
        points = Array.from({ length: Math.floor((to - start) / bucketMs) + 1 }, (_, i) => {
          const at = start + i * bucketMs;
          return { ...pointAt(at), at: Math.max(at, from), baseline: compare ? pointAt(priorStart + i * bucketMs) : null };
        });
        points[0].assetId = asset.id;
        points.range = selectedTimescale;
        points.anchor = selection.end;
        points.start = selection.start;
        timelineEvents = events.filter(event => !event.endedAt || Date.parse(event.endedAt) >= from);
        markers = [...timelineEvents.flatMap(event => [{ at: event.startedAt, title: '发现 · ' + event.title, eventId: event.id },
          ...(event.endedAt ? [{ at: event.endedAt, title: '恢复 · ' + event.title, eventId: event.id }] : [])]),
        ...(changes || []).map(change => ({ at: change.at, title: '变更 · ' + change.label }))]
          .filter(item => Date.parse(item.at) >= from && Date.parse(item.at) <= to).sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
        focused = selection.cursor ? points.reduce((best, row, i) => Math.abs(row.at - selection.cursor) < Math.abs(points[best].at - selection.cursor) ? i : best, 0) : -1;
        renderMarkers();
        draw('');
        if (get('path').open) void loadPath(asset);
      } catch {
        if (active && current === request) draw('趋势暂不可读取');
      }
    }

    function renderMarkers() {
      const host = get('markers');
      host.replaceChildren(...markers.slice(-8).map(mark => {
        const at = Date.parse(mark.at);
        const button = element('button', `${shortTime.format(at)} · ${mark.title}`);
        button.type = 'button';
        button.onclick = () => {
          selection.start = at - 5 * minute;
          selection.end = Math.min(Date.now(), at + 15 * minute);
          selection.cursor = at;
          selection.event = mark.eventId || '';
          render();
          updateRoute();
        };
        return button;
      }));
    }

    function draw(message = chartStatus) {
      if (!active) return;
      chartStatus = message;
      get('baseline').setAttribute('aria-pressed', String(compare));
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
      const values = points.flatMap(point => [point[selectedMetric.key], point.baseline?.[selectedMetric.key]]).filter(finite);
      const waitLegend = get('wait-legend');
      const waiting = selectedMetric.key === 'waiting';
      const types = [...new Set(points.flatMap(point => (point.waitGroups || []).map(row => row.waitType)))];
      waitLegend.hidden = !waiting || !types.length;
      if (!waitLegend.hidden && waitLegend.dataset.types !== types.join(',')) {
        waitLegend.dataset.types = types.join(',');
        waitLegend.replaceChildren(...types.map(type => {
          const label = element('span', type || '未知');
          label.dataset.waitColor = waitColors[type] || 'muted';
          return label;
        }));
      }
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
      if (!message) for (const marker of markers) {
        const px = x(Date.parse(marker.at));
        ctx.strokeStyle = color(marker.title.startsWith('恢复') ? 'signal' : marker.title.startsWith('变更') ? 'violet' : 'hot');
        ctx.setLineDash([2, 4]);
        ctx.beginPath(); ctx.moveTo(px, top); ctx.lineTo(px, bottom); ctx.stroke();
        ctx.setLineDash([]);
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
        if (waiting) {
          const width = Math.max(1, (right - left) / Math.max(1, points.length) * .78);
          for (const point of points) {
            let level = 0;
            for (const group of point.waitGroups || []) {
              ctx.fillStyle = color(waitColors[group.waitType] || 'muted');
              const up = level + group.averageSessions;
              ctx.fillRect(x(point.at) - width / 2, y(up), width, y(level) - y(up));
              level = up;
            }
          }
        } else {
          for (const point of points) {
            const value = point[selectedMetric.key];
            if (finite(value)) run.push([x(point.at), y(value)]);
            else finish();
          }
          finish();
        }
        if (compare) {
          ctx.strokeStyle = color('violet'); ctx.lineWidth = 1.3; ctx.setLineDash([5, 4]); ctx.beginPath();
          let adjacent = false;
          for (const point of points) {
            const value = point.baseline?.[selectedMetric.key];
            if (!finite(value)) { adjacent = false; continue; }
            if (adjacent) ctx.lineTo(x(point.at), y(value)); else ctx.moveTo(x(point.at), y(value));
            adjacent = true;
          }
          ctx.stroke(); ctx.setLineDash([]);
        }
        if (points[focused]) {
          const point = points[focused];
          const px = x(point.at);
          ctx.strokeStyle = color('signal'); ctx.setLineDash([3, 4]); ctx.beginPath();
          ctx.moveTo(px, top); ctx.lineTo(px, bottom); ctx.stroke();
          ctx.setLineDash([]);
        }
      }
      renderChartReadout();
      drawSparks();
    }

    function renderChartReadout() {
      const point = points[focused] || points.findLast(row => finite(row[selectedMetric.key]));
      const value = point?.[selectedMetric.key];
      const prior = point?.baseline?.[selectedMetric.key];
      const note = selectedMetric.source + (point ? ' · ' + shortTime.format(point.at) + ' · ' +
        (finite(value) ? number.format(value) + selectedMetric.unit : '缺采或指标不可用') : '') +
        (compare ? ` · 前一等长时段 ${finite(prior) ? number.format(prior) + selectedMetric.unit : '缺少可比样本'}` : '');
      get('chart-note').textContent = note;
      canvas.setAttribute('aria-label', selectedMetric.title + '。' + note);
      const matching = timelineEvents.find(event => event.id === selection.event) || (focused >= 0 && point
        ? timelineEvents.find(event => Date.parse(event.startedAt) <= point.at && (!event.endedAt || Date.parse(event.endedAt) >= point.at))
        : timelineEvents[0]);
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
        const previous = scope().toString();
        selection[key] = key === 'minutes' ? Number(select.value) : select.value;
        if (['organization', 'engine', 'environment'].includes(key)) selection.cluster = '';
        if (key === 'minutes') { selection.start = selection.end = selection.cursor = null; selection.event = ''; }
        if (previous !== scope().toString()) rescope();
        if (snapshot) render();
        updateRoute();
      });
    }
    get('follow').onclick = () => {
      selection.start = selection.end = selection.cursor = null;
      selection.event = '';
      if (snapshot) render();
      updateRoute();
    };
    get('baseline').onclick = () => {
      compare = !compare;
      trendKey = '';
      if (snapshot) render();
      updateRoute();
    };
    get('pause').onclick = () => {
      paused = !paused;
      get('pause').setAttribute('aria-pressed', String(paused));
      get('pause').textContent = paused ? '恢复刷新' : '暂停刷新';
      if (paused) {
        stopStream();
        connection('paused', '已暂停');
      } else {
        trendKey = '';
        connection('pending', '正在连接');
        connect();
      }
    };
    get('sql-panel').addEventListener('toggle', () => { if (get('sql-panel').open && snapshot) render(); });
    get('path').addEventListener('toggle', () => {
      if (get('path').open && snapshot) { pathKey = ''; void loadPath(snapshot.assets.find(asset => asset.id === selection.asset)); }
    });
    get('night').onclick = () => {
      const enabled = root.dataset.night !== 'true';
      root.dataset.night = String(enabled);
      get('night').setAttribute('aria-pressed', String(enabled));
      draw();
      syncGrafanaLink();
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
    canvas.addEventListener('pointerleave', () => { if (!selection.cursor) focused = -1; draw(); });
    canvas.addEventListener('click', () => {
      if (!points[focused]) return;
      selection.cursor = points[focused].at;
      updateRoute();
      if (get('path').open && snapshot) void loadPath(snapshot.assets.find(asset => asset.id === selection.asset));
    });
    canvas.addEventListener('keydown', event => {
      if (!points.length || !['ArrowLeft', 'ArrowRight'].includes(event.key)) return;
      event.preventDefault();
      focused = Math.max(0, Math.min(points.length - 1, (focused < 0 ? points.length - 1 : focused) + (event.key === 'ArrowLeft' ? -1 : 1)));
      selection.cursor = points[focused].at;
      updateRoute();
      draw();
    });
    new ResizeObserver(() => draw()).observe(canvas.parentElement);
    new MutationObserver(() => draw()).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return { route, restore, setVisible };
  }

  window.DbSphereLive = { create, grafanaPath, grafanaFrameURL };
})();
