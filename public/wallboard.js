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
  const engineLabels = {postgres:'PostgreSQL', mysql:'MySQL', dm8:'达梦 DM8', kingbase:'金仓 V9', opengauss:'openGauss', redis:'Redis', etcd:'etcd'};
  const nativeCharts = {
    mysql: [
      ['客户端连接', 'MySQL 连接状态 · 个', [['mysqlClients','连接'],['mysqlRunning','活动']]],
      ['InnoDB 锁等待', 'performance_schema · 组', [['mysqlWaits','阻塞']]],
      ['复制通道', '可见入站复制通道 · 个', [['mysqlChannels','通道']]],
      ['客户端语句速率', 'Questions · 每秒', [['mysqlQueries','语句']]],
      ['缓冲池命中与脏页', 'InnoDB · %', [['mysqlBufferHit','命中率'],['mysqlDirtyPercent','脏页占比']]],
      ['待处理 I/O', 'InnoDB · 次', [['mysqlPendingReads','读取'],['mysqlPendingWrites','写入']]]
    ],
    dm8: [
      ['会话', 'V$SESSIONS · 个', [['dm8Sessions','会话'],['dm8Active','活动']]],
      ['事务与锁等待', 'V$TRX / V$TRXWAIT · 个', [['dm8Transactions','事务'],['dm8Waits','等待']]],
      ['数据文件容量', '可见数据文件 · MiB', [['dm8Allocated','已分配'],['dm8Used','已使用']]],
      ['备库回放差距', '备库已刷盘与已重演 · LSN', [['dm8ReplayGap','回放差距']]],
      ['Data Watch 切换', '守护组累计 · 次', [['dm8Switches','切换']]]
    ],
    kingbase: [
      ['会话', 'sys_stat_activity · 个', [['kingbaseSessions','会话'],['kingbaseActive','活动']]],
      ['事务与锁等待', 'sys_stat_activity / sys_locks · 个', [['kingbaseTransactions','事务'],['kingbaseWaits','等待']]],
      ['数据库容量', 'sys_database_size · MiB', [['kingbaseSize','容量']]],
      ['复制回放差距', 'WAL 收发与回放 · MiB', [['kingbaseLag','回放差距']]],
      ['归档失败', 'sys_stat_archiver 累计 · 次', [['kingbaseArchiveFailures','失败']]]
    ],
    opengauss: [
      ['会话', 'pg_stat_activity · 个', [['opengaussSessions','会话'],['opengaussActive','活动']]],
      ['事务与锁等待', 'pg_stat_activity / pg_thread_wait_status · 个', [['opengaussTransactions','事务'],['opengaussWaits','等待']]],
      ['数据库容量', 'pg_database_size · MiB', [['opengaussSize','容量']]]
    ],
    redis: [
      ['每秒操作', 'INFO stats · OPS', [['redisOps','操作']]],
      ['客户端连接', 'INFO clients · 个', [['redisClients','连接']]],
      ['内存', 'INFO memory · MiB', [['redisMemory','已用']]],
      ['窗口命中率', '相邻采样 · %', [['redisHit','命中率']], 100],
      ['淘汰与拒绝', '相邻采样 · 次', [['redisEvicted','淘汰'],['redisRejected','拒绝']]],
      ['Redis 观测往返', '连接与 INFO server · ms', [['redisLatency','耗时']]]
    ],
    'redis-cluster': [['Redis Cluster 槽位', 'CLUSTER INFO · 个', [['redisSlots','可用'],['redisSlotsFail','故障']]]],
    'redis-sentinel': [
      ['Sentinel 主节点', 'SENTINEL MASTER · 状态', [['redisMaster','可用']], 1],
      ['Sentinel 组成员', 'SENTINEL MASTER · 个', [['redisPeers','其他 Sentinel'],['redisReplicas','副本']]],
      ['Redis 观测往返', '连接与 INFO server · ms', [['redisLatency','耗时']]]
    ],
    etcd: [
      ['Leader 可见状态', 'etcd_server_has_leader · 状态', [['etcdLeader','有 Leader']], 1],
      ['提案积压', 'etcd_server_proposals_pending · 个', [['etcdPending','提案']]],
      ['后端容量与配额', 'etcd 后端 · MiB', [['etcdSize','容量'],['etcdQuota','配额']]],
      ['WAL fsync', '相邻直方图 · ms', [['etcdWAL','平均'],['etcdWALP95','P95 上界']]],
      ['后端提交', '相邻直方图 · ms', [['etcdCommit','平均'],['etcdCommitP95','P95 上界']]],
      ['API Server → etcd', 'Kubernetes /metrics · ms', [['etcdAPI','平均'],['etcdAPIP95','P95 上界']]]
    ]
  };
  const mebi = new Set(['diskReadBps','diskWriteBps','networkReceiveBps','networkTransmitBps','lag','activeFile','inactiveFile',
    'dm8Allocated','dm8Used','kingbaseSize','kingbaseLag','opengaussSize','redisMemory','etcdSize','etcdQuota']);
  const statuses = { ok: '正常', stale: '陈旧', error: '采集失败', pending: '待首采', degraded: '部分缺采' };
  const roles = { primary: '主库', replica: '备库', standby: '备库', standalone: '单实例', leader:'Leader', follower:'Follower', 'no-leader':'暂无 Leader' };
  const boardStates = new Set(['affected', 'missing', 'maintenance', 'recovered', 'healthy']);
  const baselineModes = new Set(['previous', 'day', 'week']);
  const watchKey = 'dbsphere.wallboard.watch';
  const scopeFields = { organization: 36, environment: 80, cluster: 256 };
  const finite = v => typeof v === 'number' && Number.isFinite(v);
  const iso = v => new Date(v).toISOString();
  const short = v => clock.format(new Date(v));
  const metric = (key, raw) => !finite(raw) ? null : mebi.has(key) ? raw / 1048576 : raw;
  const countLabel = v => finite(v) ? number.format(v) : '—';
  const id = v => uuid.test(v || '') ? v : '';
  const currentState = () => ({ organization: '', environment: '', cluster: '', engine: '', state: '', window: 60, page: 0,
    asset: '', event: '', cursor: null, fixed: null, layout: 'cluster', metrics: 'all', compare: '', change: '' });
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

  function create(root, { api, openEvent, formatTime, account }) {
    const get = key => root.querySelector('[data-board="' + key + '"]');
    let selection = currentState();
    let snapshot, historyData, currentRange, pending, timer, active = false, paused = false;
    let baselineData, baselineError = false;
    let savedView = '';
    let viewDirectory = {views: [], teams: [], canSave: false};
    let viewGeneration = 0;
    let viewRequest;
    const viewAPI = '/api/duty-views';
    let snapshotKey = '', historyKey = '', renderedAssets = '', renderedEvents = '';
    let lastStatus = '', integrationsReady = false, checkIntegrationAfter = 0;
    const renderedHealth = new Map();
    let watches = [];
    try {
      const saved = JSON.parse(localStorage.getItem(watchKey));
      if (Array.isArray(saved)) watches = [...new Set(saved.filter(value => id(value)))].slice(0, 4);
    } catch {}
    const chartDialog = get('chart-dialog');
    const largePlot = get('chart-plot');
    const largeChart = {
      card: chartDialog, canvas: largePlot.querySelector('canvas'), values: get('chart-values'),
      empty: get('chart-empty'), start: get('chart-start'), end: get('chart-end'),
      source: get('chart-source'), scale: get('chart-scale')
    };

    let charts = [], chartProfile = '';
    function configureCharts(asset) {
      const engine = asset?.engine || selection.engine || 'postgres';
      const profile = engine === 'redis' && ['cluster','sentinel'].includes(asset?.engineMetrics?.mode) ?
        'redis-' + asset.engineMetrics.mode : engine;
      if (profile === chartProfile) return;
      chartProfile = profile;
      if (chartDialog.open) chartDialog.close();
      get('charts').replaceChildren();
      const specs = profile === 'redis-cluster' ? [...nativeCharts.redis,...nativeCharts[profile]] : nativeCharts[profile] || chartSpecs;
      charts = specs.map(([title, source, series, maximum], index) => {
        const card = node('section', 'wallboard-chart');
        card.dataset.group = specs === chartSpecs && (index <= 3 || index === specs.length - 1) ? 'infra' : 'pg';
        const caption = node('header', 'wallboard-chart-title');
        const info = node('div');
        info.append(node('h3', '', title), node('span', 'wallboard-chart-source', source));
        const values = node('div', 'wallboard-chart-values');
        caption.append(info, values);
        const plot = node('button', 'wallboard-plot');
        plot.type = 'button';
        plot.setAttribute('aria-label', '放大' + title);
        plot.setAttribute('aria-haspopup', 'dialog');
        const canvas = node('canvas');
        canvas.setAttribute('aria-hidden', 'true');
        const empty = node('span', 'wallboard-chart-empty');
        const axis = node('div', 'wallboard-axis');
        axis.setAttribute('aria-hidden', 'true');
        const start = node('span'), end = node('span');
        axis.append(start, end);
        const expand = node('span', 'wallboard-expand-icon', '⤢');
        expand.setAttribute('aria-hidden', 'true');
        plot.append(canvas, empty, expand);
        card.append(caption, plot, axis);
        get('charts').append(card);
        plot.addEventListener('click', () => {
          get('chart-heading').textContent = title;
          largeChart.source.textContent = source;
          largeChart.source.removeAttribute('title');
          Object.assign(largeChart, { series, maximum });
          chartDialog.showModal();
          drawAll();
        });
        plot.addEventListener('keydown', ev => {
          if (ev.key === 'Escape' && selection.cursor != null) {
            ev.preventDefault();
            setCursor(null);
          } else moveCursor(ev);
        });
        return { card, canvas, values, empty, start, end, source: info.lastChild, series, maximum };
      });
    }

    get('chart-close').addEventListener('click', () => chartDialog.close());
    largePlot.addEventListener('keydown', moveCursor);
    largePlot.addEventListener('click', ev => {
      if (!currentRange) return;
      const bounds = largePlot.getBoundingClientRect();
      const fraction = ev.detail ? (ev.clientX - bounds.left) / bounds.width : 0.5;
      setCursor(currentRange.from + fraction * (currentRange.to - currentRange.from));
    });

    const resize = new ResizeObserver(() => { if (active) drawAll(); });
    resize.observe(get('charts'));
    resize.observe(largePlot);
    const themeButton = get('theme-toggle');
    function syncTheme() {
      themeButton.textContent = document.documentElement.dataset.theme === 'dark' ? '浅色主题' : '深色主题';
      if (active) drawAll();
    }
    themeButton.addEventListener('click', () => {
      const select = document.getElementById('theme-select');
      select.value = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
      select.dispatchEvent(new Event('change'));
    });
    const theme = new MutationObserver(syncTheme);
    theme.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    syncTheme();

    function scope() {
      const params = new URLSearchParams();
      for (const key of Object.keys(scopeFields)) if (selection[key]) params.set(key, selection[key]);
      if (selection.engine) params.set('engine', selection.engine);
      if (selection.state) params.set('state', selection.state);
      return params;
    }

    function route(pin = false, asset = selection.asset, range = selection.fixed || (pin ? currentRange : null), event = selection.event, cursor = selection.cursor) {
      const params = scope();
      if (asset) params.set('asset', asset);
      if (event) params.set('event', event);
      if (selection.page) params.set('page', String(selection.page));
      params.set('window', String(selection.window));
      if (selection.layout !== 'cluster') params.set('layout', selection.layout);
      if (selection.metrics !== 'all') params.set('metrics', selection.metrics);
      if (selection.compare) params.set('compare', selection.compare);
      if (selection.change) params.set('change', selection.change);
      if (range) {
        params.set('from', iso(range.from));
        params.set('end', iso(range.to));
      }
      if (finite(cursor)) params.set('cursor', iso(cursor));
      return 'wallboard?' + params;
    }

    function savedPath() {
      const params = new URLSearchParams(route().split('?')[1]);
      params.set('pins', '1');
      for (const watch of watches) params.append('watch', watch);
      return 'wallboard?' + params;
    }

    const savedKey = () => 'dbsphere.wallboard.views.' + JSON.stringify(account());
    function browserViews() {
      try {
        const values = JSON.parse(localStorage.getItem(savedKey()));
        return Array.isArray(values) ? values.filter(row => typeof row?.name === 'string' && row.name.length <= 72 &&
          typeof row.path === 'string' && row.path.length <= 4000 && row.path.startsWith('wallboard?')).slice(0, 20) : [];
      } catch { return []; }
    }

    function renderSavedViews() {
      const select = get('saved-view');
      const {views, teams, canSave} = viewDirectory;
      const labelKey = JSON.stringify(views.map(({id, name, team}) => [id, name, team]));
      if (select.dataset.options !== labelKey) {
        select.replaceChildren(new Option('选择视图', ''), ...views.map(view =>
          new Option(view.team ? view.name + ' · ' + view.team : view.name, view.id)));
        select.dataset.options = labelKey;
      }
      const teamSelect = get('view-team');
      const teamKey = JSON.stringify(teams);
      if (teamSelect.dataset.options !== teamKey) {
        const selected = teamSelect.value;
        teamSelect.replaceChildren(new Option('个人', ''), ...teams.map(team => new Option(team.name, team.id)));
        teamSelect.dataset.options = teamKey;
        if (teams.some(team => team.id === selected)) teamSelect.value = selected;
      }
      const selected = views.find(view => view.id === savedView);
      select.value = savedView;
      get('delete-view').disabled = !selected?.canEdit;
      get('save-view').hidden = !canSave;
      get('save-view-button').disabled = !canSave;
      get('import-views').hidden = !canSave || !browserViews().some(view =>
        !views.some(existing => !existing.teamId && existing.name === view.name));
      get('share-team-view').hidden = !selected?.teamId;
    }

    function fetchViews() {
      if (viewRequest) return viewRequest;
      const generation = viewGeneration;
      viewRequest = api(viewAPI).then(result => {
        if (generation !== viewGeneration) return;
        viewDirectory = {views: result.views || [], teams: result.teams || [], canSave: result.canSave};
        renderSavedViews();
      }).finally(() => { if (generation === viewGeneration) viewRequest = undefined; });
      return viewRequest;
    }

    function openSavedView(view) {
      savedView = view.id;
      get('view-name').value = view.name;
      get('view-team').value = view.teamId || '';
      get('view-status').textContent = `已打开 ${view.name}`;
      renderSavedViews();
      location.hash = view.path;
    }

    function restore(params) {
      const shared = id(params.get('view'));
      if (shared) {
        const source = location.hash;
        const generation = viewGeneration;
        void fetchViews().then(() => {
          if (generation !== viewGeneration || location.hash !== source) return;
          const view = viewDirectory.views.find(row => row.id === shared && row.teamId);
          if (view) openSavedView(view);
          else get('view-status').textContent = '团队视图不可用或当前账号无权打开。';
        }).catch(error => {
          if (generation === viewGeneration && location.hash === source) get('view-status').textContent = error.message;
        });
        return;
      }
      const next = currentState();
      for (const [key, limit] of Object.entries(scopeFields)) next[key] = (params.get(key) || '').slice(0, limit);
      if (Object.hasOwn(engineLabels, params.get('engine'))) next.engine = params.get('engine');
      if (boardStates.has(params.get('state'))) next.state = params.get('state');
      const window = Number(params.get('window'));
      if (windows.includes(window)) next.window = window;
      next.page = Math.max(0, Math.min(10000, Number.parseInt(params.get('page'), 10) || 0));
      next.asset = id(params.get('asset'));
      next.event = id(params.get('event'));
      if (params.get('layout') === 'host') next.layout = 'host';
      if (['pg', 'infra'].includes(params.get('metrics'))) next.metrics = params.get('metrics');
      if (baselineModes.has(params.get('compare'))) next.compare = params.get('compare');
      if (next.compare === 'previous' && Number.isFinite(Date.parse(params.get('change')))) next.change = params.get('change');
      const from = Date.parse(params.get('from')), to = Date.parse(params.get('end'));
      if (finite(from) && to > from && to - from <= 1440 * minute) next.fixed = { from, to };
      const cursor = Date.parse(params.get('cursor'));
      next.cursor = finite(cursor) ? cursor : null;
      if (params.get('pins') === '1') watches = [...new Set(params.getAll('watch').map(id).filter(Boolean))].slice(0, 4);
      if (JSON.stringify(selection) !== JSON.stringify(next)) {
        selection = next;
        if (active) refreshSelection();
      } else if (active && params.get('pins') === '1') render();
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
        void fetchViews().catch(error => { if (active) get('view-status').textContent = error.message; });
        void refresh(true);
        return;
      }
      clearTimeout(timer);
      pending?.abort();
      pending = null;
      if (chartDialog.open) chartDialog.close();
      if (document.fullscreenElement === root) void document.exitFullscreen().catch(() => {});
    }

    function reset() {
      setVisible(false);
      selection = currentState();
      snapshot = historyData = currentRange = undefined;
      baselineData = undefined;
      baselineError = false;
      savedView = '';
      viewGeneration++;
      viewRequest = undefined;
      viewDirectory = {views: [], teams: [], canSave: false};
      get('view-name').value = '';
      get('view-status').textContent = '';
      get('view-url').hidden = true;
      renderedAssets = renderedEvents = snapshotKey = historyKey = '';
      integrationsReady = paused = false;
      renderedHealth.clear();
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
      baselineData = undefined;
      baselineError = false;
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
        const key = selection.asset + '/' + range.from + '/' + range.to + '/' + selection.compare;
        if (!selection.asset) {
          historyData = undefined;
          baselineData = undefined;
          currentRange = range;
        }
        render();
        if (selection.asset && key !== historyKey) {
          const target = selection.asset;
          const loadHistory = window => api(historyPath(target) + '?' + new URLSearchParams({ from: iso(window.from), to: iso(window.to) }),
            'GET', undefined, signal);
          const reference = baselineWindow(range);
          const [data, baseline] = await Promise.all([loadHistory(range), reference ? loadHistory(reference).catch(() => null) : undefined]);
          if (signal.aborted) return;
          currentRange = range;
          historyData = data;
          baselineData = baseline;
          baselineError = !!reference && !baseline;
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

    function baselineWindow(range) {
      if (!selection.compare) return null;
      const length = range.to - range.from;
      const shifted = new Date(range.from);
      if (selection.compare === 'day') shifted.setDate(shifted.getDate() - 1);
      if (selection.compare === 'week') shifted.setDate(shifted.getDate() - 7);
      const from = selection.compare === 'previous' ? range.from - length : shifted.getTime();
      return { from, to: from + length };
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
      const layout = get('layout').value;
      const key = JSON.stringify([assets, nodes, selection.asset, layout, watches]);
      if (key === renderedAssets) return;
      renderedAssets = key;
      const hostGroups = new Map();
      for (const asset of assets) {
        const key = asset.wallHost?.id || 'unlinked/' + asset.organizationId + '/' + asset.environment;
        if (!hostGroups.has(key)) hostGroups.set(key, []);
        hostGroups.get(key).push(asset);
      }
      const views = layout === 'cluster' ? renderMatrix(assets) : [...hostGroups.values()].map(group => {
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
          const role = roles[asset.role] || ({mysql:'MySQL', dm8:'达梦 DM8', kingbase:'金仓 V9', opengauss:'openGauss', redis:'Redis', etcd:'etcd'}[asset.engine] || '数据库');
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
      for (const host of layout === 'host' ? nodes : []) {
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
      const active = document.activeElement;
      const focused = container.contains(active) && active.dataset.asset;
      const focusedPin = active?.classList.contains('wallboard-pin');
      container.dataset.layout = layout;
      container.replaceChildren(...views);
      container.scrollTop = scroll;
      if (id(focused)) container.querySelector((focusedPin ? '.wallboard-pin' : '.wallboard-database') + '[data-asset="' + focused + '"]')?.focus({ preventScroll: true });
      renderWatch(assets);
    }

    function matrixState(asset) {
      if (asset.unavailable || asset.freshness === 'error' || asset.openEvents) return 'affected';
      if (asset.maintenanceUntil) return 'maintenance';
      if (asset.freshness !== 'ok') return 'missing';
      return asset.recoveredAt ? 'recovered' : 'healthy';
    }

    function renderMatrix(assets) {
      const groups = new Map();
      for (const asset of assets) {
        const key = (asset.clusterKey || asset.id) + '/' + asset.environment;
        if (!groups.has(key)) groups.set(key, []);
        groups.get(key).push(asset);
      }
      const labels = { affected: '异常', maintenance: '维护中', missing: '缺采', recovered: '已恢复', healthy: '正常' };
      return [...groups.values()].map(group => {
        const region = node('section', 'wallboard-cluster');
        const first = group[0];
        region.append(node('h3', '', first.cluster || first.name), node('small', '',
          [engineLabels[first.engine], first.organization, first.environment, group.length + ' 个实例'].filter(Boolean).join(' · ')));
        const grid = node('div', 'wallboard-cluster-grid');
        for (const asset of group) {
          const state = matrixState(asset);
          const card = node('div', 'wallboard-tile');
          card.dataset.state = state;
          card.dataset.engine = asset.engine;
          if (renderedHealth.has(asset.id) && renderedHealth.get(asset.id) !== state) card.dataset.transition = 'true';
          renderedHealth.set(asset.id, state);
          const button = node('button', 'wallboard-database');
          button.type = 'button';
          button.dataset.asset = asset.id;
          button.setAttribute('aria-pressed', String(asset.id === selection.asset));
          const text = node('span', 'wallboard-database-text');
          const reason = asset.unavailable ? '端点失联' : asset.freshness === 'error' ? '采集失败' :
            state === 'missing' ? statuses[asset.freshness] || '缺采' : labels[state];
          text.append(node('strong', '', asset.name), node('span', '',
            [reason, roles[asset.role], asset.collectedAt ? '采样 ' + short(asset.collectedAt) : '待首采'].filter(Boolean).join(' · ')));
          button.append(makeIcon('database'), text);
          button.title = [asset.name, reason, asset.collectedAt && formatTime(asset.collectedAt),
            asset.maintenanceUntil && '维护至 ' + formatTime(asset.maintenanceUntil)].filter(Boolean).join(' · ');
          button.addEventListener('click', () => selectAsset(asset.id));
          const pinned = watches.includes(asset.id);
          const pin = node('button', 'wallboard-pin', pinned ? '★' : '☆');
          pin.type = 'button'; pin.dataset.asset = asset.id;
          pin.setAttribute('aria-label', (pinned ? '取消关注 ' : '关注 ') + asset.name);
          pin.setAttribute('aria-pressed', String(pinned));
          pin.disabled = !pinned && watches.length >= 4;
          pin.addEventListener('click', () => toggleWatch(asset.id));
          card.append(button, pin);
          grid.append(card);
        }
        region.append(grid);
        return region;
      });
    }

    function toggleWatch(asset) {
      if (watches.includes(asset)) watches = watches.filter(value => value !== asset);
      else if (watches.length < 4) watches = [...watches, asset];
      try { localStorage.setItem(watchKey, JSON.stringify(watches)); } catch {}
      renderAssets(snapshot?.assets || []);
    }

    function renderWatch(assets) {
      const bar = get('watch');
      bar.hidden = !watches.length;
      if (bar.hidden) return;
      bar.replaceChildren(node('strong', '', '关注 ' + watches.length + '/4'));
      for (const asset of assets.filter(item => watches.includes(item.id))) {
        const chip = node('button', 'ghost', asset.name + ' · ' + countLabel(asset.connections) + ' 连接');
        chip.type = 'button';
        chip.setAttribute('aria-pressed', String(selection.asset === asset.id));
        chip.addEventListener('click', () => selectAsset(asset.id));
        bar.append(chip);
      }
      const clear = node('button', 'ghost', '清除关注');
      clear.type = 'button';
      clear.addEventListener('click', () => {
        watches = [];
        try { localStorage.removeItem(watchKey); } catch {}
        renderAssets(snapshot?.assets || []);
        get('layout').focus();
      });
      bar.append(clear);
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
      back.delete('state');
      void openEvent(event.id, true, 'wallboard?' + back);
    }

    function renderEvents(events) {
      const list = get('event-list').value;
      const key = JSON.stringify([events, selection.asset, selection.event, list, Math.floor(Date.parse(snapshot?.observedAt) / minute)]);
      if (key === renderedEvents) return;
      renderedEvents = key;
      const ranked = events.filter(event => event.id === selection.event)
        .concat(events.filter(event => event.status === list && event.id !== selection.event)).slice(0, 8);
      if (!ranked.length) {
        get('events').replaceChildren(node('div', 'wallboard-empty', snapshot ?
          (list === 'open' ? '当前无待处理事件' : '近 6 小时没有恢复事件') : '正在读取事件'));
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
        if (asset.engine) query.set('engine', asset.engine);
        if (!range) query.set('window', '360');
        live.href = '#live?' + query;
      }
      grafana.hidden = !asset || !integrationsReady || !currentRange ||
        document.body.dataset.role !== 'admin' || document.body.dataset.organizationScope === 'true';
      if (!grafana.hidden) {
        grafana.href = window.DbSphereLive.grafanaPath({ asset: asset.id, organization: asset.organizationId, cluster: asset.cluster,
          engine: asset.engine, event: selection.event, from: currentRange.from, to: currentRange.to,
          section: asset.engine, back: route(true) });
      }
    }

    const engineCards = new Map([['', '全部引擎'], ...Object.entries(engineLabels)].map(([engine, label]) => {
      const card = node('button', 'wallboard-engine');
      card.type = 'button';
      card.dataset.engine = engine;
      card.setAttribute('aria-label', label);
      const title = node('span', 'wallboard-engine-title');
      const count = node('strong');
      title.append(makeIcon('database'), node('span', '', label), count);
      const meter = node('span', 'wallboard-engine-meter');
      const bars = ['fresh', 'missing', 'failed'].map(key => {
        const bar = node('span');
        bar.dataset.segment = key;
        return bar;
      });
      meter.append(...bars);
      meter.setAttribute('aria-hidden', 'true');
      const summary = node('span', 'wallboard-engine-summary');
      card.append(title, meter, summary);
      card.addEventListener('click', () => {
        const next = selection.engine === engine ? '' : engine;
        if (next === selection.engine) return;
        Object.assign(selection, { engine: next, cluster: '', page: 0, asset: '', event: '', cursor: null });
        saveRoute();
        refreshSelection();
      });
      get('engines').append(card);
      return [engine,{card,count,bars,summary}];
    }));

    function renderEngines() {
      const counts = new Map((snapshot?.engines || []).map(item => [item.engine,item]));
      const total = {total:0,fresh:0,missing:0,failed:0,unavailable:0,affected:0};
      for (const count of counts.values()) {
        for (const key of Object.keys(total)) total[key] += count[key] || 0;
      }
      counts.set('',total);
      for (const [engine, info] of engineCards) {
        const row = counts.get(engine);
        info.card.hidden = !!engine && !row && selection.engine !== engine;
        info.card.setAttribute('aria-pressed', String(selection.engine === engine));
        info.count.textContent = countLabel(snapshot && row?.total);
        info.summary.textContent = !snapshot ? '等待采样' : row?.total ?
          `${row.fresh} 正常 · ${row.missing} 缺采 · ${row.unavailable} 失联 · ${row.affected} 受影响` : '当前范围无实例';
        for (const bar of info.bars) {
          bar.style.width = row?.total ? ((row[bar.dataset.segment] || 0) / row.total * 100) + '%' : '0%';
        }
      }
    }

    function render() {
      const assets = snapshot?.assets || [];
      const selected = assets.find(asset => asset.id === selection.asset);
      configureCharts(selected);
      const options = snapshot?.filters || [];
      const sorted = entries => [...new Map(entries)].sort((a, b) => a[1].localeCompare(b[1], 'zh-CN'));
      const orgOptions = options.filter(item => !selection.organization || item.organizationId === selection.organization);
      const envOptions = orgOptions.filter(item => !selection.environment || item.environment === selection.environment);
      fillOptions('organization', [['', '全部组织'], ...sorted(options.map(item => [item.organizationId, item.organization]))]);
      fillOptions('environment', [['', '全部环境'], ...sorted(orgOptions.filter(item => item.environment).map(item => [item.environment, item.environment]))]);
      fillOptions('cluster', [['', '全部集群'], ...sorted(envOptions.filter(item => !selection.engine || item.engine === selection.engine)
        .map(item => [item.clusterKey, item.cluster]))]);
      renderEngines();
      get('window').value = selection.fixed ? 'fixed' : String(selection.window);
      get('layout').value = selection.layout;
      get('metric-group').value = selection.metrics;
      get('compare').value = selection.compare;
      for (const chart of charts) chart.card.hidden = selection.metrics !== 'all' && selection.metrics !== chart.card.dataset.group;
      renderSavedViews();
      get('dates').hidden = !selection.fixed && get('dates').dataset.editing !== 'true';
      if (!get('dates').hidden && selection.fixed && get('dates').dataset.editing !== 'true') fillDates(selection.fixed);
      get('pause').textContent = paused ? '继续' : '暂停';
      get('pause').setAttribute('aria-pressed', String(paused));
      renderKPIs();
      const pages = Math.max(1, Math.ceil((snapshot?.filteredTotal ?? snapshot?.total ?? 0) / pageSize));
      get('page').textContent = (selection.page + 1) + ' / ' + pages;
      get('prev').disabled = selection.page === 0;
      get('next').disabled = selection.page + 1 >= pages;
      renderAssets(assets);
      renderEvents(snapshot?.events || []);
      get('focus').textContent = selected ? selected.name : '';
      renderContext();
      renderLinks();
      renderBaseline();
      drawAll();
    }

    const kpis = [['范围内实例', 'total', 'database', ''], ['采样正常', 'fresh', 'database', null],
      ['端点失联', 'unavailable', 'incident', null], ['处理中事件', 'openEvents', 'incident', null],
      ['受影响实例', 'affectedCount', 'incident', 'affected'], ['已恢复 · 6 小时', 'recoveredCount', 'database', 'recovered'],
      ['陈旧或缺采', 'missingCount', 'missing', 'missing'], ['维护中实例', 'maintenanceCount', 'host', 'maintenance']];
    get('kpis').append(...kpis.map(([name, key, icon, state]) => {
      const card = node(state === null ? 'div' : 'button', 'wallboard-kpi');
      card.dataset.kpi = key;
      if (state !== null) {
        card.type = 'button';
        card.dataset.filter = state;
        card.addEventListener('click', () => {
          const next = selection.state === state ? '' : state;
          if (selection.state === next) return;
          get('event-list').value = next === 'recovered' ? 'resolved' : 'open';
          Object.assign(selection, { state: next, page: 0, asset: '', cursor: null, event: '' });
          saveRoute();
          refreshSelection();
        });
      }
      card.append(makeIcon(icon), node('span', '', name), node('strong'));
      return card;
    }));

    function renderKPIs() {
      for (const card of get('kpis').children) {
        card.lastChild.textContent = countLabel(snapshot?.[card.dataset.kpi]);
        if (card.tagName === 'BUTTON') card.setAttribute('aria-pressed', String(card.dataset.filter === selection.state));
      }
    }

    function nearestPoint(data = historyData, at = selection.cursor) {
      const points = data?.points || [];
      if (!finite(at)) return points.at(-1);
      const maximum = (data?.bucketMinutes || 1) * minute;
      let closest, distance = maximum + 1;
      for (const point of points) {
        const delta = Math.abs(Date.parse(point.at) - at);
        if (delta < distance) {
          closest = point;
          distance = delta;
        }
      }
      return closest;
    }

    function baselineShift() { return currentRange ? currentRange.from - Date.parse(baselineData?.from) : NaN; }

    function baselinePoint() {
      const at = selection.cursor ?? Date.parse(nearestPoint()?.at);
      const shift = baselineShift();
      return finite(at) && finite(shift) ? nearestPoint(baselineData, at - shift) : null;
    }

    function renderBaseline() {
      const panel = get('baseline');
      panel.hidden = !selection.compare;
      if (panel.hidden) return;
      const raw = get('baseline-raw'), eventLink = get('baseline-event');
      raw.hidden = eventLink.hidden = true;
      if (!baselineData) {
        get('baseline-status').textContent = baselineError ? '基线读取失败，保留当前窗口。' :
          selection.asset ? '正在读取基线窗口…' : '选择实例后可对比历史基线。';
        return;
      }
      const coverage = data => {
        if (!data) return '—';
        const minutes = data.bucketMinutes || 1;
        const buckets = Math.ceil((Date.parse(data.to) - Date.parse(data.from)) / (minutes * minute));
        const observed = data.points.filter(row => Object.keys(row.values).length).length;
        const changes = data.points.filter(row => row.boundary).length;
        return `${observed}/${buckets} 桶有效${changes ? ` · ${changes} 处重启、统计重置或身份变化` : ''}`;
      };
      const point = baselinePoint();
      const caption = [selection.change ? `变更时刻 ${formatTime(selection.change)}` : '',
        `虚线基线 ${formatTime(baselineData.from)} 至 ${formatTime(baselineData.to)}`,
        `${baselineData.bucketMinutes} 分钟一桶`, `当前 ${coverage(historyData)}`, `基线 ${coverage(baselineData)}`,
        point ? `对齐采样 ${formatTime(point.at)}` : '对齐时刻缺采'].filter(Boolean);
      get('baseline-status').textContent = caption.join(' · ');
      if (point?.sampleId) {
        raw.href = `${historyPath(selection.asset).replace('/wallboard-history', '/samples')}/${point.sampleId}`;
        raw.hidden = false;
        const at = Date.parse(point.at);
        const event = (baselineData.events || []).find(item => Date.parse(item.startedAt) <= at && (!item.endedAt || Date.parse(item.endedAt) >= at));
        if (event) {
          eventLink.textContent = '基线事件 · ' + event.title;
          eventLink.href = '#incidents?' + new URLSearchParams({ event: event.id, return: route(true) });
          eventLink.hidden = false;
        }
      }
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
      if (point?.boundary) context.append(node('span', '', point.boundary));
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
      if (historyData?.changes?.length) {
        const changes = node('select');
        changes.setAttribute('aria-label', '选择变更前后对比');
        changes.append(new Option('变更前后对比', ''), ...historyData.changes.map(item =>
          new Option(`${short(item.at)} · ${item.label}`, iso(Date.parse(item.at)))));
        changes.addEventListener('change', () => {
          const from = Date.parse(changes.value);
          if (!finite(from)) return;
          const to = Math.min(Date.now(), from + selection.window * minute);
          if (to - from < minute) { status('error', '变更后时段不足一分钟'); return; }
          Object.assign(selection, { fixed: {from, to}, change: iso(from), compare: 'previous', cursor: null, event: '' });
          saveRoute(); refreshSelection();
        });
        context.append(changes);
      }
      get('clear').hidden = selection.cursor == null;
    }

    function paint(chart, colors) {
      const { card, canvas, values, empty, start, end, source, scale, series, maximum } = chart;
      const points = historyData?.points || [];
      const point = nearestPoint();
      const prior = baselinePoint();
      const previousPoints = baselineData?.points || [];
      const entries = series.map(([key, label], i) => {
        const current = metric(key, point?.values[key]);
        const reference = metric(key, prior?.values[key]);
        const change = finite(current) && finite(reference) ? current - reference : null;
        const value = label + ' ' + countLabel(current) + (baselineData ? ` · 基线 ${countLabel(reference)}` +
          (change == null ? '' : ` · ${change > 0 ? '+' : ''}${countLabel(change)}`) : '');
        const entry = node('span', 'wallboard-series', value);
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
      for (const tick of scale ? [0.12, 0.51, 0.9] : [0.16, 0.5, 0.84]) {
        ctx.beginPath();
        ctx.moveTo(0, height * tick);
        ctx.lineTo(width, height * tick);
        ctx.stroke();
      }
      start.textContent = currentRange ? short(currentRange.from) : '';
      end.textContent = currentRange ? short(currentRange.to) : '';
      const hasData = points.some(row => series.some(([key]) => finite(row.values[key])));
      card.dataset.available = String(hasData);
      if (scale) scale.hidden = !hasData;
      empty.hidden = hasData;
      empty.textContent = !snapshot ? '等待读取' : !selection.asset ? '请选择实例' : !historyData ? '等待趋势数据'
        : series[0][0] === 'activeFile' && !historyData.containerObserved ? '未观测到数据库容器'
        : series[0][0] === 'sqlMs' ? '无连续且可比较的 SQL 统计'
        : series[0][0] === 'diskReadMs' ? '无连续设备采样或已完成 I/O' : '此时段缺采';
      if (!currentRange) return;
      const x = at => Math.max(0, Math.min(width, width * (at - currentRange.from) / (currentRange.to - currentRange.from)));
      const max = Math.max(maximum || 0, ...[...points, ...previousPoints]
        .flatMap(row => series.map(([key]) => metric(key, row.values[key]) ?? 0)), 0.01);
      if (scale && hasData) scale.replaceChildren(...[max, max / 2, 0].map(value => node('span', '', countLabel(value))));
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
      for (const point of points) if (point.boundary) marker(Date.parse(point.at), colors.change, [1, 4]);
      function drawSeries(rows, shift, width, dashed) {
        const gap = (width || 1) * 1.05 * minute;
        ctx.setLineDash(dashed ? [5, 3] : []);
        ctx.globalAlpha = dashed ? 0.58 : 1;
        for (const [index, [key]] of series.entries()) {
          let previous = null;
          ctx.strokeStyle = ctx.fillStyle = colors.series[index];
          ctx.lineWidth = 1.65;
          for (const row of rows) {
            const value = metric(key, row.values[key]);
            if (!finite(value)) { previous = null; continue; }
            const bucket = Date.parse(row.bucket);
            const y = height * (0.9 - 0.78 * value / max);
            const left = x(Date.parse(row.at) + shift);
            ctx.beginPath();
            if (previous && !row.boundary && bucket - previous.bucket <= gap) {
              ctx.moveTo(previous.left, previous.y);
              ctx.lineTo(left, y);
              ctx.stroke();
            } else {
              ctx.arc(left, y, dashed ? 1.3 : 1.75, 0, Math.PI * 2);
              ctx.fill();
            }
            previous = { left, y, bucket };
          }
        }
        ctx.setLineDash([]); ctx.globalAlpha = 1;
      }
      if (baselineData) drawSeries(previousPoints, baselineShift(), baselineData.bucketMinutes, true);
      drawSeries(points, 0, historyData?.bucketMinutes, false);
      ctx.lineWidth = 1;
      marker(selection.cursor, colors.cursor);
    }

    function drawAll() {
      const style = getComputedStyle(root);
      const variable = name => style.getPropertyValue('--wall-' + name).trim();
      const colors = { grid: variable('grid'), error: variable('error'), ok: variable('ok'), change: variable('change'),
        cursor: variable('text'), series: [variable('a'), variable('b')] };
      for (const chart of charts) paint(chart, colors);
      if (chartDialog.open) {
        const point = nearestPoint();
        const label = point ? (selection.cursor == null ? '最近样本 ' : '游标样本 ') + formatTime(point.at)
          : selection.cursor == null ? '' : '游标处缺采 ' + formatTime(iso(selection.cursor));
        get('chart-context').textContent = [get('focus').textContent, label].filter(Boolean).join(' · ');
        paint(largeChart, colors);
      }
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

    function moveCursor(event) {
      if (!currentRange || !['ArrowLeft', 'ArrowRight'].includes(event.key)) return;
      event.preventDefault();
      const step = (historyData?.bucketMinutes || 1) * minute;
      setCursor((selection.cursor ?? currentRange.to) + (event.key === 'ArrowLeft' ? -step : step));
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
      Object.assign(selection, { window: Number(value), fixed: null, cursor: null, event: '', change: '' });
      saveRoute();
      refreshSelection();
    });
    get('dates').addEventListener('submit', ev => {
      ev.preventDefault();
      const from = Date.parse(get('from').value), to = Date.parse(get('to').value);
      if (!(to > from && to - from <= 1440 * minute)) return status('error', '请选择有效时段，跨度最多 24 小时');
      delete get('dates').dataset.editing;
      Object.assign(selection, { fixed: { from, to }, cursor: null, event: '', change: '' });
      saveRoute();
      refreshSelection();
    });
    get('clear').addEventListener('click', () => setCursor(null));
    get('layout').addEventListener('change', () => { selection.layout = get('layout').value; saveRoute(); render(); });
    get('metric-group').addEventListener('change', () => { selection.metrics = get('metric-group').value; saveRoute(); render(); });
    get('compare').addEventListener('change', () => {
      selection.compare = get('compare').value;
      selection.change = '';
      saveRoute(); refreshSelection();
    });
    get('save-view').addEventListener('submit', async event => {
      event.preventDefault();
      const name = get('view-name').value.trim();
      if (!name) { get('view-status').textContent = '请输入视图名称。'; return; }
      const teamId = get('view-team').value;
      const generation = viewGeneration;
      const button = get('save-view-button');
      button.disabled = true;
      try {
        const result = await api(viewAPI, 'PUT', {name, path: savedPath(), teamId});
        if (generation !== viewGeneration) return;
        savedView = result.id;
        await fetchViews();
        if (generation === viewGeneration) get('view-status').textContent = `已保存 ${name}`;
      } catch (error) {
        if (generation === viewGeneration) get('view-status').textContent = error.message;
      } finally {
        if (generation === viewGeneration) button.disabled = !viewDirectory.canSave;
      }
    });
    get('saved-view').addEventListener('change', () => {
      savedView = get('saved-view').value;
      const view = viewDirectory.views.find(row => row.id === savedView);
      renderSavedViews();
      if (view) openSavedView(view);
    });
    get('delete-view').addEventListener('click', async () => {
      const selected = viewDirectory.views.find(view => view.id === savedView && view.canEdit);
      if (!selected) return;
      const generation = viewGeneration;
      get('delete-view').disabled = true;
      try {
        await api(viewAPI + '/' + selected.id, 'DELETE');
        if (generation !== viewGeneration) return;
        if (savedView === selected.id) { savedView = ''; get('view-name').value = ''; }
        await fetchViews();
        if (generation === viewGeneration) get('view-status').textContent = `已删除 ${selected.name}`;
      } catch (error) {
        if (generation === viewGeneration) { renderSavedViews(); get('view-status').textContent = error.message; }
      }
    });
    get('import-views').addEventListener('click', async () => {
      const generation = viewGeneration;
      const button = get('import-views');
      const names = new Set(viewDirectory.views.filter(view => !view.teamId).map(view => view.name));
      button.disabled = true;
      let count = 0;
      let failure = '';
      try {
        for (const view of browserViews()) {
          if (generation !== viewGeneration) return;
          if (names.has(view.name)) continue;
          await api(viewAPI, 'PUT', {...view, import: true});
          names.add(view.name);
          count++;
        }
      } catch (error) { failure = error.message; }
      if (generation !== viewGeneration) return;
      await fetchViews().catch(error => { failure ||= error.message; });
      if (generation !== viewGeneration) return;
      button.disabled = false;
      get('view-status').textContent = [`已导入 ${count} 个视图`, failure].filter(Boolean).join(' · ');
    });
    async function copyView(path) {
      const url = new URL(location.href); url.hash = path;
      try {
        await navigator.clipboard.writeText(url.href);
        get('view-status').textContent = '已复制视图链接';
      } catch {
        const field = get('view-url');
        field.value = url.href; field.hidden = false; field.focus(); field.select();
        get('view-status').textContent = '可复制下方的视图链接。';
      }
    }
    get('share-view').addEventListener('click', () => void copyView(savedPath()));
    get('share-team-view').addEventListener('click', () => {
      if (viewDirectory.views.some(view => view.id === savedView && view.teamId)) void copyView('wallboard?view=' + savedView);
    });
    get('event-list').addEventListener('change', () => renderEvents(snapshot?.events || []));
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
