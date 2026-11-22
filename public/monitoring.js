(() => {
  'use strict';

  const minute = 60_000;
  const dateTimeInputValue = value => new Date(value - new Date(value).getTimezoneOffset() * minute).toISOString().slice(0, 16);
  const periods = [
    ['15 分钟', 15, 1], ['1 小时', 60, 1], ['6 小时', 360, 5], ['24 小时', 1440, 15], ['7 天', 10080, 60], ['30 天', 43200, 60]
  ];
  const tabs = [['metrics', '实例概览'], ['waits', '等待分析'], ['sql', 'SQL 性能'], ['locks', '锁与会话'],
    ['replication', '复制与高可用'], ['catalog', '数据库与对象'], ['infra', '运行环境'], ['backup', '备份与恢复'],
    ['changes', '变更时间线']];
  const changeKinds = {configuration: '配置', image: '镜像', deployment: '发布', maintenance: '维护', route: '路由', topology: '拓扑'};
  const changeSources = {pg_settings: 'PostgreSQL pg_settings', kubernetes: 'Kubernetes 容器状态',
    routing: '入口探测与 Service 采样', topology: '实例拓扑采样', operator: '人工登记 · 同窗待核对'};
  const defaultPeriod = '1 小时';
  const sqlOrders = [['time', '总耗时'], ['calls', '调用频率'], ['latency', '平均耗时'], ['reads', '读取共享块'], ['temp', '写入临时块']];
  const milliseconds = value => `${number(value, 2)} ms`;
  const waitColors = { NoWait: 'var(--plot-teal)', IO: 'var(--plot-violet)', Lock: 'var(--plot-orange)', LWLock: 'var(--plot-gold)', Client: 'var(--plot-blue)', IPC: 'var(--plot-pink)', Timeout: 'var(--plot-gray)' };
  const palette = ['blue', 'teal', 'violet', 'orange', 'pink'].map(key => `var(--plot-${key})`);
  const number = (value, digits = 1) => new Intl.NumberFormat('zh-CN', { maximumFractionDigits: digits }).format(value);
  const bytes = value => {
    for (const [unit, size] of [['TiB', 1024 ** 4], ['GiB', 1024 ** 3], ['MiB', 1024 ** 2], ['KiB', 1024]]) {
      if (value >= size) return `${number(value / size, 2)} ${unit}`;
    }
    return `${number(value, 2)} B`;
  };
  const rate = value => `${number(value, 2)} 次/s`;
  const signedBytes = value => `${value < 0 ? '−' : value > 0 ? '+' : ''}${bytes(Math.abs(value))}`;
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

  function sampleLink(assetId, sampleId, label = '原始采样') {
    const link = el('a', label, 'monitor-link');
    link.href = `/api/assets/${assetId}/samples/${sampleId}`;
    link.target = '_blank';
    link.rel = 'noopener';
    return link;
  }

  function stats(values) {
    const wrapper = el('div', null, 'monitor-stats');
    for (const [label, value, tone, action] of values) {
      const card = action ? button(null, action, 'monitor-stat') : el('div', null, 'monitor-stat');
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

  function eventPoints(events, time) {
    return events.flatMap(event => [
      { event, at: Date.parse(event.startedAt), end: false },
      { event, at: Date.parse(event.endedAt), end: true }
    ].filter(point => point.at >= time.from && point.at <= time.to));
  }

  function graph(title, window, buckets, events, format, stacked = false) {
    const card = panel(title);
    const frame = el('div', null, 'monitor-plot');
    const width = 600, height = 154, left = window.compactPlot ? 150 : 92, right = 19, top = 13, bottom = 126;
    const area = width - left - right;
    const chart = svg('svg', { viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none', 'aria-hidden': 'true' });
    const detail = el('p', null, 'monitor-cursor-value');
    const pointCount = buckets.length;
    const x = index => left + (index + .5) * area / pointCount;
    const maximum = Math.max(0, ...buckets.map(point => point ? (stacked ?
      point.values.reduce((sum, item) => sum + item.value, 0) :
      Math.max(0, ...point.values.map(item => item.value))) : 0));
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
    const marks = [];
    for (const { event, at, end } of eventPoints(events, window)) {
      const position = left + (at - window.first) / (window.bucketMs * pointCount) * area;
      const mark = svg('line', { x1: position, x2: position, y1: top, y2: bottom, class: end ? 'monitor-recovery-marker' : 'monitor-event-marker' });
      const label = svg('title');
      label.textContent = `${event.title} · ${end ? '恢复' : '开始'}`;
      const marker = svg('g', { class: 'monitor-mark' });
      marker.append(label, mark, svg('circle', { cx: position, cy: top, r: 4, fill: end ? 'var(--ok-line)' : 'var(--issue-line)' }));
      marker.addEventListener('click', () => window.openEvent(event.id));
      marks.push(marker);
    }
    const previous = new Map();
    const linePaths = new Map();
    buckets.forEach((point, index) => {
      if (!point) { previous.clear(); return; }
      const cx = x(index);
      if (stacked) {
        let base = 0;
        const bar = Math.max(1, area / pointCount * .82);
        for (const value of point.values) {
          const next = base + value.value;
          chart.append(svg('rect', { x: cx - bar / 2, y: y(next), width: bar, height: y(base) - y(next), fill: value.color }));
          base = next;
        }
        if (base === 0) chart.append(svg('circle', { cx, cy: bottom - 1, r: 1.6, fill: 'var(--plot-gray)' }));
      } else {
        const next = new Map();
        for (const value of point.values) {
          const cy = y(value.value);
          const key = value.key || value.label;
          const last = previous.get(key);
          if (pointCount > 240) {
            if (!linePaths.has(key)) linePaths.set(key, { color: value.color, points: [] });
            linePaths.get(key).points.push(`${last ? 'L' : 'M'}${cx.toFixed(2)},${cy.toFixed(2)}`);
            if (!last) linePaths.get(key).points.push(`l0.01,0`);
          } else {
            if (last) chart.append(svg('line', { x1: last.x, x2: cx, y1: last.y, y2: cy, stroke: value.color, class: 'monitor-trendline' }));
            chart.append(svg('circle', { cx, cy, r: 2.3, fill: value.color }));
          }
          next.set(key, { x: cx, y: cy });
        }
        previous.clear();
        for (const [label, location] of next) previous.set(label, location);
      }
    });
    for (const path of linePaths.values()) chart.append(svg('path', { d: path.points.join(' '), fill: 'none', stroke: path.color, class: 'monitor-trendline' }));
    const cursor = svg('line', { y1: top, y2: bottom, class: 'monitor-crosshair' });
    const brush = svg('rect', { y: top, height: bottom - top, width: 0, class: 'monitor-brush' });
    chart.append(cursor, brush, ...marks);
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
    let start = null;
    const indexAt = event => {
      const rect = chart.getBoundingClientRect();
      const logical = (event.clientX - rect.left) * width / rect.width;
      return Math.min(pointCount - 1, Math.max(0, Math.floor((logical - left) * pointCount / area)));
    };
    const move = index => {
      for (const showCursor of window.cursors) showCursor(index);
    };
    frame.addEventListener('pointerdown', event => {
      if (event.button || event.pointerType === 'touch' || event.target.closest('.monitor-mark')) return;
      start = indexAt(event);
      frame.setPointerCapture(event.pointerId);
    });
    frame.addEventListener('pointermove', event => {
      const index = indexAt(event);
      if (selected !== index) move(index);
      if (start === null) return;
      brush.setAttribute('x', x(Math.min(start, index)) - area / pointCount / 2);
      brush.setAttribute('width', (Math.abs(start - index) + 1) * area / pointCount);
    });
    frame.addEventListener('pointerup', event => {
      if (start === null) return;
      const index = indexAt(event);
      if (index !== start) window.zoom(
        Math.max(window.from, window.first + Math.min(start, index) * window.bucketMs),
        Math.min(window.to, window.first + (Math.max(start, index) + 1) * window.bucketMs)
      );
      else move(index);
      start = null;
      brush.setAttribute('width', 0);
    });
    frame.addEventListener('pointercancel', () => { start = null; brush.setAttribute('width', 0); });
    frame.addEventListener('keydown', event => {
      if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
      event.preventDefault();
      move(selected + (event.key === 'ArrowLeft' ? -1 : 1));
    });
    if (selected >= 0 || marks.length) {
      window.cursors.add(show);
      show(selected >= 0 ? selected : 0);
      frame.append(chart, axis);
      card.append(frame, detail);
    } else card.append(el('p', '所选时段暂无此指标。', 'monitor-empty'));
    return card;
  }

  function windowFor(range, fixed) {
    const [, duration, bin] = periods.find(([name]) => name === range) || periods[1];
    const to = fixed?.to ?? Date.now();
    const from = fixed?.from ?? to - duration * minute;
    const minutes = fixed ? fixed.minutes || ([1, 5, 15].find(size => to - from <= size * minute * 380) || 60) : bin;
    const bucketMs = minutes * minute;
    const first = Math.floor(from / bucketMs) * bucketMs;
    const params = new URLSearchParams({ from: new Date(from).toISOString(), to: new Date(to).toISOString(), bucketMinutes: String(minutes) });
    return { from, to, first, bucketMs, params, count: Math.ceil((to - first) / bucketMs), cursors: new Set() };
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
    for (const [key, label, formatter, color, seriesKey] of specs) {
      const points = aligned(series, window, row => finite(row[key]) ?
        { values: [{ key: seriesKey ? row[seriesKey] : '', label: '', value: row[key], color }] } : null);
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
    for (const group of groupList) group.color = waitColors[group.waitType] || 'var(--plot-gray)';
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
    card.classList.add('monitor-focus-chart');
    const ranking = panel(depth === 'query' ? 'Query ID 排名' : '主要等待');
    const total = groupList.reduce((sum, group) => sum + group.total, 0);
    const ranked = groupList.slice(0, 24).map(group => {
      const enabled = !group.other && (depth === 'query' ? Boolean(group.queryId) : depth === 'type' || Boolean(group.waitEvent));
      const label = enabled ? button(group.label, () => selectGroup(group), 'monitor-link') : el('span', group.label);
      label.classList.add('monitor-rank-label');
      const mark = el('span', null, 'monitor-swatch');
      mark.style.backgroundColor = group.color;
      label.prepend(mark);
      return [label, number(group.total / observed, 3), total ? percent(group.total * 100 / total) : '—'];
    });
    if (ranked.length) ranking.append(table(['分组', '平均会话', '占比'], ranked, '等待排行'));
    else ranking.append(el('p', observed ? '有效快照内没有匹配的活跃会话。' : '没有可供排名的等待数据。', 'monitor-empty'));
    const split = el('div', null, 'monitor-split monitor-rank');
    split.append(card, ranking);
    return split;
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
    const caption = el('p', `${new Date(snapshot.collectedAt).toLocaleString('zh-CN')} · `, 'monitor-note');
    caption.append(sampleLink(asset.id, snapshot.id));
    info.append(caption);
    if (asset.runtime === 'managed') {
      info.append(el('p', '托管实例仅可见数据库侧信号。', 'monitor-note'));
    } else if (asset.runtime === 'linux') {
      const linux = snapshot.linux;
      if (linux?.status !== 'ok') info.append(el('p', linux?.reason || '无 Linux 资源快照。', 'monitor-empty'));
      else {
        info.append(el('p', `${linux.source}${linux.endpoint ? ' · ' + linux.endpoint : ''}`, 'monitor-note'));
        info.append(stats([['CPU', display(linux.cpu?.usedPercent, percent)], ['内存', display(linux.memory?.usedPercent, percent)], ['1 分钟负载', display(linux.cpu?.load1)]]));
        if (linux.filesystems?.length) info.append(table(['挂载点', '已用', '容量'], linux.filesystems.map(fs =>
          [fs.mountpoint, display(fs.usedPercent, percent), display(fs.sizeBytes, bytes)]), 'Linux 文件系统'));
      }
    } else {
      const k8s = snapshot.k8s;
      if (k8s?.status !== 'ok') info.append(el('p', '无 K8S 快照，请检查 Pod 映射与只读权限。', 'monitor-empty'));
      else {
        const ready = k8s.pod?.conditions?.find(item => item.type === 'Ready')?.status;
        info.append(stats([['Pod', k8s.pod?.name || '—'], ['就绪', ready === 'True' ? '就绪' : ready === 'False' ? '未就绪' : '未知'],
          ['Node', k8s.node?.name || k8s.pod?.node || '—']]));
        const owner = k8s.owner ? `${k8s.owner.kind}/${k8s.owner.name}` : k8s.podMapping?.workload;
        info.append(el('p', [k8s.cluster, k8s.namespace, owner, k8s.pod?.uid].filter(Boolean).join(' · '), 'monitor-note'));
        const usage = new Map((k8s.resourceMetrics?.containers || []).map(item => [item.name, item]));
        if (k8s.containers?.length) info.append(table(['容器', '重启', '最近终止原因', 'CPU', '内存'], k8s.containers.map(item =>
          [item.name, item.restartCount, item.terminatedReason, usage.get(item.name)?.cpu, usage.get(item.name)?.memory]), 'Pod 容器'));
        if (k8s.pvcs?.length) info.append(table(['PVC', '状态', '已用', '容量'], k8s.pvcs.map(pvc =>
          [pvc.name, pvc.phase, display(pvc.usage?.usedBytes, bytes), pvc.capacity]), 'PVC 状态'));
        if (k8s.events?.length) info.append(table(['时间', '原因', '信息'], k8s.events.slice(0, 12).map(item =>
          [item.at ? new Date(item.at).toLocaleString('zh-CN') : null, item.reason, item.message]), 'Kubernetes 事件'));
        if (k8s.resourceMetricsError || !k8s.resourceMetrics) info.append(el('p', `metrics.k8s.io · ${k8s.resourceMetricsError || '未取得资源指标'}`, 'monitor-note'));
        for (const [name, issue] of [['Node', k8s.nodeError], ['PVC', k8s.pvcError], ['Event', k8s.eventsError]])
          if (issue) info.append(el('p', `${name} · ${issue}`, 'monitor-note'));
      }
    }
    return info;
  }

  function create(root, actions) {
    const { api, assetHealth, assetRuntimeLabel, formatTime, openEvent, openAssets, openPlans, canCaptureWaits } = actions;
    let assets = [], summaries = new Map(), recent = [], ready = false, enabled = false, visible = false;
    let cluster = '', instance = '', database = '', period = defaultPeriod, fixed = null, mode = 'metrics', eventAnchor = '';
    let group = 'type', waitType = '', waitEvent = '', queryId = '', request = 0, fleetRequest = 0;
    let focusAt = null, focusSample = '';
    let changePick = '';
    let waitCapturePick = '', waitCaptureRunning = false;
    let sqlOrder = 'time', baseline = '', lockPick = '', lockAt = null, replicaAt = null;
    let compare = null, abnormalOnly = false, fleetTime = null, fleetAt = null, chooseCluster = true;
    let rangeEvents = [];
    let memberSeries = new Map();
    const header = el('div', null, 'monitor-controls');
    const fleet = el('div', null, 'monitor-fleet');
    const overview = el('div', null, 'cluster-overview');
    const charts = el('div', null, 'monitor-layout');
    fleet.append(overview, charts);
    const details = el('section', null, 'monitor-instance');
    const tablist = el('div', null, 'monitor-tabs');
    tablist.setAttribute('aria-label', '实例监控专题');
    const body = el('div', null, 'monitor-content');
    const instanceTitle = el('h2', null, 'monitor-instance-title');
    const timeLabel = el('div', null, 'monitor-range-caption');
    let activeWindow = null;
    let dbOid = '', objectOid = '', objectKind = '', deviceType = '', deviceName = '';
    const fleetLabel = el('p', null, 'monitor-range-caption');
    const clusterSelect = select('PG 集群', value => {
      cluster = value; instance = ''; database = ''; chooseCluster = false;
      replicaAt = null;
      resetCatalog();
      focusAt = null; focusSample = ''; waitCapturePick = '';
      if (cluster) {
        try { localStorage.setItem('dbsphere.lastCluster', cluster); } catch {}
      }
      resetFleet(); change();
    });
    const instanceSelect = select('数据库实例', value => { if (value) openInstance(value); else backToFleet(); });
    const databaseSelect = select('纳管连接库', value => { database = value; instance = ''; resetCatalog(); resetFleet(); change(); });
    const rangeSelect = select('时间范围', value => {
      if (value === 'custom') {
        openRange();
        return;
      }
      period = value;
      fixed = null;
      activeWindow = null;
      fleetAt = null;
      eventAnchor = '';
      focusAt = null; focusSample = '';
      rangeForm.hidden = true;
      change();
    });
    const rangeForm = el('form', null, 'monitor-fixed');
    const fromField = el('input'), toField = el('input');
    fromField.type = toField.type = 'datetime-local';
    fromField.required = toField.required = true;
    const apply = el('button', '应用范围', 'monitor-action');
    apply.type = 'submit';
    const errorLabel = el('span', '', 'monitor-warning');
    errorLabel.setAttribute('role', 'alert');
    rangeForm.append(labeled('开始时间', fromField), labeled('结束时间', toField), apply,
      button('取消', () => { rangeForm.hidden = true; updatePeriodOptions(); }), errorLabel);
    rangeForm.hidden = true;
    rangeForm.addEventListener('submit', event => {
      event.preventDefault();
      if (!validRange(Date.parse(fromField.value), Date.parse(toField.value))) {
        errorLabel.textContent = '请选择至少 1 分钟、最多 30 天且结束时间不晚于当前的范围。';
        return;
      }
      zoom(Date.parse(fromField.value), Date.parse(toField.value));
    });
    const editRange = button('编辑时段', openRange);
    const shareField = el('input', null, 'monitor-share-url');
    shareField.type = 'url'; shareField.readOnly = true; shareField.hidden = true;
    shareField.setAttribute('aria-label', '监控分享地址');
    const share = button('复制链接', async () => {
      const url = new URL(location.href);
      url.hash = route(activeWindow || fixed);
      try {
        await navigator.clipboard.writeText(url.href);
        share.textContent = '链接已复制';
        setTimeout(() => { share.textContent = '复制链接'; }, 1500);
      } catch {
        shareField.hidden = false; shareField.value = url.href; shareField.focus(); shareField.select();
      }
    });
    header.append(labeled('PG 集群', clusterSelect), labeled('实例', instanceSelect), labeled('纳管连接库', databaseSelect),
      labeled('时间范围', rangeSelect), editRange, button('刷新', () => {
        if (!fixed) activeWindow = null;
        update();
        void refreshFleet();
      }), share, shareField, rangeForm);
    for (const [key, label] of tabs) tablist.append(button(label, () => { mode = key; if (key === 'sql') queryId = ''; change(); }));
    details.append(button('← 集群总览', backToFleet, 'monitor-link monitor-breadcrumb'), instanceTitle, timeLabel, tablist, body);
    root.append(header, fleet, details);

    function validRange(from, to) {
      return Number.isFinite(from) && Number.isFinite(to) && to <= Date.now() && to - from >= minute && to - from <= 43200 * minute;
    }

    function route(window = fixed) {
      const params = new URLSearchParams();
      for (const [key, value, fallback] of [
        ['cluster', cluster || (!chooseCluster && ready ? 'all' : ''), ''], ['instance', instance, ''], ['database', database, ''],
        ['period', period, defaultPeriod], ['tab', mode, 'metrics'], ['group', group, 'type'],
        ['waitType', waitType, ''], ['waitEvent', waitEvent, ''], ['queryId', queryId, ''], ['event', eventAnchor, ''],
        ['sqlOrder', sqlOrder, 'time'], ['dbOid', dbOid, ''], ['objectOid', objectOid, ''], ['objectKind', objectKind, ''],
        ['at', focusAt == null ? '' : new Date(focusAt).toISOString(), ''], ['sample', focusSample, ''], ['waitCapture', waitCapturePick, ''],
        ['baseline', baseline, ''], ['lockSample', lockPick, ''], ['lockAt', lockAt == null ? '' : new Date(lockAt).toISOString(), ''],
        ['replicaAt', replicaAt == null ? '' : new Date(replicaAt).toISOString(), ''], ['change', mode === 'changes' ? changePick : '', ''],
        ['deviceType', deviceType, ''], ['device', deviceName, ''],
        ['compare', compare ? [...compare].join(',') || 'none' : '', ''],
        ['fleetAt', fleetAt == null ? '' : new Date(fleetAt).toISOString(), '']
      ]) if (value && value !== fallback) params.set(key, value);
      if (window) {
        params.set('from', new Date(window.from).toISOString());
        params.set('to', new Date(window.to).toISOString());
        const minutes = window.minutes || window.bucketMs / minute;
        if (minutes) params.set('bucket', String(minutes));
      }
      return params.size ? `monitoring?${params}` : 'monitoring';
    }

    function restore(params) {
      chooseCluster = !params.has('cluster');
      cluster = params.get('cluster') === 'all' ? '' : params.get('cluster') || '';
      instance = params.get('instance') || '';
      database = params.get('database') || '';
      period = periods.find(([name]) => params.get('period') === name)?.[0] || defaultPeriod;
      mode = tabs.find(([name]) => params.get('tab') === name)?.[0] || 'metrics';
      group = ['event', 'query'].includes(params.get('group')) ? params.get('group') : 'type';
      waitType = (params.get('waitType') || '').slice(0, 64);
      waitEvent = (params.get('waitEvent') || '').slice(0, 128);
      queryId = /^-?\d{1,20}$/.test(params.get('queryId') || '') ? params.get('queryId') : '';
      eventAnchor = /^[0-9a-f-]{36}$/.test(params.get('event') || '') ? params.get('event') : '';
      const evidenceTime = Date.parse(params.get('at'));
      changePick = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(params.get('change') || '') ? params.get('change') : '';
      focusAt = eventAnchor && Number.isFinite(evidenceTime) ? evidenceTime : null;
      focusSample = focusAt != null && /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(params.get('sample') || '') ? params.get('sample') : '';
      waitCapturePick = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(params.get('waitCapture') || '') ? params.get('waitCapture') : '';
      sqlOrder = sqlOrders.some(([key]) => key === params.get('sqlOrder')) ? params.get('sqlOrder') : 'time';
      baseline = ['previous', 'day'].includes(params.get('baseline')) ? params.get('baseline') : '';
      lockPick = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/.test(params.get('lockSample') || '') ? params.get('lockSample') : '';
      const lockTime = Date.parse(params.get('lockAt'));
      lockAt = Number.isFinite(lockTime) ? lockTime : null;
      const replicaTime = Date.parse(params.get('replicaAt'));
      replicaAt = Number.isFinite(replicaTime) ? replicaTime : null;
      deviceType = ['disk', 'network', 'filesystem'].includes(params.get('deviceType')) ? params.get('deviceType') : '';
      deviceName = deviceType ? (params.get('device') || '').slice(0, 128) : '';
      dbOid = /^\d{1,10}$/.test(params.get('dbOid') || '') ? params.get('dbOid') : '';
      objectOid = dbOid && /^\d{1,10}$/.test(params.get('objectOid') || '') ? params.get('objectOid') : '';
      objectKind = objectOid && ['table', 'index'].includes(params.get('objectKind')) ? params.get('objectKind') : '';
      if (!objectKind) objectOid = '';
      const from = Date.parse(params.get('from'));
      const to = Date.parse(params.get('to'));
      fixed = validRange(from, to) ? { from, to } : null;
      const minutes = Number(params.get('bucket'));
      if (fixed && [1, 5, 15, 60].includes(minutes) && (minutes === 60 || to - from <= minutes * minute * 400))
        fixed.minutes = minutes;
      activeWindow = null;
      rangeForm.hidden = true;
      resetFleet();
      const compared = params.get('compare');
      if (compared) compare = new Set(compared.split(',').slice(0, 5).filter(id => /^[0-9a-f-]{36}$/.test(id)));
      const cursor = Date.parse(params.get('fleetAt'));
      if (Number.isFinite(cursor)) fleetAt = cursor;
      if (visible) update();
    }

    function change() {
      if (visible && /^#monitoring(?:\?|$)/.test(location.hash) && location.hash !== `#${route()}`)
        history.pushState(null, '', `#${route()}`);
      update();
    }

    function updatePeriodOptions() {
      options(rangeSelect, [...periods.map(([name]) => [name, `最近 ${name}`]),
        ['custom', fixed ? '固定时间范围' : '自定义范围…']], fixed ? 'custom' : period);
      editRange.hidden = !fixed;
    }

    function openRange() {
      const range = windowFor(period, fixed);
      for (const [input, value] of [[fromField, range.from], [toField, range.to]]) {
        input.value = dateTimeInputValue(value);
      }
      errorLabel.textContent = '';
      rangeForm.hidden = false;
      fromField.focus();
    }

    function zoom(from, to) {
      if (!validRange(from, to)) return;
      fixed = { from, to };
      activeWindow = null;
      rangeForm.hidden = true;
      change();
    }

    function members() {
      return assets.filter(asset => (!cluster || clusterKey(asset) === cluster) && (!database || database === asset.database));
    }

    function resetFleet() { compare = null; abnormalOnly = false; fleetAt = null; rangeEvents = []; memberSeries = new Map(); }
    function resetCatalog() { dbOid = ''; objectOid = ''; objectKind = ''; }
    function resetWaitFilters() { group = 'type'; waitType = ''; waitEvent = ''; queryId = ''; lockPick = ''; lockAt = null; waitCapturePick = ''; }

    function openCluster(key) {
      if (!assets.some(asset => clusterKey(asset) === key)) return;
      cluster = key; instance = ''; database = ''; chooseCluster = false; mode = 'metrics';
      replicaAt = null;
      eventAnchor = ''; focusAt = null; focusSample = '';
      changePick = '';
      resetWaitFilters(); resetCatalog(); resetFleet();
      try { localStorage.setItem('dbsphere.lastCluster', cluster); } catch {}
      change();
    }

    function openInstance(id, topic) {
      const asset = assets.find(item => item.id === id);
      if (!asset) return;
      if (id !== instance) {
        deviceType = ''; deviceName = '';
        changePick = '';
        if (clusterKey(asset) !== cluster) { replicaAt = null; resetFleet(); }
        resetWaitFilters();
        resetCatalog();
        mode = 'metrics';
        eventAnchor = '';
        focusAt = null; focusSample = '';
      }
      if (topic) mode = topic;
      cluster = clusterKey(asset);
      if (database && database !== asset.database) database = '';
      instance = id;
      change();
    }

    function backToFleet() { instance = ''; eventAnchor = ''; focusAt = null; focusSample = ''; resetWaitFilters(); resetCatalog(); change(); }

    function health(asset) {
      const status = assetHealth(asset);
      return el('span', status.label, `monitor-state monitor-state-${status.kind}`);
    }

    function connection(asset) {
      const values = summaries.get(asset.id);
      return finite(values?.connections) && values.maxConnections > 0 ? values.connections * 100 / values.maxConnections : null;
    }

    function eventList(events, title, navigate = openEvent) {
      const card = panel(title);
      if (eventAnchor) card.append(button('返回事件证据', () => openEvent(eventAnchor)));
      if (!events.length) card.append(el('p', '当前范围内没有关联事件。', 'monitor-empty'));
      for (const event of events) {
        const item = button(event.title, () => navigate(event.id), 'monitor-event');
        item.append(el('small', [event.status === 'open' ? '处理中' : '已恢复', event.assetName,
          `开始 ${formatTime(event.startedAt)}`].filter(Boolean).join(' · ')));
        if (event.id === eventAnchor) item.setAttribute('aria-current', 'true');
        card.append(item);
      }
      return card;
    }

    function connectionSparkline(asset, time) {
      const values = aligned(memberSeries.get(asset.id) || [], time, row => row.averageConnectionUsagePercent).slice(-24);
      if (!values.some(finite)) return null;
      const plot = svg('svg', { viewBox: '0 0 46 12', class: 'member-spark', role: 'img', 'aria-label': '近况连接占用趋势' });
      const x = i => i * 46 / Math.max(1, values.length - 1);
      const y = value => 11 - Math.max(0, Math.min(100, value)) / 10;
      values.forEach((value, index) => {
        if (!finite(value)) return;
        if (index > 0 && finite(values[index - 1])) plot.append(svg('line', { x1: x(index - 1), y1: y(values[index - 1]), x2: x(index), y2: y(value) }));
        else plot.append(svg('circle', { cx: x(index), cy: y(value), r: 1 }));
      });
      return plot;
    }

    const healthRank = { error: 0, stale: 1, pending: 2, manual: 3, ok: 4 };
    const pgLabels = { available: '可用', unavailable: '不可用', unknown: '待确认' };

    function state(asset) {
      if (['stale', 'pending'].includes(assetHealth(asset).kind)) return 'unknown';
      return summaries.get(asset.id)?.postgresState || 'unknown';
    }

    function rankedMembers() {
      const openAssets = new Set(recent.filter(event => event.status === 'open').map(event => event.assetId));
      const openCount = asset => summaries.get(asset.id)?.openEvents ?? Number(openAssets.has(asset.id));
      return members().sort((a, b) => Number(state(b) === 'unavailable') - Number(state(a) === 'unavailable') ||
        openCount(b) - openCount(a) ||
        healthRank[assetHealth(a).kind] - healthRank[assetHealth(b).kind] ||
        Number(b.role === 'primary') - Number(a.role === 'primary') || a.name.localeCompare(b.name, 'zh-CN'));
    }

    function renderFleet(time) {
      if (!time || instance) return;
      const items = rankedMembers();
      const selected = items.filter(asset => state(asset) === 'available');
      const leaders = selected.filter(asset => asset.role === 'primary');
      const ids = new Set(items.map(asset => asset.id));
      const matching = [...new Map([...recent, ...rangeEvents].map(event => [event.id, event])).values()]
        .filter(event => ids.has(event.assetId)).sort((a, b) => Date.parse(b.startedAt) - Date.parse(a.startedAt));
      const active = matching.filter(event => event.status === 'open');
      const resolved = matching.filter(event => event.status !== 'open' &&
        Date.parse(event.startedAt) < time.to && Date.parse(event.endedAt) >= time.from);
      const issues = eventList([...active, ...resolved].slice(0, 3), '开始与恢复事件', time.openEvent);
      issues.classList.add('monitor-latest', 'cluster-issues');
      const card = panel('成员 · 最新观测');
      card.classList.add('monitor-members', 'cluster-members');
      const count = items.reduce((sum, asset) => sum + (summaries.get(asset.id)?.openEvents ?? 0), 0);
      if (count > active.length) issues.append(el('p', '更多开放事件可从问题事件页查看。', 'monitor-note'));
      const leaderName = leaders.length === 1 ? leaders[0].name : leaders.length > 1 ? `${leaders.length} 个主库信号` : '待确认';
      const identity = panel(cluster ? clusterLabel(items[0] ?? assets.find(asset => clusterKey(asset) === cluster)) : '全部集群');
      identity.classList.add('cluster-identity', 'monitor-identity');
      const versions = [...new Set(items.map(asset => Math.floor(asset.versionNum / 10000)).filter(value => value > 0))].sort((a, b) => a - b);
      const memberCount = identitiesLoaded(items) ? `${selected.length} / ${items.length}` : '—';
      const title = el('div', null, 'cluster-identity-title');
      title.append(identity.firstElementChild, el('p', versions.length ? `PostgreSQL ${versions.join(' / ')}` : 'PostgreSQL', 'monitor-note'));
      identity.append(title,
        stats([
          ['已观测可用', memberCount],
          ['观测主库', cluster ? leaderName : String(leaders.length), cluster && leaders.length > 1 ? 'monitor-alert' : '',
            cluster && leaders.length === 1 ? () => openInstance(leaders[0].id, 'replication') : null],
          ['开放事件', identitiesLoaded(items) ? count : '—', count ? 'monitor-alert' : '', () => issues.scrollIntoView({ block: 'nearest' })]
        ]));
      const row = el('div', null, 'cluster-grid');
      const abnormal = asset => state(asset) === 'unavailable' || !['ok', 'manual'].includes(assetHealth(asset).kind) || summaries.get(asset.id)?.openEvents > 0;
      const toolbar = el('div', null, 'cluster-tools');
      const toggle = button(abnormalOnly ? '显示全部' : '只看异常', () => {
        abnormalOnly = !abnormalOnly;
        renderFleet(time);
        overview.querySelector('.cluster-tools button')?.focus({ preventScroll: true });
      });
      toggle.setAttribute('aria-pressed', String(abnormalOnly));
      toolbar.append(toggle);
      if (items.length) card.append(toolbar);
      const rows = (abnormalOnly ? items.filter(abnormal) : items).map(asset => {
        const values = summaries.get(asset.id);
        const conn = display(connection(asset), percent);
        const replay = display(values?.replicationLagBytes, bytes);
        const pg = state(asset);
        const name = el('div', null, 'cluster-member-name');
        name.append(button(asset.name, () => openInstance(asset.id, abnormal(asset) ? 'waits' : 'metrics'), 'monitor-link'));
        const mobile = el('details', null, 'monitor-mobile-detail');
        mobile.append(el('summary', '采集详情'), el('small', [
          health(asset).textContent, formatTime(asset.lastSampleAt), `连接 ${conn}`, `回放 ${replay}`
        ].join(' · ')));
        name.append(mobile);
        const badge = el('span', pgLabels[pg], `cluster-state cluster-state-${pg}`);
        const connectionCell = el('div', null, 'member-connection');
        const spark = connectionSparkline(asset, time);
        if (finite(connection(asset))) connectionCell.append(button(conn, () => openInstance(asset.id), 'monitor-link'));
        else connectionCell.append(conn);
        if (spark) connectionCell.append(spark);
        const replayCell = finite(values?.replicationLagBytes) ? button(replay, () => openInstance(asset.id, 'replication'), 'monitor-link') : replay;
        return [name, roleLabels[asset.role] || '未知', badge, health(asset), connectionCell, replayCell];
      });
      if (rows.length) card.append(table(['实例', '角色', 'PG 状态', '采集状态', '连接', '回放'], rows, '集群成员'));
      else if (!items.length) card.append(el('p', '没有匹配的纳管实例。', 'monitor-empty'), button('查看资产', openAssets));
      else card.append(el('p', '当前没有异常成员。', 'monitor-empty'));
      row.append(identity, issues, card);
      overview.replaceChildren(row);
    }

    function identitiesLoaded(items) {
      return items.length > 0 && items.every(asset => summaries.has(asset.id));
    }

    async function refreshFleet() {
      if (!visible || !enabled || instance) return;
      const key = ++fleetRequest;
      try {
        const payload = await api('/api/monitoring/instances');
        if (key !== fleetRequest || !visible || instance) return;
        summaries = new Map(payload.instances.map(item => [item.assetId, item]));
        if (!overview.contains(document.activeElement)) renderFleet(fleetTime);
      } catch (error) {
        if (key === fleetRequest && visible && !instance) fleetLabel.textContent = `实例摘要暂不可用 · ${error.message}`;
      }
    }

    function fleetTimeline(time, selected, records, events, picker) {
      const card = panel('可用性与事件时间条');
      card.classList.add('cluster-timeline');
      const heading = el('div', null, 'cluster-heading');
      heading.append(card.firstElementChild, picker);
      card.append(heading, fleetLabel);
      const legend = el('div', null, 'cluster-legend');
      const states = { available: '可用', unavailable: '不可用', mixed: '时段内变化', unknown: '证据不足', gap: '无采样' };
      for (const [value, label] of Object.entries(states)) legend.append(el('span', label, `cluster-state cluster-state-${value}`));
      legend.append(el('span', '上排 PG · 下排采集'));
      const tracks = el('div', null, 'cluster-tracks');
      const footer = el('div', null, 'cluster-bucket');
      const observation = el('small');
      const evidence = el('span');
      footer.append(observation, evidence);
      let inspected = events[0]?.assetId || selected[0].id;
      for (const asset of selected) {
        const row = el('div', null, 'cluster-track-row');
        const track = el('div', null, 'cluster-track');
        const plot = svg('svg', { viewBox: `0 0 ${time.count} 16`, preserveAspectRatio: 'none', 'aria-hidden': 'true' });
        const bins = aligned(records.get(asset.id), time, bucket => bucket);
        const statesAt = bins.map(bucket => {
          if (!bucket) return 'gap';
          if (bucket.pgUnavailable && bucket.pgAvailable) return 'mixed';
          if (bucket.pgUnavailable) return 'unavailable';
          if (bucket.pgAvailable === bucket.samples) return 'available';
          return 'unknown';
        });
        const collection = bins.map(bucket => !bucket ? 'gap' : bucket.errors === bucket.samples ? 'unavailable' : bucket.errors ? 'mixed' : 'available');
        for (const [trackStates, y, height] of [[statesAt, 0, 8], [collection, 10, 6]]) {
          let start = 0;
          for (let end = 1; end <= trackStates.length; end++) {
            if (trackStates[end] === trackStates[start]) continue;
            plot.append(svg('rect', { x: start, y, width: end - start, height, class: `cluster-fill-${trackStates[start]}` }));
            start = end;
          }
        }
        const cursor = svg('line', { x1: 0, x2: 0, y1: 0, y2: 16, class: 'cluster-cursor' });
        plot.append(cursor);
        track.append(plot);
        const eventsForAsset = events.filter(event => event.assetId === asset.id);
        for (const { event, at, end } of eventPoints(eventsForAsset, time)) {
          const mark = button(end ? '恢复' : '故障', () => time.openEvent(event.id), `cluster-marker ${end ? 'recovered' : ''}`);
          mark.setAttribute('aria-label', `${event.title} · ${end ? '恢复' : '开始'} · ${formatTime(at)}`);
          mark.title = mark.getAttribute('aria-label');
          mark.style.left = `${(at - time.first) * 100 / (time.count * time.bucketMs)}%`;
          track.append(mark);
        }
        let index = 0;
        time.cursors.add(next => {
          index = next;
          cursor.setAttribute('x1', index + 0.5);
          cursor.setAttribute('x2', index + 0.5);
          if (inspected !== asset.id) return;
          const role = roleLabels[bins[index]?.observedRole];
          const observed = bins[index];
          const coverage = observed ? `${observed.samples - observed.errors}/${observed.samples} 次采集成功` : '采集缺失';
          observation.textContent = [asset.name, formatTime(Math.max(time.from, time.first + index * time.bucketMs)),
            `PG ${states[statesAt[index]]}`, coverage, role].filter(Boolean).join(' · ');
          evidence.replaceChildren();
          const id = observed?.blockedPeakSampleId && observed.peakBlockedSessions > 0
            ? observed.blockedPeakSampleId : observed?.latestSampleId;
          if (id) evidence.append(sampleLink(asset.id, id, '打开游标采样'));
        });
        track.tabIndex = 0;
        track.setAttribute('aria-label', `${asset.name} PostgreSQL 状态时间条`);
        const show = value => {
          inspected = asset.id;
          const bucket = Math.min(time.count - 1, Math.max(0, value));
          for (const displayCursor of time.cursors) displayCursor(bucket);
        };
        const inspectAt = event => {
          const rect = track.getBoundingClientRect();
          show(Math.floor((event.clientX - rect.left) * time.count / rect.width));
        };
        track.addEventListener('pointermove', inspectAt);
        track.addEventListener('click', event => { if (!event.target.closest('.cluster-marker')) inspectAt(event); });
        track.addEventListener('focus', () => show(index));
        track.addEventListener('keydown', event => {
          if (event.target !== track || !['ArrowLeft', 'ArrowRight'].includes(event.key)) return;
          event.preventDefault();
          show(index + (event.key === 'ArrowLeft' ? -1 : 1));
        });
        row.append(button(asset.name, () => openInstance(asset.id), 'monitor-link'), track);
        tracks.append(row);
      }
      observation.setAttribute('aria-live', 'polite');
      card.append(tracks, legend, footer);
      return card;
    }

    async function compareTrends(time) {
      const key = ++request;
      const items = rankedMembers();
      const primary = items.filter(asset => asset.role === 'primary' && !['stale', 'pending'].includes(assetHealth(asset).kind));
      const lead = cluster && primary.length === 1 ? primary[0] : null;
      compare ??= new Set([lead, ...items].filter(Boolean).map(asset => asset.id).filter((id, i, ids) => ids.indexOf(id) === i).slice(0, 3));
      const groupIds = new Set(items.map(asset => asset.id));
      compare = new Set([...compare].filter(id => groupIds.has(id)));
      const selected = items.filter(asset => compare.has(asset.id));
      const colors = new Map(selected.map((asset, i) => [asset.id, palette[i]]));
      if (!items.length) { charts.replaceChildren(); return; }
      const picker = el('details', null, 'monitor-panel monitor-topic cluster-picker');
      picker.append(el('summary', `比较成员 · ${selected.length} / 5`));
      const controls = el('div', null, 'monitor-selection');
      picker.append(controls);
      for (const asset of items) {
        const checkbox = el('input');
        checkbox.type = 'checkbox';
        checkbox.checked = compare.has(asset.id);
        checkbox.disabled = compare.size >= 5 && !checkbox.checked;
        checkbox.addEventListener('change', () => {
          if (checkbox.checked) compare.add(asset.id); else compare.delete(asset.id);
          history.replaceState(null, '', `#${route()}`);
          void compareTrends(time);
          const replacement = charts.querySelector('details');
          replacement.open = true;
          replacement.querySelectorAll('input')[items.indexOf(asset)]?.focus({ preventScroll: true });
        });
        const label = el('label');
        label.append(checkbox);
        if (checkbox.checked) {
          const swatch = el('span', null, 'monitor-swatch');
          swatch.style.backgroundColor = colors.get(asset.id);
          label.append(swatch);
        }
        label.append(asset.name);
        controls.append(label);
      }
      const seriesPanels = el('div', null, 'cluster-charts');
      const loading = el('p', selected.length ? '正在读取所选成员的时间序列…' : '选中成员以比较趋势。', 'monitor-note');
      charts.replaceChildren(picker, loading);
      if (!selected.length) return;
      const params = new URLSearchParams(time.params);
      for (const asset of selected) params.append('asset', asset.id);
      try {
        const { series, events } = await api(`/api/monitoring/series?${params}`);
        if (key !== request || !visible || instance) return;
        rangeEvents = events;
        time.cursors.clear();
        time.cursors.add(index => { fleetAt = Math.max(time.from, time.first + index * time.bucketMs); });
        const records = new Map(selected.map(asset => [asset.id, []]));
        for (const row of series) records.get(row.assetId)?.push(row);
        memberSeries = records;
        if (!overview.contains(document.activeElement)) renderFleet(time);
        const focused = picker.contains(document.activeElement) ? document.activeElement : null;
        const flow = fleetTimeline(time, selected, records, events, picker);
        const chartWindow = { ...time, compactPlot: true };
        for (const [metric, title, format, trendAssets] of [
          ['averageConnectionUsagePercent', '连接压力', percent, selected],
          ['averageBlockedSessions', '锁等待 · 阻塞会话', number, selected],
          ['walBytesPerSecond', 'WAL 产生速率', value => `${bytes(value)}/s`, selected],
          ['averageReplicationLagBytes', '复制回放差距', bytes, selected],
          ['transactionsPerSecond', lead && compare.has(lead.id) ? '当前主库 TPS' : '事务吞吐 · TPS', rate,
            lead && compare.has(lead.id) ? [lead] : selected]
        ]) {
          const points = Array.from({ length: time.count }, () => ({ values: [] }));
          for (const { id, name: label } of trendAssets) {
            for (const [i, point] of aligned(records.get(id), time, row => row[metric]).entries()) {
              if (finite(point)) points[i].values.push({ key: id, value: point, label, color: colors.get(id) });
            }
          }
          const trend = graph(title, chartWindow, points.map(point => point.values.length ? point : null), events, format);
          const latest = points.findLast(point => point.values.length);
          if (latest) {
            const top = latest.values.reduce((a, b) => a.value >= b.value ? a : b);
            const label = el('p', null, 'cluster-trend-number');
            label.append(el('strong', format(top.value)), el('span', top.label));
            trend.insertBefore(label, trend.children[1] ?? null);
          }
          seriesPanels.append(trend);
        }
        loading.replaceWith(flow, seriesPanels);
        focused?.focus({ preventScroll: true });
        if (time.bucketMs === 60 * minute) charts.append(hourlyDetails(time, selected, records));
        const at = fleetAt != null && fleetAt >= time.from && fleetAt < time.to ? fleetAt : null;
        const started = eventPoints(events, time).find(point => !point.end)?.at;
        const observed = series.reduce((last, row) => Math.max(last, Date.parse(row.bucket)), time.from);
        const index = Math.max(0, Math.min(time.count - 1, Math.floor(((at ?? started ?? observed) - time.first) / time.bucketMs)));
        for (const show of time.cursors) show(index);
      } catch (error) {
        if (key === request && visible && !instance) {
          loading.textContent = `趋势读取失败 · ${error.message}`;
          loading.className = 'monitor-warning';
        }
      }
    }

    function hourlyDetails(time, selected, records) {
      const panelNode = panel('小时覆盖与峰值');
      const label = el('p', '', 'monitor-note');
      const detail = el('div');
      let start = time.first;
      const inspect = button('下钻这一小时', () => {
        const end = Math.min(start + time.bucketMs, Date.now());
        zoom(Math.min(start, end - minute), end);
      }, 'ghost small-button');
      panelNode.append(label, detail, inspect);
      const rowsByTime = new Map(selected.map(({ id }) => [id, new Map((records.get(id) || []).map(row => [Date.parse(row.bucket), row]))]));
      let shown = -1;
      const draw = index => {
        if (shown === index) return;
        shown = index;
        start = time.first + index * time.bucketMs;
        label.textContent = `${formatTime(start)} · 图表游标联动`;
        detail.replaceChildren(table(['实例', 'PG 有效 / 采样', '峰值阻塞', '峰值连接', '峰值回放差距', '原始证据'], selected.map(asset => {
          const row = rowsByTime.get(asset.id)?.get(start);
          const id = row?.blockedPeakSampleId || row?.latestSampleId;
          return [asset.name, row?.samples ? `${row.pgAvailable} / ${row.samples}` : '缺采', display(row?.peakBlockedSessions),
            display(row?.peakConnectionUsagePercent, percent), display(row?.peakReplicationLagBytes, bytes),
            id ? sampleLink(asset.id, id, row.blockedPeakSampleId ? '阻塞峰值采样' : '末次采样') : '—'];
        }), '小时覆盖与峰值'));
      };
      time.cursors.add(draw);
      draw(time.count - 1);
      return panelNode;
    }

    async function loadLongRange(asset, time, key) {
      const query = new URLSearchParams(time.params);
      query.append('asset', asset.id);
      const { series, events } = await api(`/api/monitoring/series?${query}`);
      if (key !== request || !visible) return;
      time.cursors.clear();
      const charts = metricCharts(series, time, events, [
        ['transactionsPerSecond', '小时平均事务吞吐', rate, palette[0]],
        ['peakBlockedSessions', '小时峰值阻塞会话', number, palette[1]],
        ['peakConnectionUsagePercent', '小时峰值连接占用', percent, palette[2]],
        ['peakReplicationLagBytes', '小时峰值复制回放差距', bytes, palette[3]]
      ]);
      body.replaceChildren(charts, hourlyDetails(time, [asset], new Map([[asset.id, series]])), eventList(events, '此实例的问题事件'));
    }

    function openQuery(value) {
      queryId = value;
      mode = 'sql';
      change();
    }

    function blockerTree(edges) {
      const connections = new Map();
      const waiting = new Set(edges.map(edge => edge.waitingPid));
      for (const edge of edges) {
        if (!connections.has(edge.blockerPid)) connections.set(edge.blockerPid, []);
        connections.get(edge.blockerPid).push(edge);
      }
      const naturalRoots = [...connections.keys()].filter(pid => !waiting.has(pid));
      const visited = new Set();
      const tree = el('ul', null, 'lock-tree');
      tree.setAttribute('aria-label', '历史锁树');
      function walk(pid, chain, edge) {
        const row = el('li', null);
        row.append(el('strong', pid === 0 ? '阻塞方 · 预备事务' : `${edge ? '等待' : '阻塞方'} PID ${pid}`));
        const identifier = edge?.waitingQueryId;
        if (identifier) row.append(' · ', button(`Query ID ${identifier}`, () => openQuery(identifier), 'monitor-link'));
        if (chain.has(pid)) { row.append(el('small', ' · 循环关系')); return row; }
        if (visited.has(pid)) return row;
        visited.add(pid);
        const children = connections.get(pid) || [];
        if (children.length) {
          const branch = el('ul');
          for (const child of children) branch.append(walk(child.waitingPid, new Set([...chain, pid]), child));
          row.append(branch);
        }
        return row;
      }
      for (const pid of [...naturalRoots, ...connections.keys()]) if (!visited.has(pid)) tree.append(walk(pid, new Set()));
      return tree;
    }

    function waitCapturePanel(asset, captures, record, events) {
      const card = panel('短时等待采样');
      const path = `/api/assets/${asset.id}/wait-captures`;
      const controls = el('div', null, 'monitor-wait-controls');
      const choices = select('历史短时采样', id => { waitCapturePick = id; change(); });
      const history = captures.map(row => [row.id, `${formatTime(row.startedAt)} · ${row.intervalMs} ms · ${row.observed}/${row.expected} 帧`]);
      if (waitCapturePick && !captures.some(row => row.id === waitCapturePick)) history.unshift([waitCapturePick, '事件关联采样']);
      options(choices, [['', '选择历史采样'], ...history], waitCapturePick);
      controls.append(labeled('回放', choices));
      if (canCaptureWaits?.()) {
        const profile = select('采样档位', () => {});
        options(profile, [['standard', '标准 · 1 秒'], ['enhanced', '增强 · 250 毫秒']], 'standard');
        const status = el('p', '', 'monitor-note');
        status.setAttribute('aria-live', 'polite');
        const run = button(waitCaptureRunning ? '正在采样…' : '采样 10 秒', async () => {
          if (waitCaptureRunning) return;
          waitCaptureRunning = true; run.disabled = true; run.textContent = '正在采样…';
          const event = events.find(row => row.id === eventAnchor && row.status === 'open');
          status.textContent = event ? `保存至事件 ${event.id}` : '采样完成后可回放。';
          try {
            const response = await api(path, 'POST', { profile: profile.value, eventId: event?.id || '' });
            if (visible && instance === asset.id && ['waits', 'locks'].includes(mode)) {
              waitCapturePick = response.id;
              waitCaptureRunning = false;
              change();
            }
          } catch (error) {
            status.textContent = error.message;
          } finally {
            waitCaptureRunning = false; run.disabled = false; run.textContent = '采样 10 秒';
          }
        });
        run.disabled = waitCaptureRunning;
        controls.append(labeled('采样档位', profile), run);
        card.append(controls, status);
      } else card.append(controls);
      if (!record) {
        if (waitCapturePick) card.append(el('p', '该采样无法读取。', 'monitor-warning'));
        return card;
      }
      const { result } = record;
      card.append(stats([
        ['返回采样帧', `${result.observed} / ${result.expected}`], ['计划间隔', `${result.intervalMs} ms`],
        ['查询往返累计', `${result.queryTotalMs.toFixed(1)} ms`], ['最长往返', `${result.queryMaximumMs.toFixed(1)} ms`]
      ]));
      const sources = el('p', `${formatTime(result.startedAt)} · `, 'monitor-note');
      const raw = el('a', '原始短时采样', 'monitor-link');
      raw.href = `${path}?id=${encodeURIComponent(record.id)}`;
      sources.append(raw);
      if (record.eventId) sources.append(' · ', button('打开原事件', () => openEvent(record.eventId, true, route(activeWindow)), 'monitor-link'));
      card.append(sources);
      const snapshotBody = el('div');
      const snapshotSelect = select('短时采样帧', value => renderFrame(result.frames.find(frame => frame.slot === Number(value))));
      const first = result.frames.find(frame => frame.snapshot?.edges.length) || result.frames.find(frame => frame.snapshot) || result.frames[0];
      options(snapshotSelect, result.frames.map(frame => [String(frame.slot), `${formatTime(frame.at)} · 第 ${frame.slot + 1} 帧${frame.snapshot ? '' : ' · 无数据'}`]), String(first?.slot));
      card.append(labeled('采样帧', snapshotSelect), snapshotBody);
      renderFrame(first);
      return card;

      function renderFrame(frame) {
        snapshotBody.replaceChildren();
        const snapshot = frame?.snapshot;
        if (!snapshot || snapshot.visibility !== 'full') {
          snapshotBody.append(el('p', snapshot ? '目标权限或采集配置限制了会话可见性。' : '这帧缺少会话采样。', 'monitor-warning'));
          return;
        }
        snapshotBody.append(el('p', `实例启动 ${formatTime(snapshot.serverStartedAt)}。会话身份由实例启动时间、PID 和后端启动时间共同确定。连接池可复用同一后端。`, 'monitor-note'));
        if (snapshot.sessionsTruncated || snapshot.lockTruncated) snapshotBody.append(el('p', '这帧达到证据条数上限，仍可能存在其他会话或阻塞关系。', 'monitor-warning'));
        if (snapshot.edges.length) {
          snapshotBody.append(blockerTree(snapshot.edges), table(['阻塞 PID', '阻塞后端启动', '阻塞事务开始', '等待 PID', '等待后端启动', '等待事件'],
            snapshot.edges.map(edge => [edge.blockerPid || '预备事务', edge.blockerBackendStart && formatTime(edge.blockerBackendStart),
              edge.blockerXactStart && formatTime(edge.blockerXactStart), edge.waitingPid, edge.waitingBackendStart && formatTime(edge.waitingBackendStart), edge.waitEvent]), '短时阻塞关系'));
        } else snapshotBody.append(el('p', snapshot.lockObserved ? '这帧没有观测到阻塞关系。' : '这帧仅包含会话快照。锁树最多每秒读取一次。', 'monitor-note'));
        snapshotBody.append(table(['PID', '并行主进程', '后端启动', '事务开始', '当前查询开始', '等待事件', 'Query ID'], snapshot.sessions.map(row => [
          row.pid, row.leaderPid, row.backendStart && formatTime(row.backendStart), row.xactStart && formatTime(row.xactStart),
          row.queryStart && formatTime(row.queryStart), [row.waitType, row.waitEvent].filter(Boolean).join(' / '), row.queryId
        ]), '短时会话明细'));
      }
    }

    function lockPanel(asset, history, time) {
      const panelNode = panel('锁阻塞链');
      const available = history?.samples || [];
      const bucket = lockAt == null ? null : time.first + Math.floor((lockAt - time.first) / time.bucketMs) * time.bucketMs;
      const nearest = bucket == null ? null : available.filter(row => {
        const at = Date.parse(row.collectedAt);
        return row.locks.data.length && at >= bucket && at < bucket + time.bucketMs;
      })
        .sort((a, b) => Math.abs(Date.parse(a.collectedAt) - lockAt) - Math.abs(Date.parse(b.collectedAt) - lockAt))[0];
      const snapshot = available.find(row => row.id === (lockPick || focusSample)) || nearest ||
        available.find(row => row.locks.data.length) || available[0];
      if (available.length) {
        const choices = select('区间锁链采样', id => { lockPick = id; change(); });
        options(choices, available.map(row => [row.id, `${formatTime(row.collectedAt)} · ${row.locks.data.length} 条阻塞关系`]), snapshot?.id || '');
        const controls = el('div', null, 'monitor-wait-controls');
        controls.append(labeled('采样', choices));
        panelNode.append(controls);
      }
      if (lockAt != null && !nearest && !lockPick) panelNode.append(el('p', '等待峰值所在时间桶无锁链采样。下方保留区间内已有的锁链记录。', 'monitor-note'));
      if (snapshot) {
        const source = sampleLink(asset.id, snapshot.id);
        const label = snapshot === nearest ? '等待峰值同桶锁链快照' : '锁链采样';
        const caption = el('p', `${label} · ${formatTime(snapshot.collectedAt)} · `, 'monitor-note');
        caption.append(source);
        panelNode.append(caption);
        panelNode.append(button('回看采样时的等待趋势', () => {
          lockAt = Date.parse(snapshot.collectedAt); mode = 'waits'; change();
        }));
      }
      const sample = snapshot?.locks;
      if (sample?.status !== 'ok') panelNode.append(el('p', '所选时段无可用锁链采样。', 'monitor-empty'));
      else if (!sample.data.length) panelNode.append(el('p', '最后一次采样时没有观测到阻塞关系。', 'monitor-empty'));
      else {
        const queryLink = value => value ? button(value, () => openQuery(value), 'monitor-link') : '—';
        panelNode.append(blockerTree(sample.data));
        panelNode.append(table(['阻塞 PID', '阻塞 Query ID', '等待 PID', '等待 Query ID', '等待事件', '事务开始'],
          sample.data.map(edge => [edge.blockerPid, queryLink(edge.blockerQueryId), edge.waitingPid, queryLink(edge.waitingQueryId),
            [edge.waitType, edge.waitEvent].filter(Boolean).join(' / '), edge.waitingSince && formatTime(edge.waitingSince)]), '锁阻塞关系'));
      }
      return panelNode;
    }

    function rankSQL(performance, evidence, path, time) {
      const card = panel(queryId ? `Query ID ${queryId}` : '区间 SQL 排名');
      const controls = el('div', null, 'monitor-wait-controls');
      const order = select('SQL 排序', value => { sqlOrder = value; change(); });
      options(order, sqlOrders, sqlOrder);
      controls.append(labeled('排序', order));
      const comparison = select('SQL 基线窗口', value => { baseline = value; change(); });
      options(comparison, [['', '仅当前窗口'], ['previous', '前一等长窗口'], ['day', '昨日同时段（最多 24 小时）']], baseline);
      controls.append(labeled('基线', comparison));
      if (queryId) controls.append(button('返回 SQL 排名', () => { queryId = ''; change(); }));
      if (queryId && group === 'query') controls.append(button('返回等待排行', () => { mode = 'waits'; queryId = ''; change(); }));
      card.append(controls);
      const observed = performance.firstSampleAt && performance.lastSampleAt
        ? `${formatTime(performance.firstSampleAt)} 至 ${formatTime(performance.lastSampleAt)}` : '无采样';
      card.append(el('p', `来源 pg_stat_statements · ${observed} · ${performance.comparablePairs} 个可比区间`, 'monitor-note'));
      if (!performance.statements.length) {
        card.append(el('p', performance.reason || (queryId ? '该 Query ID 在所选时段没有可比较的执行增量。' :
          '此范围没有可比较的 SQL 执行增量。统计重置、样本缺失和未配置扩展会影响覆盖。'), 'monitor-empty'));
      }
      if (performance.truncated || performance.collectionLimited || performance.total > 50)
        card.append(el('p', '已达到采样或排行上限，排名只包含已覆盖的指纹和区间。', 'monitor-warning'));
      if (performance.statements.length) {
        card.append(table(['Query ID', '数据库 OID', '角色 OID', '调用/s', '总耗时', '平均耗时', '共享块读', '临时块写', '覆盖时长'],
          performance.statements.map(row => [
            queryId ? row.queryId : button(row.queryId, () => openQuery(row.queryId), 'monitor-link'),
            row.databaseOid, row.userOid, number(row.callsPerSecond, 2), milliseconds(row.totalExecMs), milliseconds(row.averageExecMs),
            number(row.sharedBlocksRead, 0), number(row.tempBlocksWritten, 0), `${number(row.coveredSeconds)} s`
          ]), 'SQL 区间排行'));
      }
      if (baseline) card.append(sqlComparison(performance, evidence.baseline, time));
      if (!queryId) return card;
      const trail = panel('同窗慢日志、执行计划与应用 Trace');
      for (const [kind, items] of [
        ['slow-logs', evidence?.entries || []],
        ['sql-plans', (evidence?.plans || []).filter(plan => Date.parse(plan.observedAt) >= time.from && Date.parse(plan.observedAt) < time.to)]
      ]) {
        for (const item of items.filter(row => !row.database || row.database === evidence.database).slice(0, 8)) {
          const link = el('a', kind === 'slow-logs' ? '慢日志证据' : '执行计划证据', 'monitor-link');
          link.href = `${path}${kind}/${item.id}`;
          link.target = '_blank';
          link.rel = 'noopener';
          const duration = item.durationMs ?? item.summary?.executionMs;
          const row = el('p', `${formatTime(item.observedAt)}${finite(duration) ? ' · ' + milliseconds(duration) : ''} · `, 'monitor-note');
          row.append(link);
          if (kind === 'sql-plans' && item.comparison) {
            const cmp = item.comparison;
            row.append(` · ${cmp.shapeChanged ? '计划形状变化' : '计划形状一致'}`);
            if (finite(cmp.executionRatio)) row.append(` · 执行耗时 ${number(cmp.executionRatio, 2)} 倍`);
            const previous = el('a', '上次计划', 'monitor-link');
            previous.href = `${path}sql-plans/${cmp.priorId}`; previous.target = '_blank'; previous.rel = 'noopener';
            row.append(' · ', previous);
          }
          for (const id of (item.eventIds || []).slice(0, 2)) row.append(' · ', button(`事件 ${id.slice(0, 8)}`, () => openEvent(id), 'monitor-link'));
          trail.append(row);
        }
      }
      for (const trace of (evidence.traces || []).slice(0, 8)) {
        const origin = el('a', `Trace ${trace.traceId.slice(0, 12)}`, 'monitor-link');
        origin.href = `${path}traces/${trace.traceId}`; origin.target = '_blank'; origin.rel = 'noopener';
        const row = el('p', `${formatTime(trace.firstAt)} · ${trace.services.join(' / ')} · ${trace.databaseSpans} 条 PG Span · `, 'monitor-note');
        row.append(origin); trail.append(row);
      }
      if (trail.children.length === 1) trail.append(el('p', '所选时段没有已纳管的匹配证据。', 'monitor-empty'));
      trail.append(button('查看实例的执行计划留存', () => openPlans(instance)));
      card.append(trail);
      return card;
    }

    function baselineWindow(time) {
      const duration = time.to - time.from;
      if (baseline === 'day' && duration > 1440 * minute) return null;
      const shift = baseline === 'day' ? 1440 * minute : duration;
      return { from: time.from - shift, to: time.to - shift };
    }

    function sqlComparison(current, prior, time) {
      const card = panel('基线与当前 SQL 对比');
      const range = baselineWindow(time);
      if (!range) {
        card.append(el('p', '昨日同时段支持不超过 24 小时的固定或滚动窗口。', 'monitor-note'));
        return card;
      }
      card.append(el('p', `基线 ${formatTime(range.from)} 至 ${formatTime(range.to)} · ${prior?.comparablePairs ?? 0} 个可比采样对`, 'monitor-note'));
      const before = new Map((prior?.statements || []).map(item => [item.fingerprint, item]));
      const rows = current.statements.map(row => {
        const old = before.get(row.fingerprint);
        const ratio = old?.averageExecMs > 0 ? row.averageExecMs / old.averageExecMs : null;
        return [button(row.queryId, () => openQuery(row.queryId), 'monitor-link'), row.databaseOid, row.userOid,
          display(old?.callsPerSecond, rate), display(row.callsPerSecond, rate), display(old?.averageExecMs, milliseconds),
          display(row.averageExecMs, milliseconds), display(ratio, value => `${number(value, 2)} 倍`),
          `${display(old?.coveredSeconds)} / ${display(row.coveredSeconds)} s`];
      });
      if (!prior) card.append(el('p', '未成功读取基线窗口。', 'monitor-note'));
      if (rows.length) card.append(table(['Query ID', '库 OID', '角色 OID', '基线调用', '当前调用', '基线均耗', '当前均耗', '变化', '可比时长'], rows, 'SQL 基线对比'));
      else card.append(el('p', '当前窗口缺少可对照的 SQL 执行数据。', 'monitor-empty'));
      return card;
    }

    function waitOverview(rows, time, events, depth, callback, context) {
      const split = waitChart(rows, time, events, depth, callback);
      const locked = context.blockingSample?.locks?.data?.length;
      if (locked) split.lastElementChild.append(button(`查看区间锁链 · ${locked} 条关系`, () => { mode = 'locks'; change(); }));
      const peaks = rows.map(row => ({ at: Date.parse(row.bucket), load: row.groups.reduce((sum, group) => sum + group.averageSessions, 0) }));
      const peak = peaks.reduce((highest, row) => row.load > highest.load ? row : highest, { load: 0 });
      if (peak.load > 0) split.lastElementChild.append(button(`查峰值锁链 · ${formatTime(peak.at)}`, () => {
        lockPick = ''; lockAt = peak.at; mode = 'locks'; change();
      }));
      return split;
    }

    function replicationPanel(asset, history, rows, time, events, topology) {
      const result = document.createDocumentFragment();
      const charts = metricCharts(rows, time, events, [
        ['averageReplicationLagBytes', '发送端最大回放差距', bytes, palette[2]],
        ['walBytesPerSecond', 'WAL 产生速率', value => `${bytes(value)}/s`, palette[0]],
        ['xidUsedPercent', 'XID 年龄 / 冻结阈值', percent, palette[1]]
      ]);
      const observation = panel('复制链路与 WAL 位置');
      const view = el('div');
      const latest = history.observations.find(row => row.kind === 'latest');
      const peak = history.observations.find(row => row.kind === 'peak');
      const slotPeak = history.observations.find(row => row.kind === 'slot_peak');
      const incidentSample = history.observations.find(row => row.kind === 'incident' && row.logicalSlotPressure?.data?.name);
      if (latest && peak) {
        const choices = select('复制采样', value => describe(value === 'peak' ? peak : latest));
        options(choices, [['latest', '区间末次采样'], ['peak', '区间回放差距峰值']], 'latest');
        observation.append(labeled('采样', choices));
      }
      observation.append(view);

      function describe(sample) {
        view.replaceChildren();
        if (!sample) {
          view.append(el('p', '所选时段没有采样。', 'monitor-empty'));
          return;
        }
        const heading = el('p', `${formatTime(sample.collectedAt)} · ${roleLabels[sample.role] || 'PG 角色未知'} · `, 'monitor-note');
        heading.append(sampleLink(asset.id, sample.id));
        view.append(heading);
        const position = sample.position?.data;
        if (position) view.append(stats([
          ['WAL 位置', position.position || '—'], ['时间线', position.timeline || '—'],
          ['服务启动', formatTime(position.startedAt)]
        ]));
        const links = sample.links?.data;
        if (links?.links.length) {
          view.append(table(['发送会话', '客户端', '状态 / 同步', '发送 LSN', '写入 LSN', '刷盘 LSN', '回放 LSN',
            '回放差距', '写入 / 刷盘 / 回放耗时'], links.links.map(link => [
            `${link.application || '—'} · PID ${link.pid}`, link.clientAddress,
            [link.state, link.syncState].filter(Boolean).join(' / '), link.sentLsn, link.writeLsn, link.flushLsn,
            link.replayLsn, display(link.replayGapBytes, bytes), [link.writeLagSeconds, link.flushLagSeconds, link.replayLagSeconds]
              .map(value => display(value, seconds => `${number(seconds, 3)} s`)).join(' / ')
          ]), 'WAL 发送会话'));
          if (links.total > links.links.length) view.append(el('p', `显示 ${links.links.length} / ${links.total} 条发送会话。`, 'monitor-note'));
          view.append(el('p', '来源 pg_stat_replication。耗时对应近期 WAL 确认，空闲或不可见时留空。', 'monitor-note'));
        } else if (sample.role === 'standby') {
          const receiver = sample.receiver?.data;
          view.append(stats([
            ['WAL 接收', receiver?.status || sample.receiverState || '—'], ['本地回放差距', display(position?.localReplayGapBytes, bytes)],
            ['回放暂停', typeof position?.replayPaused === 'boolean' ? (position.replayPaused ? '是' : '否') : '—']
          ]));
          if (receiver) view.append(table(['上游', '复制槽', '收到的 LSN', '刷盘的 LSN', '上游 LSN', '收到消息'], [[
            receiver.senderHost && `${receiver.senderHost}:${receiver.senderPort}`, receiver.slot, receiver.writtenLsn,
            receiver.flushedLsn, receiver.senderLsn, formatTime(receiver.lastMessageAt)
          ]], 'WAL 接收会话'));
        } else view.append(el('p', links ? '本次未观测到 WAL 发送会话。' : '缺少可见的 WAL 发送采样。', 'monitor-empty'));
        const storage = panel('WAL 归档与冻结');
        const archive = sample.archive?.data;
        const freeze = sample.freeze?.data;
        storage.append(stats([
          ['累计成功归档', display(archive?.archivedCount)], ['累计归档失败', display(archive?.failedCount)],
          ['最近归档失败', formatTime(archive?.lastFailedAt)], ['当前连接库 XID 年龄', display(freeze?.xidAge)]
        ]));
        view.append(storage, servicesPanel(asset, sample));
      }
      describe(latest);

      const changes = panel('角色与时间线变化');
      if (history.changes.length) changes.append(table(['首次观测时间', '角色变化', '时间线变化', '边界', '采样'], history.changes.map(row => [
        formatTime(row.collectedAt), `${roleLabels[row.priorRole] || '未知'} → ${roleLabels[row.role] || '未知'}`,
        row.timeline !== row.priorTimeline ? `${row.priorTimeline || '—'} → ${row.timeline || '—'}` : row.timeline,
        [row.restarted && '重启', row.walReset && 'WAL 统计重置', row.archiveReset && '归档统计重置',
          row.gapSeconds > 600 && '采样间隔超过 10 分钟'].filter(Boolean).join(' · ') || '—', sampleLink(asset.id, row.id)
      ]), '复制角色变化'));
      else changes.append(el('p', '所选时段未观测到角色、时间线或采集边界变化。', 'monitor-note'));
      result.append(topologyPanel(topology, time));
      if (asset.patroniConfigured || history.topology.some(row => row.patroni?.status === 'ok'))
        result.append(patroniSwitchPanel(asset, history.topology, events));
      result.append(logicalReplicationPanel(asset, latest, slotPeak, incidentSample, events));
      result.append(observation, charts, changes);
      return result;
    }

    function logicalReplicationPanel(asset, latest, peak, incident, events) {
      const card = panel('逻辑复制与复制槽');
      if (!latest) {
        card.append(el('p', '所选时段没有逻辑复制采样。', 'monitor-empty'));
        return card;
      }
      const view = el('div');
      const event = events.find(item => item.id === eventAnchor);
      const withinEvent = peak && event && Date.parse(peak.collectedAt) >= Date.parse(event.startedAt) &&
        (!event.endedAt || Date.parse(peak.collectedAt) <= Date.parse(event.endedAt));
      const focus = incident ? 'incident' : withinEvent ? 'peak' : 'latest';
      const frames = {latest, peak, incident};
      if (peak || incident) {
        const choice = select('逻辑复制采样', value => render(frames[value]));
        options(choice, [['latest', '区间末次采样'], ['peak', '区间逻辑槽保留峰值'], ['incident', '原事件采样']]
          .filter(([value]) => frames[value]), focus);
        card.append(labeled('采样', choice));
      }
      card.append(view);
      render(frames[focus]);
      return card;

      function render(sample) {
        const collected = el('p', `${formatTime(sample.collectedAt)} · `, 'monitor-note');
        collected.append(sampleLink(asset.id, sample.id));
        view.replaceChildren(collected);
        const note = message => view.append(el('p', message, 'monitor-note'));
        const unavailable = section => section?.reason === 'SQLSTATE 42501' ? '监控账号权限不足' : '当前采样不可用';
        const amount = value => value == null ? '—' : display(Number(value), bytes);
        const pressure = sample.logicalSlotPressure?.data;
        if (pressure?.name) note(`纳管数据库保留量最高的逻辑槽 ${pressure.name} · ${amount(pressure.retainedBytes)} · ${pressure.active ? '活动' : '空闲'}`);
        const slots = sample.slots?.data;
        if (slots?.length) {
          view.append(table(['复制槽', '类型 / 插件', '归属数据库', '活动', 'WAL 状态', '保留 WAL', '未确认 WAL', '剩余预算', '回放确认耗时', '重启 / 已确认 LSN'],
            slots.map(slot => [slot.name, [slot.type, slot.plugin].filter(Boolean).join(' / '), slot.database, slot.active ? '是' : '否',
              [slot.walStatus, slot.invalidationReason].filter(Boolean).join(' / '), amount(slot.retainedBytes), amount(slot.unconfirmedBytes),
              amount(slot.safeWalBytes), display(slot.replayLagSeconds, value => `${number(value, 3)} s`),
              [slot.restartLsn, slot.confirmedFlushLsn].filter(Boolean).join(' / ')]), '复制槽保留量'));
          if (sample.slotSummary?.data?.total > slots.length) note(`显示 ${slots.length} / ${sample.slotSummary.data.total} 个复制槽。`);
        } else note(sample.slots?.status === 'ok' ? '本次未观测到复制槽。' : `复制槽 · ${unavailable(sample.slots)}`);
        const pub = sample.logicalPublications;
        if (pub?.data?.entries.length) {
          const operations = {insert: '插入', update: '更新', delete: '删除', truncate: '清空'};
          view.append(table(['Publication', '表范围', '发布操作', '分区根发布'], pub.data.entries.map(item => [
            item.name, item.allTables ? '全部表' : `${item.explicitTables} 条显式表绑定`,
            Object.keys(operations).filter(key => item[key]).map(key => operations[key]).join(' / '), item.viaRoot ? '是' : '否'
          ]), '逻辑复制 Publication'));
          if (pub.data.total > pub.data.entries.length) note(`显示 ${pub.data.entries.length} / ${pub.data.total} 个 publication。`);
        } else note(pub?.status === 'ok' ? '当前连接库无 publication。' : `Publication · ${unavailable(pub)}`);
        const subs = sample.logicalSubscriptions;
        if (subs?.data?.entries.length) {
          const states = {running: '应用进程运行', disabled: '订阅已停用', worker_absent: '已启用，应用进程未运行', worker_unknown: '已启用，进程可见性受限'};
          const applied = new Map((sample.logicalApplyProgress?.data || []).map(item => [item.oid, item]));
          const errors = new Map((sample.logicalSubscriptionErrors?.data || []).map(item => [item.oid, item]));
          const errorFields = sample.postgresMajor >= 19 ? ['applyErrors', 'syncErrors', 'sequenceSyncErrors'] : ['applyErrors', 'syncErrors'];
          view.append(table(['Subscription', '状态', '上游复制槽', '订阅的 Publication', '表同步', '最近收信', '收到 / 回报 LSN', '已应用的上游 LSN',
            sample.postgresMajor >= 19 ? '应用 / 表同步 / 序列同步错误' : '应用 / 表同步错误'],
            subs.data.entries.map(item => [item.name, states[item.state] || item.state, item.slot,
              [...item.publications, ...(item.publicationCount > item.publications.length ? ['…'] : [])].join(' / '),
              `${item.readyTableCount} / ${item.tableCount} 就绪`, formatTime(item.lastMessageAt),
              [item.receivedLsn, item.reportedLsn].filter(Boolean).join(' / '), applied.get(item.oid)?.appliedOriginLsn,
              errors.has(item.oid) ? errorFields.map(key => display(errors.get(item.oid)[key])).join(' / ') : '—'
            ]), '逻辑复制 Subscription'));
          if (subs.data.total > subs.data.entries.length) note(`显示 ${subs.data.entries.length} / ${subs.data.total} 个 subscription。`);
          if (sample.logicalApplyProgress?.status !== 'ok') note(`上游应用位置 · ${unavailable(sample.logicalApplyProgress)}`);
        } else note(subs?.status === 'ok' ? '当前连接库无 subscription。' : `Subscription · ${unavailable(subs)}`);
        if (sample.postgresMajor === 14) note('PostgreSQL 14 提供订阅进程与位置信息，应用和同步错误计数从 PostgreSQL 15 起提供。');
        else if (subs?.data?.entries.length && sample.logicalSubscriptionErrors?.status !== 'ok')
          note(`应用与同步错误 · ${unavailable(sample.logicalSubscriptionErrors)}`);
        if (slots?.length || subs?.data?.entries.length) note('保留 WAL 以槽重启位置计算，未确认 WAL 以消费者确认位置计算。回放确认耗时来自发送进程近期反馈。');
      }
    }

    function patroniSwitchPanel(asset, points, events) {
      const card = panel('Patroni 切换前后');
      const observed = [...new Map(points.map(row => [row.id, row])).values()].sort((a, b) => Date.parse(a.collectedAt) - Date.parse(b.collectedAt));
      const leader = row => row.patroni?.leaderCount === 1
        ? row.patroni.members.find(member => ['leader', 'primary', 'standby_leader'].includes(member.role)) : null;
      const segments = [];
      for (const row of observed) {
        const member = leader(row);
        if (!member) continue;
        if (segments.at(-1)?.leader !== member.name) segments.push({leader: member.name, rows: []});
        segments.at(-1).rows.push(row);
      }
      const confirmedRoutes = row => row.routes?.status === 'ok' && row.routes.probe?.role === 'primary' &&
        (!row.routes.readEntry || (row.routes.readStatus === 'ok' && row.routes.readProbe?.role === 'standby'));
      const switches = segments.slice(1).map((next, index) => {
        const prior = segments[index].rows;
        const after = next.rows;
        return {from: prior.findLast(confirmedRoutes) || prior.at(-1), to: after.find(confirmedRoutes) || after[0]};
      });
      const overlaps = (event, change) => Date.parse(event.startedAt) <= Date.parse(change.to.collectedAt) &&
        (!event.endedAt || Date.parse(event.endedAt) >= Date.parse(change.from.collectedAt));
      const origin = events.find(item => item.id === eventAnchor);
      const change = origin ? switches.findLast(item => overlaps(origin, item)) : switches.at(-1);
      if (!change) {
        card.append(el('p', '此时段尚无来自两个主库的切换观测。', 'monitor-note'));
        return card;
      }
      const {from: changeFrom, to: changeTo} = change;
      const routeText = (status, probe) => probe ? [status === 'ok' ? '可用' : status, probe.backend, roleLabels[probe.role]].filter(Boolean).join(' · ') : '未采集';
      card.append(table(['阶段', '观测时间', '主库与时间线', '主入口', '备库入口', '最大回放差距', '采样'],
        [['切换前', changeFrom], ['切换后', changeTo]].map(([stage, row]) => {
          const routes = row.routes;
          const lag = (row.patroni?.members || []).map(item => item.replayLagBytes ?? item.lagBytes).filter(finite);
          return [stage, formatTime(row.collectedAt), `${leader(row).name} · ${leader(row).timeline || '时间线未知'}`,
            routeText(routes?.status, routes?.probe), routeText(routes?.readStatus, routes?.readProbe),
            lag.length ? bytes(Math.max(...lag)) : '未采集', sampleLink(asset.id, row.id)];
        }), '切换前后证据'));
      const first = Date.parse(changeFrom.collectedAt), after = Date.parse(changeTo.collectedAt);
      card.append(el('p', `切换观测间隔 ${number((after - first) / 1000, 2)} 秒 · 两条主库采样之间`, 'monitor-note'));
      for (const event of events.filter(item => overlaps(item, change))) {
        card.append(button(`同窗事件 · ${event.title}`, () => openEvent(event.id), 'monitor-link'));
      }
      return card;
    }

    function topologyPanel(views, time) {
      const card = panel('集群复制拓扑与服务入口');
      const end = replicaAt != null && replicaAt >= time.from && replicaAt < time.to ? replicaAt : time.to;
      const changes = views.flatMap(({ asset, observations }) => observations.filter(row => row.kind === 'change')
        .map(row => ({ at: Date.parse(row.collectedAt), asset, row })));
      changes.sort((left, right) => right.at - left.at || left.asset.name.localeCompare(right.asset.name));
      const chooser = select('拓扑观测时间', value => { replicaAt = value ? Number(value) : null; change(); });
      const points = new Map();
      for (const change of changes.slice(0, 60)) {
        const key = Math.min(change.at + 1, time.to - 1);
        if (!points.has(key)) points.set(key, `${formatTime(change.at)} · ${change.asset.name} · ${roleLabels[change.row.role] || '角色未知'}`);
      }
      if (end < time.to && !points.has(end)) points.set(end, formatTime(end));
      options(chooser, [['', '区间末次观测'], ...Array.from(points, ([at, title]) => [String(at), title])], end === time.to ? '' : String(end));
      card.append(labeled('拓扑时间', chooser));
      const peers = views.map(({ asset, observations }) => ({ asset, point: observations.find(row => row.kind === 'point') }));
      const address = (host, port) => host ? `${host.includes(':') ? '[' + host + ']' : host}:${port || '—'}` : '—';
      const recent = point => point && point.postgresState === 'available' && end - Date.parse(point.collectedAt) <= 600000;
      const edge = peer => {
        const { point } = peer;
        const receiver = point?.receiver;
        if (!recent(point) || !receiver?.senderHost) return '—';
        const candidates = peers.filter(({ asset, point: source }) => recent(source) && source.role &&
          Math.abs(Date.parse(source.collectedAt) - Date.parse(point.collectedAt)) <= Math.min(time.bucketMs, 600000) &&
          ((asset.host === receiver.senderHost && asset.port === receiver.senderPort) ||
            (source.endpoint?.address === receiver.senderHost && source.endpoint?.port === receiver.senderPort)));
        const source = candidates.length === 1 ? candidates[0] : null;
        if (source?.point.senders?.some(link => link.clientAddress && link.clientAddress === point.endpoint?.address))
          return button(`双端匹配 · ${source.asset.name}`, () => openInstance(source.asset.id, 'replication'), 'monitor-link');
        return `接收端报告 · ${address(receiver.senderHost, receiver.senderPort)}`;
      };
      const readings = {available: '可连通', unavailable: '连接失败', unknown: '连通性未知'};
      card.append(table(['成员', 'PG 观测', '时间线', '上游', '复制状态', '观测时间', '采样'], peers.map(peer => {
        const { asset, point } = peer;
        const state = !point ? '缺采' : end - Date.parse(point.collectedAt) > 600000 ? '采样间隔超过 10 分钟' :
          [readings[point.postgresState], roleLabels[point.role]].filter(Boolean).join(' · ');
        const replication = point?.paused ? `回放暂停 · ${display(point.replayGap, bytes)}` :
          point?.receiverState ? (point.receiverState === 'absent' ? '接收端无 WAL 进程' : point.receiverState) :
            point?.senders ? `${point.senders.length} 条发送会话` : '—';
        const archiveFailure = point?.archive?.lastFailedAt &&
          (!point.archive.lastArchivedAt || Date.parse(point.archive.lastFailedAt) > Date.parse(point.archive.lastArchivedAt));
        return [button(asset.name, () => openInstance(asset.id, 'replication'), 'monitor-link'), state, point?.timeline,
          edge(peer), archiveFailure ? `${replication} · 最近归档失败` : replication,
          point && formatTime(point.collectedAt), point && sampleLink(asset.id, point.id)];
      }), '集群拓扑'));
      const scopedPeers = assets.filter(asset => clusterKey(asset) === clusterKey(peers[0].asset)).length;
      if (scopedPeers > peers.length) card.append(el('p', `此视图读取当前实例及同集群最多四个成员（${peers.length} / ${scopedPeers}）。`, 'monitor-note'));
      const routeRows = [];
      for (const { asset, point } of peers) {
        if (!point || end - Date.parse(point.collectedAt) > 600000) continue;
        const evidence = () => sampleLink(asset.id, point.id);
        routeRows.push([asset.tags?.includes('haproxy') ? 'HAProxy · PostgreSQL 探测' : 'PostgreSQL 探测', asset.name,
          address(asset.host, asset.port), address(point.endpoint?.address, point.endpoint?.port),
          [readings[point.postgresState], roleLabels[point.role]].filter(Boolean).join(' · '), evidence()]);
        if (asset.poolerConfigured || point.pool) {
          const pool = point.pool, route = pool?.route;
          const waiting = pool?.pools?.reduce((sum, item) => sum + (item.clientWaiting || 0), 0);
          const observed = [...new Set((pool?.servers || []).map(server => address(server.address, server.port)))].slice(0, 5);
          const reported = route ? address(route.host, route.port) : '—';
          routeRows.push(['PgBouncer', asset.name, asset.poolerEndpoint,
            observed.length ? observed.join(' / ') : `SHOW DATABASES · ${reported}`,
            route?.disabled ? '已禁用' : route?.paused ? '已暂停' : waiting > 0 ? `${waiting} 个客户端排队` : pool?.status === 'ok' ? '统计可读' : '当前无统计', evidence()]);
        }
        if (point.routes?.entry) {
          for (const [label, entry, status, probe] of [
            ['主入口探测', point.routes.entry, point.routes.status, point.routes.probe],
            ['备库入口探测', point.routes.readEntry, point.routes.readStatus, point.routes.readProbe]
          ]) if (entry) routeRows.push([label, asset.name, entry,
            [probe?.backend, roleLabels[probe?.role]].filter(Boolean).join(' · ') || '后端待核对',
            status === 'ok' ? '可用' : status || '待观测', evidence()]);
        }
        for (const service of point.services || []) routeRows.push(['Kubernetes Service', asset.name,
          `${point.namespace}/${service.name} · ${service.clusterIP || '—'} · ${(service.ports || []).join(', ')}`,
          point.pod && `标签选择 · ${point.pod}`, point.podReady === 'True' ? 'Pod 就绪' : point.podReady === 'False' ? 'Pod 未就绪' : 'Pod 就绪未知', evidence()]);
      }
      if (routeRows.length) card.append(table(['来源', '成员', '配置／服务地址', '采样观测', '状态', '采样'], routeRows, '服务入口与后端'));
      else card.append(el('p', '该时间点缺少服务入口采样。', 'monitor-note'));
      return card;
    }

    function servicesPanel(asset, sample) {
      const card = panel('高可用与连接池');
      const patroni = sample.patroni;
      const pooler = sample.pooler;
      card.append(el('p', patroni?.status === 'ok'
        ? `Patroni · leader ${patroni.leaderCount} · ${formatTime(patroni.observedAt)}`
        : `Patroni · ${asset.patroniConfigured ? '此采样不可用' : '未配置'}`, 'monitor-note'));
      if (patroni?.nodeState?.status === 'ok') {
        const node = patroni.nodeState;
        const heartbeat = {healthy: '正常', unhealthy: '异常', unavailable: '未知'};
        card.append(el('p', `DCS 最近可见 ${formatTime(node.dcsLastSeen)} · 观测间隔 ${display(node.dcsAgeSeconds, value => number(value, 2) + ' 秒')} · 观测节点 ${node.name}`, 'monitor-note'));
        card.append(el('p', `Patroni 心跳 ${heartbeat[patroni.heartbeat?.status] || '未知'}${node.unlocked ? ' · 集群无主锁' : ''}${node.failsafe ? ' · Failsafe 正在使用' : ''}`, 'monitor-note'));
      } else if (patroni?.status === 'ok') card.append(el('p', 'DCS 观测暂不可用。', 'monitor-note'));
      if (patroni?.members?.length) card.append(table(['Patroni 成员', '角色', '状态', '时间线', '回放差距', '同步资格 / 状态'],
        patroni.members.map(member => {
          const matches = members().filter(item => item.host === member.host && item.port === member.port);
          const sync = patroni.nodeState?.replication?.find(item => item.name === member.name)?.syncState;
          return [matches.length === 1 ? button(member.name, () => openInstance(matches[0].id, 'replication'), 'monitor-link') : member.name,
            member.role, member.state, member.timeline > 0 ? member.timeline : null, display(member.replayLagBytes ?? member.lagBytes, bytes),
            member.nosync ? '已排除同步' : [sync, ['sync_standby', 'quorum_standby'].includes(member.role) ? '同步候选' : ''].filter(Boolean).join(' · ') || '待观测'];
        }), 'Patroni 成员'));
      if (patroni?.history?.entries?.length) card.append(table(['切换时间', '先前时间线', '切换 WAL 位置', '新主库'],
        [...patroni.history.entries].reverse().map(row => [formatTime(row.switchedAt), row.previousTimeline, row.switchLSN, row.newLeader]), 'Patroni 时间线历史'));
      const poolerState = pooler?.status === 'ok' ? `${pooler.database} · ${formatTime(pooler.observedAt)}` :
        pooler?.status === 'unmapped' ? '未匹配到纳管数据库' : asset.poolerConfigured ? '此采样不可用' : '未配置统计采集';
      card.append(el('p', `PgBouncer · ${poolerState}${asset.poolerEndpoint ? ` · 配置的应用端点 ${asset.poolerEndpoint}` : ''}`, 'monitor-note'));
      if (pooler?.pools?.length) card.append(table(['用户', '模式', '活动客户端', '等待客户端', '活动服务端', '最大等待'],
        pooler.pools.map(row => [row.user, row.mode, row.clientActive, row.clientWaiting, row.serverActive,
          display(row.maxWaitSeconds, value => `${number(value, 3)} s`)]), 'PgBouncer 连接池'));
      return card;
    }

    function backupPanel(asset, data) {
      const card = panel('备份与时间点恢复');
      const observations = data.observations;
      const body = el('div');
      if (observations.length > 1) {
        const picker = select('备份证据采样', id => describe(observations.find(row => row.id === id)));
        options(picker, observations.map(row => [row.id, formatTime(row.collectedAt)]),
          observations.find(row => row.id === focusSample)?.id || observations[0].id);
        card.append(labeled('采样', picker));
      }
      card.append(body);
      describe(observations.find(row => row.id === focusSample) || observations[0]);
      return card;

      function describe(sample) {
        body.replaceChildren();
        if (!sample) {
          body.append(el('p', '此时段没有备份或 WAL 采样。', 'monitor-empty'));
          return;
        }
        const source = el('p', `${formatTime(sample.collectedAt)} · `, 'monitor-note');
        source.append(sampleLink(asset.id, sample.id));
        body.append(source);
        const archiver = sample.archiver;
        if (archiver) body.append(stats([
          ['归档成功', display(archiver.archivedCount), formatTime(archiver.lastArchivedAt)],
          ['归档失败', display(archiver.failedCount), formatTime(archiver.lastFailedAt)]
        ]));
        const report = sample.report;
        if (report?.status !== 'ok') {
          body.append(el('p', report?.reason || '该时点缺少 pgBackRest 报告，WAL 计数来自 PostgreSQL。', 'monitor-note'));
          return;
        }
        const seconds = value => `${number(value)} s`;
        body.append(stats([
          ['pgBackRest stanza', report.stanza, report.repositoryHealthy ? '报告正常' : '报告异常'],
          ['最近成功备份', formatTime(report.lastSuccessAt), report.lastAttemptStatus === 'failed' ? '最近尝试失败' : ''],
          ['目标 RPO', display(report.targetRpoSeconds || null, seconds)],
          ['目标 RTO', display(report.targetRtoSeconds || null, seconds)]
        ]));
        if (report.archives.length) body.append(table(['仓库', '数据库编号', '最早归档 WAL', '最近归档 WAL', '仓库状态'],
          report.archives.map(row => [row.repoKey, row.databaseId, row.first, row.last,
            report.repositories.find(repo => repo.key === row.repoKey)?.healthy === true ? '报告正常' :
              report.repositories.some(repo => repo.key === row.repoKey) ? '报告异常' : '状态未上报']), '仓库 WAL 边界'));
        body.append(el('p', '归档边界来自 pgBackRest。连续性以逐段清单核查及隔离恢复记录为准。', 'monitor-note'));
        if (report.walChecks.length) body.append(table(['仓库', '数据库编号', '核查范围', '已列出 / 应有', '缺失 WAL'],
          report.walChecks.map(check => {
            const covered = el('span', `${check.present} / ${check.present + check.missingCount}`);
            const bar = el('progress');
            bar.max = check.present + check.missingCount; bar.value = check.present;
            bar.setAttribute('aria-label', 'WAL 清单覆盖');
            covered.append(' ', bar);
            const missing = check.missingCount ? `${check.missingCount} 段 · ${check.missing.join(', ')}` : '清单无缺口';
            return [check.repoKey, check.databaseId, `${check.first} → ${check.last}`, covered, el('span', missing, check.missingCount ? 'monitor-warning' : '')];
          }), 'WAL 逐段核查'));
        if (report.jobs.length) body.append(table(['备份标识', '类型', '仓库', '完成时间', '容量', '依赖', '状态'],
          report.jobs.map(job => [job.label, {full: '全量', diff: '差异', incr: '增量'}[job.type], job.repoKey || '未知',
            formatTime(job.completedAt), display(job.bytes, bytes), [job.prior, ...job.references].filter(Boolean).join(' / '),
            job.failed ? '失败' : job.dependenciesAvailable ? '清单完整' : '依赖未齐']), '备份清单'));
        if (report.jobLimit) body.append(el('p', '当前采样最多保留 100 个备份条目。', 'monitor-note'));
        if (report.restoreTests.length) body.append(table(['演练目标时刻', '备份', '仓库', '目标前事务', '目标后事务', '实测 RPO', '实测 RTO', '结束时间'],
          report.restoreTests.map(test => [formatTime(test.targetAt), test.label, test.repoKey, test.confirmedBefore ? '已核对' : '核对失败',
            test.excludedAfter ? '已排除' : '排除失败', display(test.rpoSeconds, seconds), display(test.rtoSeconds, seconds), formatTime(test.finishedAt)]), '隔离恢复演练记录'));
      }
    }

    function catalogPanel(asset, data, time, events) {
      const result = document.createDocumentFragment();
      const growth = new Map((data.growth || []).map(row => [`${row.kind}:${row.oid}`, row]));
      const sample = data.snapshot;
      const inventory = sample?.databases?.data;
      const databases = inventory?.databases || [];
      const selected = databases.find(row => row.oid === dbOid);
      const catalog = panel('实例内数据库');
      const activeDB = databases.find(row => row.connected);
      const summary = el('p', `来源 pg_stat_database · ${formatTime(sample?.collectedAt)}${inventory ? ` · ${databases.length} / ${inventory.total} 个可连接库` : ''}`, 'monitor-note');
      if (sample) summary.append(' · ', sampleLink(asset.id, sample.id));
      catalog.append(summary);
      if (!inventory) catalog.append(el('p', '此时段没有可用的数据库清单采样。', 'monitor-empty'));
      if (databases.length) {
        const rank = el('div');
        const order = select('数据库排行', key => drawRank(key));
        const chooser = select('实例内数据库', oid => chooseDatabase(oid));
        const controls = el('div', null, 'monitor-wait-controls');
        options(order, [['bytes', '容量'], ['growthBytes', '区间容量变化'], ['sessions', '连接']], 'bytes');
        options(chooser, [['', '查看数据库排行'], ...databases.map(row => [row.oid, row.name])], selected?.oid || '');
        controls.append(labeled('数据库', chooser), labeled('排行', order));
        catalog.append(controls, el('p', '容量变化按 OID 比较所选范围内的首末有效采样。7 天估算仅使用覆盖至少 24 小时的正增长。', 'monitor-note'), rank);
        const drawRank = key => {
          const sorted = databases.map(row => ({ ...row, ...growth.get(`database:${row.oid}`) }))
            .sort((a, b) => (b[key] ?? -Infinity) - (a[key] ?? -Infinity) || a.name.localeCompare(b.name));
          rank.replaceChildren(table(['数据库', '容量', '区间变化', '7 天增长估算', '读取块增量', '连接', 'XID 年龄', '证据'], sorted.map(row => [
            button(row.previousName ? `${row.previousName} → ${row.name}` : row.name, () => chooseDatabase(row.oid), 'monitor-link'),
            display(row.bytes, bytes), display(row.growthBytes, signedBytes), display(row.projectedGrowth7d, signedBytes),
            display(row.blocksReadDelta), display(row.sessions), display(row.xidAge), growthEvidence(asset.id, row)
          ]), '数据库排行'));
        };
        drawRank('bytes');
      }
      if (inventory && !databases.length) catalog.append(el('p', '本次采样没有可连接的数据库。', 'monitor-empty'));
      result.append(catalog);

      if (dbOid) {
        const databasePanel = panel(selected?.name || `数据库 OID ${dbOid}`);
        if (!selected) databasePanel.append(el('p', '此库缺少末次采样。保留历史观测。', 'monitor-note'));
        const rows = data.series.map(row => ({ ...row,
          writesPerSecond: [row.rates?.rowsInserted, row.rates?.rowsUpdated, row.rates?.rowsDeleted].every(finite)
            ? row.rates.rowsInserted + row.rates.rowsUpdated + row.rates.rowsDeleted : null,
          rowsReturnedPerSecond: row.rates?.rowsReturned, fetchedPerSecond: row.rates?.rowsFetched,
          temporaryBytes: row.deltas?.tempBytes, temporaryFiles: row.deltas?.tempFiles, deadlocks: row.deltas?.deadlocks
        }));
        databasePanel.append(metricCharts(rows, time, events, [
          ['transactionsPerSecond', '事务吞吐', rate, palette[0]], ['sessions', '平均连接', number, palette[1]],
          ['bytes', '数据库容量', bytes, palette[2]], ['cacheHitPercent', '读块缓存命中率', percent, palette[0]],
          ['rowsReturnedPerSecond', '扫描返回行 / 秒', number, palette[1]], ['fetchedPerSecond', '索引取回行 / 秒', number, palette[1]],
          ['writesPerSecond', '增删改行 / 秒', number, palette[2]], ['deadlocks', '死锁增量', number, 'var(--issue-line)'],
          ['temporaryFiles', '临时文件增量', number, palette[1]], ['temporaryBytes', '临时写入增量', bytes, palette[2]]
        ]));
        if (!rows.some(row => row.observed)) databasePanel.append(el('p', '所选 OID 在此时间范围内无可见样本。', 'monitor-note'));
        result.append(databasePanel);
        if (selected && !selected.connected) {
          const configured = assets.filter(item => clusterKey(item) === clusterKey(asset) && item.host === asset.host &&
            item.port === asset.port && item.database === selected.name);
          const link = panel('表与索引');
          link.append(el('p', configured.length === 1 ? '对象统计从该库的纳管连接读取。' : `对象统计仅覆盖当前纳管连接 ${asset.database}。`, 'monitor-note'));
          if (configured.length === 1) link.append(button(`打开 ${selected.name} 的对象采样`, () => openInstance(configured[0].id, 'catalog')));
          result.append(link);
        }
      }
      if (activeDB && (!dbOid || dbOid === activeDB.oid)) {
        if (objectOid) {
          const kind = objectKind === 'table' ? sample.tables : sample.indexes;
          const object = kind?.data?.objects.find(row => row.oid === objectOid);
          const focus = panel(object ? `${object.schema}.${object.name}` : `对象 OID ${objectOid}`);
          focus.append(button('返回对象排行', () => { objectOid = ''; objectKind = ''; change(); }));
          focus.append(metricCharts(data.objectSeries, time, events, [
            ['bytes', '对象容量', bytes, palette[2]], ['deadRows', '死元组估计', number, palette[1]],
            ['blocksRead', '累计读取块', number, palette[0]], ['modifiedSinceAnalyze', 'Analyze 后变化行数', number, palette[2]],
            ['xidAge', '冻结 XID 年龄', number, palette[0]]
          ].filter(([key]) => objectKind === 'table' || !['deadRows', 'modifiedSinceAnalyze', 'xidAge'].includes(key))));
          const last = data.objectSeries.findLast(row => finite(row.bytes));
          if (last) focus.append(sampleLink(asset.id, last.sampleId, '对象原始采样'));
          result.append(focus);
        }
        result.append(maintenancePanel(asset, sample.tables, activeDB, growth),
          objectRanking(asset, sample.tables, activeDB, 'table', growth), objectRanking(asset, sample.indexes, activeDB, 'index', growth));
      }
      const io = sample?.instanceIO?.data;
      const major = Math.floor((sample?.versionNum || asset.versionNum || 0) / 10000);
      const ioCard = panel('I/O 采样范围');
      ioCard.append(el('p', io ? 'pg_stat_io · 实例级客户端关系 I/O 累计值。库与对象的读块来自各自统计视图。' :
        major >= 16 ? '本次未取得实例级 pg_stat_io。' : '此版本使用库与对象的块读取与命中统计。', 'monitor-note'));
      if (io) ioCard.append(stats([['读取', display(io.reads)], ['写入', display(io.writes)], ['同步', display(io.fsyncs)]]));
      result.append(ioCard);
      return result;
    }

    function chooseDatabase(oid) {
      dbOid = oid; objectOid = ''; objectKind = ''; change();
    }

    function growthEvidence(assetId, row) {
      if (!row.lastSample) return '—';
      const evidence = el('span');
      if (finite(row.seconds)) evidence.append(sampleLink(assetId, row.firstSample, '起点'), ' · ');
      evidence.append(sampleLink(assetId, row.lastSample, '末点'));
      return evidence;
    }

    function maintenancePanel(asset, inventory, db, growth) {
      const card = panel('表维护、冻结与 Analyze');
      const objects = inventory?.data?.objects || [];
      if (!objects.length) {
        card.append(el('p', '此范围缺少可见的表维护采样。', 'monitor-note'));
        return card;
      }
      const list = objects.map(row => ({ ...row, ...growth.get(`table:${row.oid}`) }));
      const coverage = list.find(row => finite(row.seconds));
      card.append(el('p', `死元组和活元组为 PostgreSQL 统计估计。${coverage ? ' 对照窗口 ' + formatTime(coverage.firstAt) + ' 至 ' + formatTime(coverage.lastAt) + '。' : ''}`, 'monitor-note'));
      const rank = el('div');
      const order = select('表维护排序', render);
      options(order, [['deadRows', '死元组'], ['freeze', '冻结阈值占比'], ['modifiedSinceAnalyze', '未分析变化行']], 'deadRows');
      card.append(labeled('排序', order), rank);
      function render(key) {
        const rows = list.map(row => ({ ...row, freeze: row.xidLimit > 0 && finite(row.xidAge) ? Math.max(row.xidAge, 0) / row.xidLimit * 100 : null }))
          .sort((a, b) => (b[key] ?? -Infinity) - (a[key] ?? -Infinity)).slice(0, 10);
        rank.replaceChildren(table(['表', '死元组估计 · 前 → 后', '死元组占比', '冻结阈值占比', 'Autovacuum', '未分析变化行', '最近 Vacuum', '最近 Analyze', '证据'], rows.map(row => {
          const total = (row.liveRows || 0) + (row.deadRows || 0);
          return [button(`${row.schema}.${row.name}`, () => { dbOid = db.oid; objectOid = row.oid; objectKind = 'table'; change(); }, 'monitor-link'),
            `${display(row.previousDeadRows)} → ${display(row.deadRows)}`, total > 0 ? percent(100 * row.deadRows / total) : '—',
            display(row.freeze, percent), typeof row.autovacuumEnabled === 'boolean' ? (row.autovacuumEnabled ? '启用' : '常规关闭') : '—',
            display(row.modifiedSinceAnalyze), formatTime(row.lastVacuumAt), formatTime(row.lastAnalyzeAt), growthEvidence(asset.id, row)];
        }), '表维护排行'));
      }
      render('deadRows');
      return card;
    }

    function objectRanking(asset, inventory, db, kind, growth) {
      const isTable = kind === 'table';
      const title = isTable ? '表排行' : '索引排行';
      const card = panel(`${title} · ${db.name}`);
      const objects = inventory?.data?.objects || [];
      if (!objects.length) {
        card.append(el('p', inventory?.status === 'ok' ? '未观测到可读对象。' : '此时段缺少对象采样。', 'monitor-empty'));
        return card;
      }
      const controls = el('div', null, 'monitor-wait-controls');
      const order = select(`${title}排序`, value => { by = value; currentPage = 0; render(); });
      const schemaFilter = select(`${title} Schema`, value => { schema = value; currentPage = 0; render(); });
      const output = el('div');
      let by = 'bytes', currentPage = 0, schema = '';
      const pageSize = 10;
      const labels = isTable ? ['容量', '区间容量变化', '表级读取增量', '死元组', '写入增量'] : ['容量', '区间容量变化', '索引读取增量', '扫描增量'];
      const fields = isTable ? ['bytes', 'growthBytes', 'blocksReadDelta', 'deadRows', 'writesDelta'] : ['bytes', 'growthBytes', 'blocksReadDelta', 'scansDelta'];
      options(order, fields.map((field, i) => [field, labels[i]]), by);
      options(schemaFilter, [['', '全部 Schema'], ...[...new Set(objects.map(row => row.schema))].sort().map(schema => [schema, schema])], schema);
      controls.append(labeled('Schema', schemaFilter), labeled('排序', order));
      card.append(el('p', `已采样 ${objects.length} / ${inventory.data.total} 个可见对象 · 依容量、活动和维护信号选取`, 'monitor-note'), controls, output);
      function render() {
        const rows = objects.filter(row => !schema || row.schema === schema).map(row => ({ ...row, ...growth.get(`${kind}:${row.oid}`) }))
          .sort((a, b) => (b[by] ?? -Infinity) - (a[by] ?? -Infinity) || a.name.localeCompare(b.name));
        output.replaceChildren(table(isTable ? ['表', '类型 / 父 OID', '容量', '区间变化', '读取增量', '写入增量', '死元组', '证据'] :
          ['索引', '所属表', '容量', '区间变化', '读取增量', '扫描增量', '证据'], rows.slice(currentPage * pageSize, (currentPage + 1) * pageSize).map(row => {
          const label = [row.schema + '.' + row.name, row.previousName && `原名 ${row.previousName}`, row.rewritten && '存储重写'].filter(Boolean).join(' · ');
          const name = button(label, () => { dbOid = db.oid; objectOid = row.oid; objectKind = kind; change(); }, 'monitor-link');
          return isTable ? [name, row.partition ? `分区 / ${row.parentOid || '—'}` : row.relationKind === 'p' ? '分区根表' : '表',
            display(row.bytes, bytes), display(row.growthBytes, signedBytes), display(row.blocksReadDelta), display(row.writesDelta),
            display(row.deadRows), growthEvidence(asset.id, row)] :
            [name, row.table, display(row.bytes, bytes), display(row.growthBytes, signedBytes), display(row.blocksReadDelta),
              display(row.scansDelta), growthEvidence(asset.id, row)];
        }), title));
        if (rows.length > pageSize) {
          const nav = el('div', null, 'monitor-wait-controls');
          const previous = button('上一页', () => { currentPage--; render(); });
          const next = button('下一页', () => { currentPage++; render(); });
          previous.disabled = currentPage === 0;
          next.disabled = (currentPage + 1) * pageSize >= rows.length;
          nav.append(previous, el('span', `${currentPage + 1} / ${Math.ceil(rows.length / pageSize)}`, 'monitor-note'), next);
          output.append(nav);
        }
      }
      render();
      return card;
    }

    function environmentPanel(asset, snapshot, data, time, events) {
      const result = document.createDocumentFragment();
      if (snapshot?.routing) {
        const entry = panel('代理与访问入口');
        entry.append(actions.routingOverview(asset.id, snapshot.routing, snapshot.id));
        result.append(entry);
      }
      if (!data) { result.append(renderInfra(snapshot, asset)); return result; }
      const kubernetes = asset.runtime === 'k8s';
      const timeline = panel('PostgreSQL 与环境同窗');
      const charts = el('div', null, 'monitor-charts');
      const statusGraph = (title, positive, negative) => graph(title, time, aligned(data.series, time, row => {
        const values = [[positive, '可用', 'var(--ok-line)'], [negative, '异常', 'var(--issue-line)']]
          .filter(([key]) => row[key] > 0).map(([key, label, color]) => ({ label, color, value: row[key] }));
        return values.length ? { values } : null;
      }), events, value => `${number(value)} 次采样`, true);
      charts.append(statusGraph('PostgreSQL 连通性', 'pgAvailable', 'pgUnavailable'));
      if (kubernetes) charts.append(statusGraph('Pod 就绪状态', 'podReady', 'podUnready'));
      else charts.append(metricCharts(data.series, time, events, [['cpuPercent', 'Linux CPU 占用', percent, palette[0]]]).firstElementChild);
      timeline.append(charts);

      const history = panel(kubernetes ? 'Pod、Node 与 PVC 状态变化' : '主机与数据库状态变化');
      if (data.changes.length) history.append(table(kubernetes ? ['观测时间', 'PG 状态', 'Pod / UID', '就绪 / 调度', 'Node / 压力', '重启计数', 'PVC', '采样'] :
        ['观测时间', 'PG 状态', '主机记录', '采样'], data.changes.map(row => {
        const pg = row.pg === 'ok' ? '可用' : row.pg === 'availability' ? '不可用' : '未知';
        const origin = sampleLink(asset.id, row.id);
        if (!kubernetes) return [formatTime(row.collectedAt), pg, [row.hostRestart && '主机启动时间变化',
          row.hostEndpointChanged && '采集端点变化', row.hostEndpoint].filter(Boolean).join(' · ') || '—', origin];
        const pod = [row.pod, row.uid, row.podPhase, row.podReason, row.deletingAt && '终止中'];
        if (row.kubeStatus && row.kubeStatus !== 'ok') pod.push(row.kubeReason || 'Kubernetes 对象状态未知');
        if (row.uid && row.previousUID && row.uid !== row.previousUID) pod.push(`前一 UID ${row.previousUID}`);
        const summary = pairs => Object.entries(pairs || {}).map(([key, value]) => `${key} ${value ?? '未知'}`).join(' · ');
        const ready = row.ready === 'True' ? '就绪' : row.ready === 'False' ? '未就绪' : '未知';
        const scheduled = row.scheduled === 'True' ? '已调度' : row.scheduled === 'False' ? '调度失败' : '未知';
        return [formatTime(row.collectedAt), pg, pod.filter(Boolean).join(' · '), `${ready} / ${scheduled}`,
          [row.node, row.nodePressure].filter(Boolean).join(' · '), summary(row.restarts), summary(row.pvcs), origin];
      }), kubernetes ? 'Pod UID 历史' : 'Linux 环境历史'));
      else history.append(el('p', '此时段没有环境历史采样。', 'monitor-empty'));

      const bySecond = value => `${bytes(value)}/s`;
      const resourceCharts = kubernetes ? [
        ['podCPUCores', 'Pod CPU · metrics.k8s.io', value => `${number(value, 3)} 核`, palette[0], 'podUid'],
        ['podMemoryBytes', 'Pod 内存 · metrics.k8s.io', bytes, palette[1], 'podUid']
      ] : [
        ['memoryPercent', 'Linux 内存占用', percent, palette[1]], ['cpuLoad', 'Linux 1 分钟负载', number, palette[0]],
        ['cpuIOWait', 'Linux CPU I/O 等待', percent, palette[2]],
        ['diskUsedPercent', '文件系统最高占用', percent, palette[2]], ['diskReadBps', '磁盘读取', bySecond, palette[0]],
        ['diskWriteBps', '磁盘写入', bySecond, palette[2]], ['networkReceiveBps', '网络接收', bySecond, palette[0]],
        ['networkTransmitBps', '网络发送', bySecond, palette[1]], ['pressureCPU', 'CPU 压力 · PSI', percent, palette[2]],
        ['pressureMemory', '内存压力 · PSI', percent, palette[1]], ['pressureIO', 'I/O 压力 · PSI', percent, palette[2]]
      ];
      const resources = panel(kubernetes ? '容器资源历史' : 'Linux 资源历史 · node_exporter');
      let graphs;
      let graphCursors = [];
      const drawResources = name => {
        for (const cursor of graphCursors) time.cursors.delete(cursor);
        const previous = new Set(time.cursors);
        const selected = data.containers.find(item => item.name === name);
        const rendered = metricCharts(selected?.series || data.series, time, events, selected ? [
          ['cpuCores', '容器 CPU · metrics.k8s.io', value => `${number(value, 3)} 核`, palette[0], 'podUid'],
          ['memoryBytes', '容器内存 · metrics.k8s.io', bytes, palette[1], 'podUid'],
          ['cpuRequest', '容器 CPU 请求', number, palette[2], 'podUid'],
          ['cpuLimit', '容器 CPU 限额', number, palette[3], 'podUid'],
          ['memoryRequest', '容器内存请求', bytes, palette[2], 'podUid'],
          ['memoryLimit', '容器内存限额', bytes, palette[3], 'podUid']
        ] : resourceCharts);
        graphCursors = [...time.cursors].filter(cursor => !previous.has(cursor));
        if (graphs) graphs.replaceWith(rendered);
        else resources.append(rendered);
        graphs = rendered;
      };
      if (kubernetes && data.containers.length) {
        const container = select('容器资源', drawResources);
        options(container, [['', 'Pod 总量'], ...data.containers.map(item => [item.name, item.name])], '');
        const controls = el('div', null, 'monitor-wait-controls');
        controls.append(labeled('容器', container));
        resources.append(controls);
      }
      drawResources('');
      if (kubernetes && !data.series.some(row => finite(row.podCPUCores) || finite(row.podMemoryBytes)))
        resources.append(el('p', '此时段无可用的 Pod 资源指标。就绪、调度、重启和卷状态来自 Kubernetes 对象采样。', 'monitor-note'));
      const covered = key => data.series.reduce((sum, row) => sum + (row[key] ?? 0), 0);
      const total = covered('observed');
      const coverage = kubernetes ? `Kubernetes 对象 ${covered('kubeObserved')}/${total} · 资源用量 ${covered('resourceObserved')}/${total}` :
        `node_exporter ${covered('hostObserved')}/${total} 次采样`;
      resources.insertBefore(el('p', coverage, 'monitor-note'), resources.children[1] || null);
      result.append(timeline, history, renderInfra(snapshot, asset));
      result.append(kubernetes ? kubernetesIdentityPanel(asset, snapshot, data.changes) : linuxDevicePanel(asset, snapshot, data, time, events), resources);
      return result;
    }

    function changeTimelinePanel(asset, data, time, events) {
      const card = panel('变更与事件时间线');
      const changes = data.changes || [];
      const entries = changes.map(item => ({ at: Date.parse(item.occurredAt), change: item }));
      for (const event of events) {
        const opened = Date.parse(event.startedAt);
        const recovered = Date.parse(event.endedAt);
        if (opened >= time.from && opened < time.to) entries.push({at: opened, event, phase: '事件开始'});
        if (recovered >= time.from && recovered < time.to) entries.push({at: recovered, event, phase: '事件恢复'});
      }
      entries.sort((a, b) => b.at - a.at);
      if (entries.length) {
        const timeline = el('ol', null, 'monitor-change-list');
        for (const entry of entries) {
          const row = el('li');
          row.append(el('time', formatTime(entry.at), 'monitor-note'));
          if (entry.event) {
            row.append(button(`${entry.phase} · ${entry.event.title}`, () => openEvent(entry.event.id), 'monitor-link'));
          } else {
            const item = entry.change;
            const action = button(`${changeKinds[item.kind] || item.kind} · ${item.label}`, () => {
              changePick = item.id; change();
            }, 'monitor-link');
            action.setAttribute('aria-pressed', String(item.id === changePick));
            row.append(action, el('small', changeSources[item.source] || item.source, 'monitor-note'));
          }
          timeline.append(row);
        }
        card.append(timeline);
      } else {
        card.append(el('p', '此时段没有变更或事件节点。', 'monitor-note'));
      }
      if (data.truncated) card.append(el('p', '此时段仅显示最近 100 条变更。可缩小时间范围。', 'monitor-note'));
      const selected = changes.find(item => item.id === changePick);
      if (selected) {
        const compare = panel('变更前后对照');
        const label = selected.source === 'operator' ? '发生时间' : '首次观测';
        compare.append(el('h4', selected.label),
          el('p', `${label} ${formatTime(selected.occurredAt)} · 入库 ${formatTime(selected.recordedAt)} · ${changeSources[selected.source]}`, 'monitor-note'));
        if (selected.fields?.length) {
          compare.append(table(['项目', '之前', '之后'], selected.fields.map(field => [field.name, field.before, field.after]), '变更字段对照'));
        }
        compare.append(table(['采样', '时间', '来源'], [['变更前', selected.before], ['变更后', selected.after]].map(([name, point]) =>
          [name, point ? formatTime(point.collectedAt) : '无可用采样', point ? sampleLink(asset.id, point.id) : '—']), '变更采样对照'));
        card.append(compare);
      } else if (changePick) {
        card.append(el('p', '所选时间窗没有该变更。', 'monitor-note'));
      }
      if (actions.canRecordChanges?.()) {
        const details = el('details', null, 'settings-details');
        details.append(el('summary', '登记运维变更'));
        const form = el('form', null, 'form-grid routing-form');
        const kind = select('变更类型', () => {});
        options(kind, Object.entries(changeKinds), 'deployment');
        const occurred = el('input'), name = el('input');
        occurred.type = 'datetime-local'; occurred.required = true;
        occurred.value = dateTimeInputValue(Math.min(Date.now(), time.to - 1000));
        name.setAttribute('aria-label', '变更标题'); name.required = true; name.maxLength = 120;
        occurred.setAttribute('aria-label', '变更发生时间');
        const submit = el('button', '登记变更', 'monitor-action');
        submit.type = 'submit';
        const feedback = el('p', null, 'monitor-note wide');
        feedback.setAttribute('role', 'status');
        form.append(labeled('类型', kind), labeled('发生时间', occurred), labeled('标题', name), submit, feedback);
        form.addEventListener('submit', async event => {
          event.preventDefault();
          submit.disabled = true;
          try {
            const at = new Date(occurred.value).getTime();
            const recorded = await api(`/api/assets/${asset.id}/changes`, 'POST',
              {kind: kind.value, label: name.value, occurredAt: new Date(at).toISOString()});
            if (instance !== asset.id || mode !== 'changes') return;
            changePick = recorded.id;
            if (at < time.from || at >= time.to) {
              fixed = { from: at - 15 * minute, to: Math.min(Date.now(), at + 15 * minute) };
              activeWindow = null;
            }
            change();
          } catch (error) {
            feedback.textContent = error.message;
          } finally {
            submit.disabled = false;
          }
        });
        details.append(form); card.append(details);
      }
      return card;
    }

    function linuxDevicePanel(asset, snapshot, data, time, events) {
      const card = panel('块设备、网卡与文件系统');
      const host = snapshot?.linux;
      const labels = {disk: '磁盘', network: '网卡', filesystem: '挂载点'};
      const devices = ['disk', 'network'].flatMap(kind => Object.keys(host?.devices?.[kind] || {}).sort().map(name => [`${kind}:${name}`, `${labels[kind]} · ${name}`]));
      for (const fs of host?.filesystems || []) devices.push([`filesystem:${fs.mountpoint}`, `挂载点 · ${fs.mountpoint}`]);
      const active = deviceType && deviceName ? `${deviceType}:${deviceName}` : '';
      if (active && !devices.some(([key]) => key === active)) devices.push([active, `${labels[deviceType]} · ${deviceName}`]);
      const chooser = select('Linux 资源对象', value => {
        const split = value.indexOf(':');
        deviceType = split < 0 ? '' : value.slice(0, split);
        deviceName = split < 0 ? '' : value.slice(split + 1);
        change();
      });
      options(chooser, [['', '核对主机总量'], ...devices], active);
      card.append(labeled('对象', chooser));
      if (host?.endpoint) card.append(el('p', `node_exporter · ${host.endpoint}`, 'monitor-note'));
      if (!active) return card;
      const rows = data.deviceSeries || [];
      const bySecond = value => `${bytes(value)}/s`;
      const dimensions = {
        disk: [['readBps', '设备读吞吐', bySecond], ['writeBps', '设备写吞吐', bySecond], ['busyPercent', '设备 I/O 时间占比', percent],
          ['queueDepth', '设备平均 I/O 队列深度', number], ['readIOPS', '设备读 IOPS', rate], ['writeIOPS', '设备写 IOPS', rate]],
        network: [['receiveBps', '网卡接收', bySecond], ['transmitBps', '网卡发送', bySecond], ['receiveErrors', '接收错误', rate],
          ['transmitErrors', '发送错误', rate], ['receiveDrops', '接收丢包', rate], ['transmitDrops', '发送丢包', rate]],
        filesystem: [['usedPercent', '挂载点占用', percent], ['availableBytes', '可用容量', bytes], ['sizeBytes', '文件系统容量', bytes]]
      };
      card.append(metricCharts(rows, time, events, dimensions[deviceType].map(([key, title, format], i) => [key, title, format, palette[i % palette.length], 'identity'])));
      const latest = rows.findLast(row => row.observed);
      if (latest) card.append(sampleLink(asset.id, latest.sampleId));
      else card.append(el('p', '所选时段无此对象的可见采样。', 'monitor-note'));
      return card;
    }

    function cloudNativePGPanel(asset, cluster, changes) {
      const card = panel('CloudNativePG');
      const observed = cluster.databaseObservation || {};
      const agreement = { aligned: '角色一致', mismatch: '角色观测不同步', unverified: '后端身份或角色待确认' };
      const role = key => roleLabels[key] || '未知';
      card.append(el('p', `${cluster.namespace}/${cluster.name} · UID ${cluster.uid}`, 'monitor-note'), stats([
        ['阶段', cluster.phase || '未知'], ['当前主库', cluster.currentPrimary || '未知'], ['目标主库', cluster.targetPrimary || '未知'],
        ['实例就绪', `${cluster.readyInstances ?? '—'} / ${cluster.instanceCount ?? '—'}`],
        ['PostgreSQL 实测', `${role(observed.pgRole)}${observed.pod ? ` · ${observed.pod}` : ''}`]
      ]));
      card.append(el('p', [agreement[observed.status] || agreement.unverified,
        observed.operatorRole && `控制器报告 ${role(observed.operatorRole)}`,
        observed.matchesMappedPod === false && '连接命中其他成员',
        cluster.readWriteService && `读写 ${cluster.readWriteService}`,
        cluster.readService && `读服务 ${cluster.readService}`].filter(Boolean).join(' · '),
      observed.status === 'mismatch' ? 'monitor-warning' : 'monitor-note'));
      if (cluster.phaseReason) card.append(el('p', cluster.phaseReason, 'monitor-note'));
      if (cluster.members?.length) card.append(table(['实例', 'Pod UID', '地址', '控制器角色标签', '就绪'], cluster.members.map(member => [
        member.name, member.uid, member.address, member.roleLabel || '—', member.deleting ? '终止中' :
          member.ready === 'True' ? '就绪' : member.ready === 'False' ? '未就绪' : '未知'
      ]), 'CloudNativePG 成员'));
      if (cluster.conditions?.length) card.append(table(['条件', '状态', '原因', '观测时间'], cluster.conditions.map(condition => [
        condition.type, condition.status, [condition.reason, condition.message].filter(Boolean).join(' · '), formatTime(condition.changedAt)
      ]), 'CloudNativePG 条件'));
      const backup = cluster.backup || {};
      card.append(el('p', backup.status === 'observed' ? [
        backup.lastSuccessfulBackup && `最近备份成功 ${formatTime(backup.lastSuccessfulBackup)}`,
        backup.lastFailedBackup && `最近备份失败 ${formatTime(backup.lastFailedBackup)}`,
        backup.firstRecoverabilityPoint && `最早可恢复 ${formatTime(backup.firstRecoverabilityPoint)}`
      ].filter(Boolean).join(' · ') : 'Cluster 状态未报告可读取的备份时间。', 'monitor-note'));
      const history = changes.filter(row => row.cloudNativePG).slice(0, 12);
      if (history.length) card.append(table(['观测时间', '阶段', '当前 / 目标主库', '数据库角色核对', '采样'], history.map(row => {
        const state = row.cloudNativePG;
        return [formatTime(row.collectedAt), state.phase, `${state.currentPrimary || '未知'} / ${state.targetPrimary || '未知'}`,
          agreement[state.observation?.status] || agreement.unverified, sampleLink(asset.id, row.id)];
      }), 'CloudNativePG 切换历史'));
      return card;
    }

    function kubernetesIdentityPanel(asset, snapshot, changes) {
      const card = panel('Workload、Pod、节点和服务端点');
      const kube = snapshot?.k8s;
      const owner = kube?.owner;
      const workload = kube?.workload;
      const pod = kube?.pod;
      const node = kube?.node;
      if (kube?.cloudNativePG) card.append(cloudNativePGPanel(asset, kube.cloudNativePG, changes));
      const named = item => item && [item.kind, item.name].filter(Boolean).join('/');
      const endpoints = routes => Object.entries(routes?.slices || {}).map(([name, slice]) => {
        const conditions = slice.endpoint.conditions || {};
        const state = (flag, yes, no) => conditions[flag] === true ? yes : conditions[flag] === false ? no : '未报告';
        return [slice.service, name, slice.uid, slice.endpoint.targetUID, (slice.endpoint.addresses || []).join(', '),
          state('ready','就绪','未就绪'), state('serving','服务中','停止服务'), state('terminating','终止中','运行中')];
      });
      const routeSummary = routes => {
        if (!routes) return '—';
        if (routes.status === 'unavailable') return routes.reason || 'EndpointSlice 读取受限';
        if (routes.status === 'not_applicable') return '未发现匹配的 Service';
        return [routes.truncated ? '读取范围受限' : '', routes.identityMismatches ? `已排除 ${routes.identityMismatches} 条过期身份` : ''].filter(Boolean).join(' · ');
      };
      if (pod) card.append(table(['对象', '名称', 'UID', '状态'], [
        ['Workload', named(workload), workload?.uid, workload ? '按所有者 UID 验证' : '未配置工作负载映射'],
        ['控制器', named(owner), owner?.uid, 'Pod ownerReference'],
        ['Pod', `${kube.namespace}/${pod.name}`, pod.uid, [pod.phase, pod.reason, pod.deletingAt && '终止中'].filter(Boolean).join(' · ')],
        ['Node', node?.name || pod.node, node?.uid, kube.nodeError || (node ? '已观测' : '尚未调度或未知')]
      ], 'Kubernetes 对象身份'));
      else if (kube?.reason) card.append(el('p', kube.reason, 'monitor-warning'));
      const currentRoutes = endpoints(kube?.endpointRoutes);
      if (currentRoutes.length) card.append(table(['Service', 'EndpointSlice', '切片 UID', '目标 Pod UID', '地址', '就绪', '服务', '终止'],
        currentRoutes, '当前 EndpointSlice'));
      const routeNote = routeSummary(kube?.endpointRoutes);
      if (kube?.endpointRoutes && (routeNote || !currentRoutes.length))
        card.append(el('p', routeNote || '未观察到指向当前 Pod UID 的端点。', 'monitor-note'));
      if (kube?.pvcs?.length) card.append(table(['PVC', 'PVC UID', '状态', 'PV', 'PV UID', '绑定证据'], kube.pvcs.map(pvc => [
        pvc.name, pvc.uid, pvc.phase || '未知', pvc.volume?.name, pvc.volume?.uid, pvc.error || pvc.volumeError ||
          (pvc.volume?.claimUID ? '已核对 claimRef UID' : '待绑定或证据不足')
      ]), 'PVC 与 PV 绑定'));
      if (kube?.resourceSpecs && Object.keys(kube.resourceSpecs).length) {
        const usage = new Map((kube.resourceMetrics?.containers || []).map(row => [row.name, row]));
        card.append(table(['容器', 'CPU 用量 / 请求 / 限额（核）', '内存用量 / 请求 / 限额'],
          Object.entries(kube.resourceSpecs).map(([name, config]) => [
            name, [usage.get(name)?.cpuCores, config.requests.cpuCores, config.limits.cpuCores].map(value => display(value)).join(' / '),
            [usage.get(name)?.memoryBytes, config.requests.memoryBytes, config.limits.memoryBytes].map(value => display(value, bytes)).join(' / ')
          ]), '容器规格与用量'));
      }
      const changed = changes.filter(row => row.routesChanged || row.volumes || row.events?.length);
      if (changed.length) card.append(table(['观察时间', 'Pod UID', 'EndpointSlice', 'PVC 身份与容量', '对象事件', '采样'], changed.map(row => {
        const routes = endpoints(row.endpointRoutes).map(([service, name, , target, addresses, ready, serving, terminating]) =>
          [service, name, target, addresses, ready, serving, terminating].filter(Boolean).join(' · '));
        const volumes = Object.entries(row.volumes || {}).map(([name, [uid, capacity, requested, phase, error]]) =>
          [name, uid, capacity, requested, phase || row.pvcs?.[name], error].filter(Boolean).join(' · '));
        const recent = (row.events || []).slice(0, 3).map(event => [event.reason, formatTime(event.at), event.message].filter(Boolean).join(' · '));
        return [formatTime(row.collectedAt), row.uid, [...routes, routeSummary(row.endpointRoutes)].filter(Boolean).join(' / '),
          volumes.join(' / '), recent.join(' / '), sampleLink(asset.id, row.id)];
      }), '环境身份与端点历史'));
      return card;
    }

    function followWait(item) {
      if (group === 'type') { waitType = item.waitType; waitEvent = ''; group = 'event'; }
      else if (group === 'event') { waitType = item.waitType; waitEvent = item.waitEvent; group = 'query'; }
      else if (item.queryId) { openQuery(item.queryId); return; }
      change();
    }

    function instanceIndicators(asset, history, snapshot, time, events) {
      const result = document.createDocumentFragment();
      const major = Math.floor(asset.versionNum / 10000);
      const sampled = history.series.reduce((total, row) => total + row.samples, 0);
      const visibleSamples = history.series.reduce((total, row) => total + row.observed, 0);
      const read = el('div', null, 'monitor-wait-controls');
      read.append(el('span', `${visibleSamples}/${sampled} 次 PG 采样可用${snapshot ? ` · 区间末次 ${formatTime(snapshot.collectedAt)}` : ''}`, 'monitor-note'));
      if (snapshot) read.append(sampleLink(asset.id, snapshot.id));
      result.append(read);
      const points = history.series.map(row => {
        const hit = row.values.blocksHit, loaded = row.values.blocksRead;
        return { bucket: row.bucket, ...row.values,
          cacheRatio: finite(hit) && finite(loaded) && hit + loaded > 0 ? hit / (hit + loaded) * 100 : null };
      });
      const formats = { count: number, percent, rate, bytes, bytesRate: value => `${bytes(value)}/s`,
        ms: milliseconds, blocks: value => `${number(value, 0)} 块` };
      for (const [key, label, scope] of [
        ['sessions', '连接与等待', '客户端会话按实例采样。阻塞计数来自纳管连接库。'],
        ['transactions', '事务与死锁', '事务与死锁来自纳管库。计数重置、重启与缺采切断增量。'],
        ['io', '读写与缓存', '读块与缓存来自纳管库。实例级客户端关系 I/O 从 PG16 起按可用性显示。'],
        ['wal', 'WAL 与检查点', 'WAL 和检查点来自实例。角色或 WAL 时间线变化时切断增量。']
      ]) {
        const section = el('details', null, 'monitor-panel monitor-topic instance-topic');
        section.append(el('summary', label));
        const draw = () => {
          const definitions = history.metrics.filter(metric => metric.group === key && (!metric.minMajor || major >= metric.minMajor));
          if (key === 'io') definitions.unshift({ key: 'cacheRatio', label: '纳管库读块命中率', unit: 'percent', source: 'pg_stat_database' });
          const specs = definitions.map((item, i) => [item.key, item.label, formats[item.unit], palette[i % palette.length]]);
          const grid = metricCharts(points, time, events, specs);
          definitions.forEach((item, index) => {
            const source = item.source === 'checkpoint' ? (major >= 17 ? 'pg_stat_checkpointer' : major ? 'pg_stat_bgwriter' : 'PG 版本待确认') : item.source;
            grid.children[index].append(el('p', `${source} · ${item.mode === 'rate' ? '按区间时间折算' : item.mode === 'delta' ? '区间增量' : '时间桶均值'}`, 'monitor-note'));
          });
          section.append(el('p', scope, 'monitor-note'), grid);
        };
        if (key === 'sessions') { section.open = true; draw(); }
        else section.addEventListener('toggle', () => { if (section.open && section.children.length === 1) draw(); });
        result.append(section);
      }
      return result;
    }

    async function load(asset, time) {
      const key = ++request;
      if (mode !== 'changes' && time.to - time.from > 10080 * minute) {
        body.replaceChildren(el('p', '正在读取小时汇总…', 'monitor-note'));
        try { await loadLongRange(asset, time, key); }
        catch (error) { if (key === request) body.replaceChildren(el('p', error.message, 'monitor-warning')); }
        return;
      }
      const path = `/api/assets/${asset.id}/`;
      const query = new URLSearchParams(time.params);
      let section;
      if (mode === 'replication') section = 'series';
      if (mode === 'metrics') section = 'instance-history';
      if (mode === 'changes') section = 'changes';
      if (mode === 'backup') {
        section = 'backup';
        if (focusSample) query.set('sample', focusSample);
      }
      if (mode === 'infra' && asset.runtime !== 'managed') {
        section = 'environment';
        if (asset.runtime === 'linux' && deviceType && deviceName) {
          query.set('deviceType', deviceType);
          query.set('device', deviceName);
        }
      }
      if (mode === 'locks') {
        section = 'lock-history';
        if (lockPick || focusSample) query.set('sample', lockPick || focusSample);
      }
      if (mode === 'waits') {
        section = 'wait-history';
        query.set('group', group);
        query.set('waitType', waitType);
        query.set('waitEvent', waitEvent);
      }
      if (mode === 'sql') {
        section = 'sql-window';
        query.set('order', sqlOrder);
        if (queryId) query.set('queryId', queryId);
      }
      if (mode === 'catalog') {
        section = 'catalog';
        if (dbOid) query.set('dbOid', dbOid);
        if (objectOid) { query.set('objectOid', objectOid); query.set('objectKind', objectKind); }
      }
      const contextParams = new URLSearchParams(time.params);
      if (['metrics', 'waits', 'locks'].includes(mode)) contextParams.set('blocking', 'true');
      if (mode === 'changes') contextParams.set('changes', 'true');
      const linkedSQL = mode === 'sql' && queryId;
      const topologyQuery = new URLSearchParams(time.params);
      const topologyAt = replicaAt;
      if (topologyAt != null && topologyAt >= time.from && topologyAt < time.to)
        topologyQuery.set('at', new Date(topologyAt).toISOString());
      const replicationQuery = new URLSearchParams(topologyQuery);
      if (focusSample) replicationQuery.set('sample', focusSample);
      const peers = mode === 'replication' ? assets.filter(row => row.id !== asset.id && clusterKey(row) === clusterKey(asset)).slice(0, 4) : [];
      const range = mode === 'sql' && baseline ? baselineWindow(time) : null;
      const baselineParams = new URLSearchParams(query);
      if (range) {
        baselineParams.set('from', new Date(range.from).toISOString());
        baselineParams.set('to', new Date(range.to).toISOString());
      }
      body.setAttribute('aria-busy', 'true');
      body.replaceChildren(el('p', '正在读取所选时间范围…', 'monitor-note'));
      try {
        const showWaitCaptures = ['waits', 'locks'].includes(mode);
        const [context, data, waits, slow, plans, replication, prior, traces, topology, captures, capture] = await Promise.all([
          api(`${path}monitoring-context?${contextParams}`), section ? api(`${path}${section}?${query}`) : null,
          mode === 'metrics' ? api(`${path}wait-history?${time.params}&group=type`).catch(() => null) : null,
          linkedSQL ? api(`${path}slow-logs?${time.params}&queryId=${encodeURIComponent(queryId)}`).catch(() => null) : null,
          linkedSQL ? api(`${path}sql-plans?queryId=${encodeURIComponent(queryId)}`).catch(() => null) : null,
          mode === 'replication' ? api(`${path}replication?${replicationQuery}`) : null,
          range ? api(`${path}sql-window?${baselineParams}`).catch(() => null) : null,
          linkedSQL ? api(`${path}query-traces?${time.params}&queryId=${encodeURIComponent(queryId)}`).catch(() => null) : null,
          Promise.all(peers.map(peer => api(`/api/assets/${peer.id}/replication?${topologyQuery}&view=topology`)
            .then(value => ({ asset: peer, observations: value.topology })).catch(() => ({ asset: peer, observations: [] })))),
          showWaitCaptures ? api(`${path}wait-captures`).catch(() => null) : null,
          showWaitCaptures && waitCapturePick ? api(`${path}wait-captures?id=${encodeURIComponent(waitCapturePick)}`).catch(() => null) : null
        ]);
        if (key !== request || !visible) return;
        const events = context.events.map(event => ({ ...event, assetName: asset.name }));
        time.cursors.clear();
        const fragment = document.createDocumentFragment();
        if (eventAnchor) {
          const context = el('div', null, 'monitor-wait-controls');
          context.append(button('返回原事件', () => openEvent(eventAnchor), 'monitor-link'));
          if (focusAt != null) context.append(el('span', `证据时刻 ${formatTime(focusAt)}`, 'monitor-note'));
          if (focusSample) context.append(sampleLink(asset.id, focusSample));
          fragment.append(context);
        }
        if (mode === 'metrics') {
          const latest = context.snapshot;
          const current = data.series.at(-1)?.values;
          const strip = stats([
            ['近时桶活跃会话', display(current?.activeSessions), '', () => { resetWaitFilters(); mode = 'waits'; change(); }],
            ['末次采样的阻塞会话', display(latest?.blocked?.data?.blockedSessions), '', () => { mode = 'locks'; change(); }],
            ['近时桶连接占用', display(current?.connectionUsage, percent)],
            asset.versionNum >= 160000 ? ['近时桶实例读操作', display(current?.ioReads, rate)] : ['近时桶纳管库读块增量', display(current?.blocksRead)]
          ]);
          strip.classList.add('activity-stats');
          fragment.append(strip);
          if (waits?.buckets.some(bucket => bucket.observed > 0)) {
            fragment.append(waitOverview(waits.buckets, time, events, 'type', item => {
              resetWaitFilters(); mode = 'waits'; followWait(item);
            }, context));
          } else {
            const rates = data.series.map(row => ({ bucket: row.bucket, tps: finite(row.values.commits) && finite(row.values.rollbacks)
              ? row.values.commits + row.values.rollbacks : null }));
            const load = metricCharts(rates, time, events, [
              ['tps', '数据库负载 · 事务吞吐', rate, palette[0]]
            ]).firstElementChild;
            load.classList.add('monitor-focus-chart');
            load.append(el('p', '此时段缺少等待采样。可在等待分析查看采集覆盖。', 'monitor-note'));
            const split = el('div', null, 'monitor-split');
            split.append(load, eventList(events, '此实例的问题事件')); fragment.append(split);
          }
          fragment.append(instanceIndicators(asset, data, latest, time, events));
        }
        if (mode === 'waits') {
          const filter = el('div', null, 'monitor-wait-controls');
          const dimension = select('等待分组', value => { group = value; waitType = ''; waitEvent = ''; change(); });
          options(dimension, [['type', '等待类型'], ['event', '具体等待事件'], ['query', 'Query ID']], group);
          filter.append(labeled('分组方式', dimension));
          if (lockAt != null) filter.append(el('span', `锁链定位 · ${formatTime(lockAt)}`, 'monitor-note'));
          if (waitType) filter.append(button(`返回上一级 · ${waitType}${waitEvent ? '/' + waitEvent : ''}`, () => {
            if (waitEvent) { waitEvent = ''; group = 'event'; }
            else { resetWaitFilters(); }
            change();
          }));
          fragment.append(filter, waitOverview(data.buckets, time, events, group, followWait, context));
        }
        if (mode === 'sql') fragment.append(rankSQL(data.performance, { ...slow, ...plans, ...traces, database: asset.database, baseline: prior?.performance }, path, time));
        if (mode === 'locks') fragment.append(lockPanel(asset, data, time));
        if (showWaitCaptures && captures) fragment.append(waitCapturePanel(asset, captures.captures || [], capture, events));
        if (mode === 'replication') fragment.append(replicationPanel(asset, replication, data.series, time, events,
          [{ asset, observations: replication.topology }, ...topology]));
        if (mode === 'catalog') fragment.append(catalogPanel(asset, data, time, events));
        if (mode === 'backup') fragment.append(backupPanel(asset, data));
        if (mode === 'infra') fragment.append(environmentPanel(asset, context.snapshot, data, time, events));
        if (mode === 'changes') fragment.append(changeTimelinePanel(asset, data, time, events));
        if ((mode !== 'metrics' && mode !== 'changes') || waits?.buckets.some(bucket => bucket.observed > 0))
          fragment.append(eventList(events, '此实例的问题事件'));
        body.replaceChildren(fragment);
        const marker = mode === 'waits' && lockAt != null ? lockAt : focusAt;
        if (marker != null && marker >= time.from && marker < time.to) {
          const index = Math.floor((marker - time.first) / time.bucketMs);
          for (const cursor of time.cursors) cursor(index);
        }
      } catch (error) {
        if (key === request && visible) body.replaceChildren(el('p', `读取监控数据失败 · ${error.message}`, 'monitor-warning'));
      } finally {
        if (key === request) body.removeAttribute('aria-busy');
      }
    }

    function update() {
      if (!visible || !ready) return;
      header.hidden = !enabled;
      if (!enabled) {
        details.hidden = true;
        fleet.hidden = false;
        overview.replaceChildren(el('p', '监控工作台使用 Go 版和 PostgreSQL 18 长期存储。', 'monitor-note'));
        charts.replaceChildren();
        return;
      }
      const clusters = new Map(assets.map(asset => [clusterKey(asset), clusterLabel(asset)]));
      if (cluster && !clusters.has(cluster)) { cluster = ''; chooseCluster = true; }
      if (chooseCluster && clusters.size) {
        let previous;
        try { previous = localStorage.getItem('dbsphere.lastCluster'); } catch {}
        cluster = clusters.has(previous) ? previous : [...clusters].sort((a, b) => a[1].localeCompare(b[1], 'zh-CN'))[0][0];
        chooseCluster = false;
        if (/^#monitoring(?:\?|$)/.test(location.hash)) history.replaceState(null, '', `#${route()}`);
      }
      const available = assets.filter(asset => !cluster || clusterKey(asset) === cluster);
      const dbs = [...new Set(available.map(asset => asset.database))].filter(Boolean).sort();
      if (!dbs.includes(database)) database = '';
      const list = members();
      const asset = list.find(asset => asset.id === instance);
      if (!asset) instance = '';
      options(clusterSelect, [['', '全部集群'], ...[...clusters].sort((a, b) => a[1].localeCompare(b[1], 'zh-CN'))], cluster);
      options(databaseSelect, [['', '全部数据库'], ...dbs.map(name => [name, name])], database);
      options(instanceSelect, [['', '集群总览'], ...list.map(item => [item.id, item.name])], instance);
      updatePeriodOptions();
      const time = activeWindow ??= windowFor(period, fixed);
      time.openEvent = openEvent;
      time.zoom = zoom;
      const caption = `${formatTime(time.from)} 至 ${formatTime(time.to)} · ${fixed ? '固定范围' : '滚动范围'}`;
      fleet.hidden = !!asset;
      details.hidden = !asset;
      if (asset) {
        instanceTitle.textContent = asset.name;
        timeLabel.textContent = [clusterLabel(asset), asset.database, roleLabels[asset.role] || '角色未知', assetHealth(asset).label, caption].join(' · ');
        for (const [i, item] of [...tablist.children].entries()) item.setAttribute('aria-pressed', String(tabs[i][0] === mode));
        void load(asset, time);
      } else {
        fleetTime = time;
        time.openEvent = id => openEvent(id, true, route(time));
        fleetLabel.textContent = caption;
        renderFleet(time);
        void compareTrends(time);
      }
    }

    return {
      sync(nextAssets, available, events) {
        const first = !ready;
        assets = nextAssets;
        recent = events;
        ready = true;
        enabled = available;
        if (visible) {
          if (first || (!fixed && !root.contains(document.activeElement))) {
            if (!fixed) activeWindow = null;
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
      route, restore, openInstance, openCluster,
      openIncident(event, topic = 'waits', point = null) {
        const asset = assets.find(item => item.id === event.assetId);
        if (!asset) return;
        const start = Date.parse(event.startedAt);
        const selectedAt = Date.parse(point?.sampleAt || point?.at);
        let to = Math.min(Date.now(), event.endedAt ? Date.parse(event.endedAt) + 15 * minute : Date.now());
        let from = Math.min(to - minute, Math.max(to - 10080 * minute, start - 15 * minute));
        if (Number.isFinite(selectedAt) && (selectedAt < from || selectedAt >= to)) {
          to = Math.min(Date.now(), selectedAt + 15 * minute);
          from = selectedAt - 15 * minute;
        }
        fixed = validRange(from, to) ? { from, to } : null;
        activeWindow = null;
        if (cluster !== clusterKey(asset)) resetFleet();
        cluster = clusterKey(asset);
        chooseCluster = false;
        database = asset.database;
        instance = asset.id;
        mode = tabs.some(([name]) => name === topic) ? topic : 'waits';
        eventAnchor = event.id;
        changePick = point?.changeId || '';
        resetWaitFilters();
        resetCatalog();
        focusAt = Number.isFinite(selectedAt) ? selectedAt : null;
        replicaAt = mode === 'replication' && focusAt != null ? Math.min(to - 1, focusAt + 1) : null;
        focusSample = point?.sampleId || '';
        waitCapturePick = point?.waitCaptureId || '';
      }
    };
  }

  window.DbSphereMonitoring = { create };
})();
