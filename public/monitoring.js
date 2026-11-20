(() => {
  'use strict';

  const minute = 60_000;
  const periods = [
    ['1 小时', 60, 1], ['6 小时', 360, 5], ['24 小时', 1440, 15], ['7 天', 10080, 60]
  ];
  const tabs = [['metrics', '核心指标'], ['waits', '等待分析'], ['sql', 'SQL 性能'], ['locks', '锁与会话'], ['infra', '运行环境']];
  const waitColors = { NoWait: '#2ac9ad', IO: '#ad96f7', Lock: '#f59a61', LWLock: '#f1cf6a', Client: '#66a4ff', IPC: '#ec8ab4', Timeout: '#b3c6df' };
  const palette = ['#56c8df', '#a899ff', '#ffac86', '#90d38a', '#ec8ab4', '#6d9ef4', '#eed078', '#9bb0c9', '#71d2b1', '#bdb7ff', '#f698a6', '#91a9ec'];
  const number = (value, digits = 1) => new Intl.NumberFormat('zh-CN', { maximumFractionDigits: digits }).format(value);
  const bytes = value => {
    for (const [unit, size] of [['TiB', 1024 ** 4], ['GiB', 1024 ** 3], ['MiB', 1024 ** 2], ['KiB', 1024]]) {
      if (value >= size) return `${number(value / size, 2)} ${unit}`;
    }
    return `${number(value, 2)} B`;
  };
  const rate = value => `${number(value, 2)} 次/s`;
  const percent = value => `${number(value)}%`;
  const finite = value => typeof value === 'number' && Number.isFinite(value);
  const display = (value, formatter = number) => finite(value) ? formatter(value) : '—';
  const roleLabels = { primary: '主库', standby: '备库' };

  function el(tag, text, cls) {
    const item = document.createElement(tag);
    if (text != null) item.textContent = text;
    if (cls) item.className = cls;
    return item;
  }

  function button(label, action, cls = 'monitor-action') {
    const item = el('button', label, cls);
    item.type = 'button';
    item.addEventListener('click', action);
    return item;
  }

  function select(label, onChange) {
    const item = el('select');
    item.setAttribute('aria-label', label);
    item.addEventListener('change', () => onChange(item.value));
    return item;
  }

  function options(item, values, current) {
    item.replaceChildren(...values.map(([value, label]) => {
      const option = el('option', label);
      option.value = value;
      return option;
    }));
    item.value = current;
  }

  function labeled(label, input) {
    const item = el('label', label, 'monitor-filter');
    item.append(input);
    return item;
  }

  function table(labels, rows, name) {
    const item = el('table');
    item.setAttribute('aria-label', name);
    const head = item.createTHead().insertRow();
    for (const label of labels) {
      const th = el('th', label);
      th.scope = 'col';
      head.append(th);
    }
    const body = item.createTBody();
    for (const row of rows) {
      const tr = body.insertRow();
      for (const value of row) {
        const td = tr.insertCell();
        if (value instanceof Node) td.append(value);
        else td.textContent = value ?? '—';
      }
    }
    const viewport = el('div', '', 'monitor-table');
    viewport.append(item);
    return viewport;
  }

  function panel(title) {
    const section = el('section', null, 'monitor-panel');
    section.append(el('h3', title));
    return section;
  }

  function stats(values) {
    const wrapper = el('div', null, 'monitor-stats');
    for (const [label, value, tone] of values) {
      const card = el('div', null, 'monitor-stat');
      card.append(el('span', label), el('strong', value, tone || ''));
      wrapper.append(card);
    }
    return wrapper;
  }

  function svg(tag, attrs = {}) {
    const item = document.createElementNS('http://www.w3.org/2000/svg', tag);
    for (const [key, value] of Object.entries(attrs)) item.setAttribute(key, value);
    return item;
  }

  function graph(title, window, buckets, events, format, stacked = false) {
    const card = panel(title);
    const frame = el('div', null, 'monitor-plot');
    const width = 600, height = 154, left = 85, right = 14, top = 12, bottom = 126;
    const area = width - left - right;
    const chart = svg('svg', { viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none', 'aria-hidden': 'true' });
    const detail = el('p', null, 'monitor-cursor-value');
    const pointCount = buckets.length;
    const x = index => left + (index + .5) * area / pointCount;
    const maximum = Math.max(0, ...buckets.map(point => point?.values.reduce((sum, item) => sum + item.value, 0) ?? 0));
    const scale = maximum > 0 ? maximum : 1;
    const y = value => bottom - value * (bottom - top) / scale;
    const axis = el('div', null, 'monitor-axis');
    axis.setAttribute('aria-hidden', 'true');
    for (const ratio of [0, .5, 1]) {
      const heightAt = y(scale * ratio);
      chart.append(svg('line', { x1: left, x2: width - right, y1: heightAt, y2: heightAt, class: 'monitor-gridline' }));
      if (maximum > 0 || ratio === 0) {
        const label = el('span', format(scale * ratio), 'monitor-y-tick');
        label.style.left = `${left * 100 / width}%`;
        label.style.top = `${heightAt * 100 / height}%`;
        axis.append(label);
      }
    }
    for (const [at, side, offset] of [[window.from, 'left', left], [window.to, 'right', right]]) {
      const label = el('span', new Date(at).toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }), 'monitor-x-tick');
      label.style[side] = `${offset * 100 / width}%`;
      axis.append(label);
    }
    for (const event of events) {
      const started = Date.parse(event.startedAt);
      if (started < window.from || started > window.to) continue;
      const position = left + (started - window.first) / (window.bucketMs * pointCount) * area;
      const mark = svg('line', { x1: position, x2: position, y1: top, y2: bottom, class: 'monitor-event-marker' });
      const label = svg('title');
      label.textContent = event.title;
      mark.append(label); chart.append(mark);
    }
    let previous = null;
    buckets.forEach((point, index) => {
      if (!point) { previous = null; return; }
      const cx = x(index);
      if (stacked) {
        let base = 0;
        const bar = Math.max(1, area / pointCount * .82);
        for (const value of point.values) {
          const next = base + value.value;
          chart.append(svg('rect', { x: cx - bar / 2, y: y(next), width: bar, height: y(base) - y(next), fill: value.color }));
          base = next;
        }
        if (base === 0) chart.append(svg('circle', { cx, cy: bottom - 1, r: 1.6, fill: '#7192a9' }));
      } else {
        const value = point.values[0];
        const cy = y(value.value);
        if (previous) chart.append(svg('line', { x1: previous.x, x2: cx, y1: previous.y, y2: cy, stroke: value.color, class: 'monitor-trendline' }));
        chart.append(svg('circle', { cx, cy, r: 2.3, fill: value.color }));
        previous = { x: cx, y: cy };
      }
    });
    const cursor = svg('line', { y1: top, y2: bottom, class: 'monitor-crosshair' });
    chart.append(cursor);
    frame.tabIndex = 0;
    frame.setAttribute('role', 'group');
    frame.setAttribute('aria-label', `${title}，用左右方向键查看时间桶`);
    detail.setAttribute('aria-live', 'polite');
    let selected = buckets.findLastIndex(Boolean);
    const show = index => {
      selected = Math.min(Math.max(index, 0), pointCount - 1);
      const at = window.first + selected * window.bucketMs;
      const timestamp = new Date(Math.max(at, window.from)).toLocaleString('zh-CN');
      const point = buckets[selected];
      cursor.setAttribute('x1', x(selected));
      cursor.setAttribute('x2', x(selected));
      const values = point?.values.map(value => `${value.label} ${format(value.value)}`);
      const observed = point?.observed == null ? '' : ` · 有效采样 ${point.observed}/${point.samples}`;
      detail.textContent = `${timestamp} · ${point ? (values.join(' / ').trim() || format(0)) + observed : '该时间桶无可用采样'}`;
    };
    frame.addEventListener('pointermove', event => {
      const rect = chart.getBoundingClientRect();
      const logical = (event.clientX - rect.left) * width / rect.width;
      const index = Math.floor((logical - left) * pointCount / area);
      if (selected !== index) show(index);
    });
    frame.addEventListener('keydown', event => {
      if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
      event.preventDefault();
      show(selected + (event.key === 'ArrowLeft' ? -1 : 1));
    });
    if (selected >= 0) {
      show(selected);
      frame.append(chart, axis);
      card.append(frame, detail);
    } else card.append(el('p', '所选时段暂无此指标。', 'monitor-empty'));
    return card;
  }

  function windowFor(range, fixed) {
    const [, duration, bin] = periods.find(([name]) => name === range) || periods[0];
    const to = fixed?.to ?? Date.now();
    const from = fixed?.from ?? to - duration * minute;
    const minutes = fixed ? [1, 5, 15, 60].find(size => to - from <= size * minute * 380) : bin;
    const bucketMs = minutes * minute;
    const first = Math.floor(from / bucketMs) * bucketMs;
    const params = new URLSearchParams({ from: new Date(from).toISOString(), to: new Date(to).toISOString(), bucketMinutes: String(minutes) });
    return { from, to, first, bucketMs, params, count: Math.ceil((to - first) / bucketMs) };
  }

  function aligned(rows, window, map) {
    const byTime = new Map(rows.map(row => [Date.parse(row.bucket), row]));
    return Array.from({ length: window.count }, (_, i) => {
      const row = byTime.get(window.first + i * window.bucketMs);
      return row ? map(row) : null;
    });
  }

  function metricCharts(series, window, events, specs) {
    const grid = el('div', null, 'monitor-charts');
    for (const [key, label, formatter, color] of specs) {
      const points = aligned(series, window, row => finite(row[key]) ? { values: [{ label: '', value: row[key], color }] } : null);
      grid.append(graph(label, window, points, events, formatter));
    }
    return grid;
  }

  function waitKey(group) {
    return group.other ? 'other' : JSON.stringify([group.waitType, group.waitEvent, group.queryId]);
  }

  function waitName(group) {
    if (group.other) return '其他（已保留样本）';
    const title = group.waitType === 'NoWait' ? '无等待事件' : group.waitType;
    return [title, group.waitEvent, group.queryId === '' ? 'Query ID 未提供' : group.queryId].filter(Boolean).join(' / ');
  }

  function waitChart(rows, window, events, depth, selectGroup) {
    const catalog = new Map();
    let observed = 0;
    let samples = 0;
    for (const row of rows) {
      observed += row.observed;
      samples += row.samples;
      for (const group of row.groups) {
        const key = waitKey(group);
        if (!catalog.has(key)) catalog.set(key, { ...group, label: waitName(group), total: 0 });
        catalog.get(key).total += group.averageSessions * row.observed;
      }
    }
    const groupList = [...catalog.values()].sort((a, b) => b.total - a.total || a.label.localeCompare(b.label));
    groupList.forEach((group, index) => {
      group.color = group.other ? '#7c90a3' : depth === 'type' && waitColors[group.waitType] || palette[index % palette.length];
    });
    const byKey = new Map(groupList.map((group, index) => [waitKey(group), { ...group, index }]));
    const points = aligned(rows, window, row => row.observed ? {
      observed: row.observed, samples: row.samples,
      values: row.groups.map(group => ({ ...byKey.get(waitKey(group)), value: group.averageSessions })).sort((a, b) => a.index - b.index)
    } : null);
    const card = graph('平均活跃会话', window, points, events, value => number(value, 3), true);
    const summary = observed ? `有效等待快照 ${observed}/${samples} 次。空白时间桶无可用快照，零值表示采样时没有匹配的活跃会话。` :
      '此范围没有可用等待快照。升级后需要新采样，并为采集账号授予 pg_read_all_stats（也可用 pg_monitor）。';
    card.append(el('p', summary, 'monitor-note'));
    if (rows.some(row => row.truncated)) card.append(el('p', '部分采样达到分组上限，细分数值可能偏低。', 'monitor-warning'));
    const legend = el('div', null, 'monitor-legend');
    for (const group of groupList) {
      const enabled = !group.other && (depth === 'query' ? Boolean(group.queryId) : depth === 'type' || Boolean(group.waitEvent));
      const label = enabled ? button(group.label, () => selectGroup(group), 'monitor-legend-item') : el('span', group.label, 'monitor-legend-item');
      const mark = el('span', null, 'monitor-swatch');
      mark.style.backgroundColor = group.color;
      label.prepend(mark);
      legend.append(label);
    }
    if (groupList.length) card.append(legend);
    return card;
  }

  function clusterKey(asset) {
    if (asset.clusterName) return `pg:${asset.clusterName}`;
    return `instance:${asset.id}`;
  }

  function clusterLabel(asset) {
    return asset.clusterName || `${asset.name}（独立）`;
  }

  function renderInfra(snapshot, asset) {
    const info = panel('所选时段最近的环境快照');
    if (!snapshot) { info.append(el('p', '此范围没有采样。', 'monitor-empty')); return info; }
    info.append(el('p', new Date(snapshot.collectedAt).toLocaleString('zh-CN'), 'monitor-note'));
    if (asset.runtime === 'managed') {
      info.append(el('p', '托管实例仅可见数据库侧信号。', 'monitor-note'));
    } else if (asset.runtime === 'linux') {
      const linux = snapshot.linux;
      if (linux?.status !== 'ok') info.append(el('p', '无 Linux 资源快照，请检查 node_exporter 配置和采样能力。', 'monitor-empty'));
      else {
        info.append(stats([['CPU', display(linux.cpu?.usedPercent, percent)], ['内存', display(linux.memory?.usedPercent, percent)], ['1 分钟负载', display(linux.cpu?.load1)]]));
        if (linux.filesystems?.length) info.append(table(['挂载点', '已用', '容量'], linux.filesystems.map(fs =>
          [fs.mountpoint, display(fs.usedPercent, percent), display(fs.sizeBytes, bytes)]), 'Linux 文件系统'));
      }
    } else {
      const k8s = snapshot.k8s;
      if (k8s?.status !== 'ok') info.append(el('p', '无 K8S 快照，请检查 Pod 映射与只读权限。', 'monitor-empty'));
      else {
        info.append(stats([['Pod', k8s.pod?.name || '—'], ['状态', k8s.pod?.phase || '—'], ['Node', k8s.node?.name || k8s.pod?.node || '—']]));
        const usage = new Map((k8s.resourceMetrics?.containers || []).map(item => [item.name, item]));
        if (k8s.containers?.length) info.append(table(['容器', '重启', '最近终止原因', 'CPU', '内存'], k8s.containers.map(item =>
          [item.name, item.restartCount, item.terminatedReason, usage.get(item.name)?.cpu, usage.get(item.name)?.memory]), 'Pod 容器'));
        if (k8s.pvcs?.length) info.append(table(['PVC', '状态', '已用', '容量'], k8s.pvcs.map(pvc =>
          [pvc.name, pvc.phase, display(pvc.usage?.usedBytes, bytes), pvc.capacity]), 'PVC 状态'));
        if (k8s.events?.length) info.append(table(['时间', '原因', '信息'], k8s.events.slice(0, 12).map(item =>
          [item.at ? new Date(item.at).toLocaleString('zh-CN') : null, item.reason, item.message]), 'Kubernetes 事件'));
        if (!k8s.resourceMetrics) info.append(el('p', '无 metrics.k8s.io 资源数据，Pod、Node 和 PVC 状态仍可查看。', 'monitor-note'));
      }
    }
    return info;
  }

  function create(root, actions) {
    const { api, assetHealth, assetRuntimeLabel, formatTime, openEvent, openAssets, renderSqlPerformance } = actions;
    let assets = [], summaries = new Map(), ready = false, enabled = false, visible = false;
    let cluster = '', instance = '', range = '1 小时', fixed = null, mode = 'metrics';
    let timeWindow = null;
    let group = 'type', waitType = '', waitEvent = '', queryId = '', request = 0, fleetRequest = 0;
    const header = el('div', null, 'monitor-controls');
    const fleet = el('div');
    const details = el('section', null, 'monitor-instance');
    const tablist = el('div', null, 'monitor-tabs');
    tablist.setAttribute('aria-label', '实例监控专题');
    const body = el('div', null, 'monitor-content');
    const instanceTitle = el('h2', null, 'monitor-instance-title');
    const timeLabel = el('div', null, 'monitor-range-caption');
    const clusterSelect = select('PG 集群', value => { cluster = value; instance = ''; update(); });
    const instanceSelect = select('数据库实例', value => { instance = value; resetWaitFilters(); update(); });
    const rangeSelect = select('时间范围', value => { range = value; fixed = null; timeWindow = null; update(); });
    header.append(labeled('PG 集群', clusterSelect), labeled('实例', instanceSelect), labeled('时间范围', rangeSelect),
      button('刷新', () => { if (!fixed) timeWindow = null; refreshFleet(); update(); }));
    for (const [key, label] of tabs) tablist.append(button(label, () => { mode = key; if (key === 'sql') queryId = ''; update(); }));
    details.append(instanceTitle, timeLabel, tablist, body);
    root.append(header, fleet, details);

    function members() {
      return assets.filter(asset => !cluster || clusterKey(asset) === cluster);
    }

    function health(asset) {
      const status = assetHealth(asset);
      return el('span', status.label, `monitor-state monitor-state-${status.kind}`);
    }

    function renderFleet() {
      const items = members();
      const rows = items.map(asset => {
        const values = summaries.get(asset.id);
        const connection = finite(values?.connections) && values.maxConnections > 0 ? percent(values.connections * 100 / values.maxConnections) : '—';
        return [button(asset.name, () => { openInstance(asset.id); }, 'monitor-link'), clusterLabel(asset),
          roleLabels[asset.role] || '未知', health(asset), display(values?.replicationLagBytes, bytes), connection,
          display(values?.openEvents), assetRuntimeLabel(asset)];
      });
      const card = panel(cluster ? '集群成员' : '集群总览');
      const roles = items.filter(asset => ['ok', 'manual'].includes(assetHealth(asset).kind));
      const open = items.reduce((sum, asset) => sum + (summaries.get(asset.id)?.openEvents ?? 0), 0);
      const lag = items.map(asset => summaries.get(asset.id)?.replicationLagBytes).filter(finite);
      card.append(stats([['纳管实例', items.length], ['已观测主库 / 备库', `${roles.filter(a => a.role === 'primary').length} / ${roles.filter(a => a.role === 'standby').length}`],
        ['最大回放差距', lag.length ? bytes(Math.max(...lag)) : '—'], ['开放事件', items.every(asset => summaries.has(asset.id)) && items.length ? open : '—', open ? 'monitor-alert' : '']]));
      if (items.length) card.append(table(['实例', 'PG 集群', '最近角色', '采集状态', '回放差距', '连接占用', '事件', '环境'], rows, '集群成员'));
      else card.append(el('p', '暂无纳管实例。', 'monitor-empty'), button('纳管 PostgreSQL', openAssets));
      fleet.replaceChildren(card);
    }

    async function refreshFleet() {
      if (!visible || !enabled) return;
      const key = ++fleetRequest;
      try {
        const payload = await api('/api/monitoring/instances');
        if (key !== fleetRequest || !visible) return;
        summaries = new Map(payload.instances.map(item => [item.assetId, item]));
        renderFleet();
      } catch (error) {
        if (key === fleetRequest && visible) fleet.append(el('p', `实例摘要暂不可用 · ${error.message}`, 'monitor-warning'));
      }
    }

    function openInstance(id) {
      const asset = assets.find(item => item.id === id);
      if (!asset) return;
      if (instance !== id) resetWaitFilters();
      cluster = clusterKey(asset);
      instance = id;
      update();
    }

    function resetWaitFilters() { group = 'type'; waitType = ''; waitEvent = ''; queryId = ''; }

    function followWait(item) {
      if (group === 'type') { waitType = item.waitType; waitEvent = ''; group = 'event'; }
      else if (group === 'event') { waitType = item.waitType; waitEvent = item.waitEvent; group = 'query'; }
      else if (item.queryId) { queryId = item.queryId; mode = 'sql'; }
      update();
    }

    function eventList(events) {
      const card = panel('时段内的事件');
      if (!events.length) card.append(el('p', '此实例在当前范围内没有关联事件。', 'monitor-empty'));
      for (const event of events) {
        const item = button(`${event.status === 'open' ? '处理中' : '已恢复'} · ${event.title}`, () => openEvent(event.id), 'monitor-event');
        item.append(el('small', `${formatTime(event.startedAt)} · ${event.id.slice(0, 8)}`));
        card.append(item);
      }
      return card;
    }

    async function load(asset, time) {
      const key = ++request;
      const path = `/api/assets/${asset.id}/`;
      const query = new URLSearchParams(time.params);
      let section;
      if (mode === 'metrics' || (mode === 'infra' && asset.runtime === 'linux')) section = 'series';
      if (mode === 'waits') {
        section = 'wait-history';
        query.set('group', group);
        query.set('waitType', waitType);
        query.set('waitEvent', waitEvent);
      }
      if (mode === 'sql') { section = 'sql'; if (queryId) query.set('queryId', queryId); }
      body.setAttribute('aria-busy', 'true');
      body.replaceChildren(el('p', '正在读取所选时间范围…', 'monitor-note'));
      try {
        const [context, data] = await Promise.all([
          api(`${path}monitoring-context?${time.params}`), section ? api(`${path}${section}?${query}`) : null
        ]);
        if (key !== request || !visible) return;
        const events = context.events;
        const fragment = document.createDocumentFragment();
        if (mode === 'metrics') fragment.append(metricCharts(data.series, time, events, [
          ['transactionsPerSecond', '事务吞吐', rate, '#2ac9ad'], ['averageConnectionUsagePercent', '连接占用率', percent, '#72a5ff'],
          ['averageReplicationLagBytes', '复制回放差距', bytes, '#f59a61'], ['walBytesPerSecond', 'WAL 产生速率', value => `${bytes(value)}/s`, '#ad96f7'],
          ['averageBlockedSessions', '阻塞会话', number, '#f3bc77'], ['averageLongTransactions', '长事务', number, '#e68cb6'],
          ['averageDatabaseBytes', '数据库容量', bytes, '#7bc6e0'], ['averageConnections', '平均连接数', number, '#9dd19e']
        ]));
        if (mode === 'waits') {
          const filter = el('div', null, 'monitor-wait-controls');
          const dimension = select('等待分组', value => { group = value; waitType = ''; waitEvent = ''; update(); });
          options(dimension, [['type', '等待类型'], ['event', '具体等待事件'], ['query', 'Query ID']], group);
          filter.append(labeled('分组方式', dimension));
          if (waitType) filter.append(button(`清除筛选 · ${waitType}${waitEvent ? '/' + waitEvent : ''}`, () => { waitType = ''; waitEvent = ''; update(); }));
          fragment.append(filter, waitChart(data.buckets, time, events, group, followWait));
        }
        if (mode === 'sql') {
          const sql = panel(queryId ? `Query ID ${queryId}` : '区间 SQL 性能');
          if (queryId) sql.append(button('查看此范围内的 Top SQL', () => { queryId = ''; update(); }));
          if (data.performance.status === 'ok' && queryId && !data.performance.statements.length) {
            sql.append(el('p', '该 Query ID 在此范围的 SQL 性能快照里没有匹配指纹。', 'monitor-empty'));
          } else renderSqlPerformance(sql, data.performance);
          fragment.append(sql);
        }
        if (mode === 'locks') {
          const locks = panel('锁阻塞链');
          if (context.snapshot) locks.append(el('p', `区间最后一次采样 · ${formatTime(context.snapshot.collectedAt)}`, 'monitor-note'));
          const sample = context.snapshot?.locks;
          if (sample?.status !== 'ok') locks.append(el('p', '此范围最后一次采样没有锁链数据，可从事件核对故障时刻的证据。', 'monitor-empty'));
          else if (!sample.data.length) locks.append(el('p', '采样时没有观测到阻塞关系。', 'monitor-empty'));
          else locks.append(table(['阻塞 PID', '等待 PID', '等待事件', '等待事务开始时间'], sample.data.map(edge =>
            [edge.blockerPid, edge.waitingPid, [edge.waitType, edge.waitEvent].filter(Boolean).join(' / '), edge.waitingSince && formatTime(edge.waitingSince)]), '锁阻塞关系'));
          fragment.append(locks);
        }
        if (mode === 'infra') {
          if (data?.series) fragment.append(metricCharts(data.series, time, events, [
            ['averageLinuxCpuPercent', 'Linux CPU · node_exporter', percent, '#2ac9ad'],
            ['averageLinuxMemoryPercent', 'Linux 内存 · node_exporter', percent, '#72a5ff']
          ]));
          fragment.append(renderInfra(context.snapshot, asset));
        }
        fragment.append(eventList(events));
        body.replaceChildren(fragment);
      } catch (error) {
        if (key === request && visible) body.replaceChildren(el('p', `读取监控数据失败 · ${error.message}`, 'monitor-warning'));
      } finally {
        if (key === request) body.removeAttribute('aria-busy');
      }
    }

    function update() {
      if (!visible || !ready) return;
      header.hidden = !enabled;
      details.hidden = true;
      if (!enabled) { fleet.replaceChildren(el('p', '监控工作台使用 Go 版和 PostgreSQL 18 长期存储。', 'monitor-note')); return; }
      const clusters = new Map(assets.map(asset => [clusterKey(asset), clusterLabel(asset)]));
      if (!clusters.has(cluster)) cluster = '';
      const selected = members();
      const asset = selected.find(item => item.id === instance);
      if (!asset) instance = '';
      options(clusterSelect, [['', '全部集群'], ...[...clusters].sort((a, b) => a[1].localeCompare(b[1], 'zh-CN'))], cluster);
      options(instanceSelect, [['', '选择实例'], ...selected.map(item => [item.id, item.name])], instance);
      options(rangeSelect, [...periods.map(([name]) => [name, `最近 ${name}`]), ...(fixed ? [['event', '事件时段']] : [])], fixed ? 'event' : range);
      renderFleet();
      if (!asset) { request++; return; }
      details.hidden = false;
      const currentWindow = timeWindow ??= windowFor(range, fixed);
      instanceTitle.textContent = asset.name;
      timeLabel.textContent = `${asset.name} · ${asset.database} · ${formatTime(currentWindow.from)} 至 ${formatTime(currentWindow.to)}`;
      for (const [index, item] of [...tablist.children].entries()) item.setAttribute('aria-pressed', String(tabs[index][0] === mode));
      void load(asset, currentWindow);
    }

    return {
      sync(nextAssets, available) {
        assets = nextAssets;
        ready = true;
        enabled = available;
        if (visible) {
          if (![body, header].some(container => container.contains(document.activeElement))) {
            if (!fixed) timeWindow = null;
            update();
          }
          void refreshFleet();
        }
      },
      setVisible(value) {
        if (visible === value) return;
        visible = value;
        if (value) { update(); void refreshFleet(); }
        else { request++; fleetRequest++; }
      },
      openInstance,
      openIncident(event) {
        const start = Date.parse(event.startedAt);
        const to = Math.min(Date.now(), event.endedAt ? Date.parse(event.endedAt) + 15 * minute : Date.now());
        if (Number.isFinite(start) && Number.isFinite(to)) {
          fixed = { from: Math.min(to - minute, Math.max(to - 10080 * minute, start - 15 * minute)), to };
        }
        timeWindow = null;
        mode = 'waits'; resetWaitFilters();
        openInstance(event.assetId);
      }
    };
  }

  window.DbSphereMonitoring = { create };
})();
