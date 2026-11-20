(() => {
  'use strict';

  const minute = 60_000;
  const periods = [
    ['15 分钟', 15, 1], ['1 小时', 60, 1], ['6 小时', 360, 5], ['24 小时', 1440, 15], ['7 天', 10080, 60]
  ];
  const tabs = [['metrics', '实例概览'], ['waits', '等待分析'], ['sql', 'SQL 性能'], ['locks', '锁与会话'], ['infra', '运行环境']];
  const defaultPeriod = '1 小时';
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

  function graph(title, window, buckets, events, format, stacked = false) {
    const card = panel(title);
    const frame = el('div', null, 'monitor-plot');
    const width = 600, height = 154, left = 92, right = 19, top = 13, bottom = 126;
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
    for (const event of events) {
      const started = Date.parse(event.startedAt);
      if (started < window.from || started > window.to) continue;
      const position = left + (started - window.first) / (window.bucketMs * pointCount) * area;
      const mark = svg('line', { x1: position, x2: position, y1: top, y2: bottom, class: 'monitor-event-marker' });
      const label = svg('title');
      label.textContent = event.title;
      mark.append(label);
      const marker = svg('g', { class: 'monitor-mark' });
      marker.append(mark, svg('circle', { cx: position, cy: top, r: 4, fill: 'var(--issue-line)' }));
      marker.addEventListener('click', () => window.openEvent(event.id));
      marks.push(marker);
    }
    const previous = new Map();
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
          if (last) chart.append(svg('line', { x1: last.x, x2: cx, y1: last.y, y2: cy, stroke: value.color, class: 'monitor-trendline' }));
          chart.append(svg('circle', { cx, cy, r: 2.3, fill: value.color }));
          next.set(key, { x: cx, y: cy });
        }
        previous.clear();
        for (const [label, location] of next) previous.set(label, location);
      }
    });
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
    const minutes = fixed ? [1, 5, 15, 60].find(size => to - from <= size * minute * 380) : bin;
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
    const { api, assetHealth, assetRuntimeLabel, formatTime, openEvent, openAssets, openPlans, renderSqlPerformance } = actions;
    let assets = [], summaries = new Map(), recent = [], ready = false, enabled = false, visible = false;
    let cluster = '', instance = '', database = '', period = defaultPeriod, fixed = null, mode = 'metrics', eventAnchor = '';
    let group = 'type', waitType = '', waitEvent = '', queryId = '', request = 0, fleetRequest = 0;
    let compare = null, abnormalOnly = false, fleetTime = null;
    const header = el('div', null, 'monitor-controls');
    const fleet = el('div', null, 'monitor-fleet');
    const charts = el('div', null, 'monitor-layout');
    const details = el('section', null, 'monitor-instance');
    const tablist = el('div', null, 'monitor-tabs');
    tablist.setAttribute('aria-label', '实例监控专题');
    const body = el('div', null, 'monitor-content');
    const instanceTitle = el('h2', null, 'monitor-instance-title');
    const timeLabel = el('div', null, 'monitor-range-caption');
    let activeWindow = null;
    const fleetLabel = el('p', null, 'monitor-range-caption');
    const clusterSelect = select('PG 集群', value => { cluster = value; instance = ''; database = ''; resetFleet(); change(); });
    const instanceSelect = select('数据库实例', value => { if (value) openInstance(value); else backToFleet(); });
    const databaseSelect = select('数据库', value => { database = value; instance = ''; resetFleet(); change(); });
    const rangeSelect = select('时间范围', value => {
      if (value === 'custom') {
        openRange();
        return;
      }
      period = value;
      fixed = null;
      activeWindow = null;
      eventAnchor = '';
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
        errorLabel.textContent = '请选择至少 1 分钟、最多 7 天且结束时间不晚于当前的范围。';
        return;
      }
      zoom(Date.parse(fromField.value), Date.parse(toField.value));
    });
    const editRange = button('编辑时段', openRange);
    header.append(labeled('PG 集群', clusterSelect), labeled('实例', instanceSelect), labeled('数据库', databaseSelect),
      labeled('时间范围', rangeSelect), editRange, button('刷新', () => {
        if (!fixed) activeWindow = null;
        update();
        void refreshFleet();
      }), rangeForm);
    for (const [key, label] of tabs) tablist.append(button(label, () => { mode = key; if (key === 'sql') queryId = ''; change(); }));
    details.append(button('← 集群总览', backToFleet, 'monitor-link monitor-breadcrumb'), instanceTitle, timeLabel, tablist, body);
    root.append(header, fleet, details);

    function validRange(from, to) {
      return Number.isFinite(from) && Number.isFinite(to) && to <= Date.now() && to - from >= minute && to - from <= 10080 * minute;
    }

    function route() {
      const params = new URLSearchParams();
      for (const [key, value, fallback] of [
        ['cluster', cluster, ''], ['instance', instance, ''], ['database', database, ''],
        ['period', period, defaultPeriod], ['tab', mode, 'metrics'], ['group', group, 'type'],
        ['waitType', waitType, ''], ['waitEvent', waitEvent, ''], ['queryId', queryId, ''], ['event', eventAnchor, '']
      ]) if (value && value !== fallback) params.set(key, value);
      if (fixed) { params.set('from', new Date(fixed.from).toISOString()); params.set('to', new Date(fixed.to).toISOString()); }
      return params.size ? `monitoring?${params}` : 'monitoring';
    }

    function restore(params) {
      cluster = params.get('cluster') || '';
      instance = params.get('instance') || '';
      database = params.get('database') || '';
      period = periods.find(([name]) => params.get('period') === name)?.[0] || defaultPeriod;
      mode = tabs.find(([name]) => params.get('tab') === name)?.[0] || 'metrics';
      group = ['event', 'query'].includes(params.get('group')) ? params.get('group') : 'type';
      waitType = (params.get('waitType') || '').slice(0, 64);
      waitEvent = (params.get('waitEvent') || '').slice(0, 128);
      queryId = /^-?\d{1,20}$/.test(params.get('queryId') || '') ? params.get('queryId') : '';
      eventAnchor = /^[0-9a-f-]{36}$/.test(params.get('event') || '') ? params.get('event') : '';
      const from = Date.parse(params.get('from'));
      const to = Date.parse(params.get('to'));
      fixed = validRange(from, to) ? { from, to } : null;
      activeWindow = null;
      rangeForm.hidden = true;
      resetFleet();
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
        const local = new Date(value - new Date(value).getTimezoneOffset() * minute);
        input.value = local.toISOString().slice(0, 16);
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

    function resetFleet() { compare = null; abnormalOnly = false; }
    function resetWaitFilters() { group = 'type'; waitType = ''; waitEvent = ''; queryId = ''; }

    function openInstance(id) {
      const asset = assets.find(item => item.id === id);
      if (!asset) return;
      if (id !== instance) {
        resetWaitFilters();
        mode = 'metrics';
        eventAnchor = '';
      }
      cluster = clusterKey(asset);
      if (database && database !== asset.database) database = '';
      instance = id;
      change();
    }

    function backToFleet() { instance = ''; eventAnchor = ''; resetWaitFilters(); change(); }

    function health(asset) {
      const status = assetHealth(asset);
      return el('span', status.label, `monitor-state monitor-state-${status.kind}`);
    }

    function connection(asset) {
      const values = summaries.get(asset.id);
      return finite(values?.connections) && values.maxConnections > 0 ? values.connections * 100 / values.maxConnections : null;
    }

    function eventList(events, title) {
      const card = panel(title);
      if (eventAnchor && !events.some(event => event.id === eventAnchor))
        card.append(button('返回事件证据', () => openEvent(eventAnchor)));
      if (!events.length) card.append(el('p', '当前范围内没有关联事件。', 'monitor-empty'));
      for (const event of events) {
        const item = button(event.title, () => openEvent(event.id), 'monitor-event');
        item.append(el('small', [event.status === 'open' ? '处理中' : '已恢复', event.assetName,
          formatTime(event.startedAt), event.id.slice(0, 8)].filter(Boolean).join(' · ')));
        if (event.id === eventAnchor) item.setAttribute('aria-current', 'true');
        card.append(item);
      }
      return card;
    }

    function renderFleet(time) {
      if (!time || instance) return;
      const rank = { error: 0, stale: 1, pending: 2, manual: 3, ok: 4 };
      const items = members().sort((a, b) => rank[assetHealth(a).kind] - rank[assetHealth(b).kind] ||
        (summaries.get(b.id)?.openEvents ?? 0) - (summaries.get(a.id)?.openEvents ?? 0) ||
        Number(b.role === 'primary') - Number(a.role === 'primary') || a.name.localeCompare(b.name, 'zh-CN'));
      const roles = items.filter(asset => ['ok', 'manual'].includes(assetHealth(asset).kind));
      const abnormal = items.length - roles.length;
      const open = items.reduce((sum, asset) => sum + (summaries.get(asset.id)?.openEvents ?? 0), 0);
      const peak = items.filter(asset => finite(connection(asset))).sort((a, b) => connection(b) - connection(a))[0];
      const lag = items.filter(asset => finite(summaries.get(asset.id)?.replicationLagBytes)).sort(
        (a, b) => summaries.get(b.id).replicationLagBytes - summaries.get(a.id).replicationLagBytes)[0];
      const split = el('div', null, 'monitor-split');
      const card = panel(abnormalOnly ? '异常成员' : '成员健康');
      card.classList.add('monitor-members');
      card.append(el('p', '最新采样 · 异常成员优先', 'monitor-note'));
      const ids = new Set(items.map(asset => asset.id));
      const matching = recent.filter(event => ids.has(event.assetId));
      const active = matching.filter(event => event.status === 'open');
      const resolved = matching.filter(event => event.status !== 'open' && Date.parse(event.startedAt) < time.to && Date.parse(event.endedAt) >= time.from);
      const issues = eventList((active.length ? active : resolved).slice(0, 5), active.length ? '当前事件' : '所选区间最近恢复');
      issues.classList.add('monitor-latest');
      if (open > active.length) issues.append(el('p', '这里展示最近返回的事件。其余可在问题事件中查找。', 'monitor-note'));
      const statsBar = stats([
        ['异常实例', abnormal, abnormal ? 'monitor-alert' : '', () => {
          abnormalOnly = !abnormalOnly;
          renderFleet(time);
          fleet.querySelector('.monitor-stat')?.focus({ preventScroll: true });
        }],
        ['开放事件', items.length && items.every(asset => summaries.has(asset.id)) ? open : '—', open ? 'monitor-alert' : '', () => issues.scrollIntoView({ block: 'nearest' })],
        ['最近主库 / 备库', `${roles.filter(a => a.role === 'primary').length} / ${roles.filter(a => a.role === 'standby').length}`],
        ['最高连接占用', display(peak && connection(peak), percent), '', peak && (() => openInstance(peak.id))],
        ['最大回放差距', display(lag && summaries.get(lag.id).replicationLagBytes, bytes), '', lag && (() => openInstance(lag.id))]
      ]);
      statsBar.firstElementChild.setAttribute('aria-pressed', String(abnormalOnly));
      const list = abnormalOnly ? items.filter(asset => !roles.includes(asset)) : items;
      const rows = list.map(asset => {
        const values = summaries.get(asset.id);
        const conn = display(connection(asset), percent);
        const replay = display(values?.replicationLagBytes, bytes);
        const name = el('div');
        name.append(button(asset.name, () => openInstance(asset.id), 'monitor-link'));
        const mobile = el('details', null, 'monitor-mobile-detail');
        mobile.append(el('summary', '采样详情'), el('small', `${formatTime(asset.lastSampleAt)} · 连接 ${conn} · 回放 ${replay}`));
        name.append(mobile);
        return [name, roleLabels[asset.role] || '未知', health(asset), formatTime(asset.lastSampleAt), conn, replay, assetRuntimeLabel(asset)];
      });
      if (list.length) card.append(table(['实例', '角色', '采集状态', '最近采样', '连接占用', '回放差距', '环境'], rows, '集群成员'));
      else if (!items.length) card.append(el('p', '没有匹配的纳管实例。', 'monitor-empty'), button('查看资产', openAssets));
      else card.append(el('p', '当前没有异常成员。', 'monitor-empty'));
      split.append(card, issues);
      fleet.replaceChildren(statsBar, split, fleetLabel, charts);
    }

    async function refreshFleet() {
      if (!visible || !enabled) return;
      const key = ++fleetRequest;
      try {
        const payload = await api('/api/monitoring/instances');
        if (key !== fleetRequest || !visible) return;
        summaries = new Map(payload.instances.map(item => [item.assetId, item]));
        renderFleet(fleetTime);
      } catch (error) {
        if (key === fleetRequest && visible && !instance) fleetLabel.textContent = `实例摘要暂不可用 · ${error.message}`;
      }
    }

    async function compareTrends(time) {
      const key = ++request;
      const items = members();
      compare ??= new Set([...items].sort((a, b) => Number(b.role === 'primary') - Number(a.role === 'primary') ||
        Number(assetHealth(b).kind === 'error') - Number(assetHealth(a).kind === 'error')).slice(0, 3).map(asset => asset.id));
      const selected = items.filter(asset => compare.has(asset.id));
      const colors = new Map(selected.map((asset, i) => [asset.id, palette[i]]));
      if (!items.length) { charts.replaceChildren(); return; }
      const picker = panel('集群趋势');
      const controls = el('div', null, 'monitor-selection');
      picker.append(el('p', '比较最多 5 个实例，每条曲线对应一个实例。', 'monitor-note'), controls);
      for (const asset of items) {
        const checkbox = el('input');
        checkbox.type = 'checkbox';
        checkbox.checked = compare.has(asset.id);
        checkbox.disabled = compare.size >= 5 && !checkbox.checked;
        checkbox.addEventListener('change', () => {
          if (checkbox.checked) compare.add(asset.id); else compare.delete(asset.id);
          void compareTrends(time);
          [...charts.querySelectorAll('input')][items.indexOf(asset)]?.focus({ preventScroll: true });
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
      charts.replaceChildren(picker);
      if (!selected.length) { picker.append(el('p', '选择实例以比较趋势。', 'monitor-empty')); return; }
      const loading = el('p', '正在读取集群趋势…', 'monitor-note');
      picker.append(loading);
      const results = await Promise.allSettled(selected.map(asset => api(`/api/assets/${asset.id}/series?${time.params}`)));
      if (key !== request || !visible || instance) return;
      time.cursors.clear();
      loading.remove();
      const failures = results.flatMap((result, i) => result.status === 'rejected' ? [selected[i].name] : []);
      if (failures.length) picker.append(el('p', `趋势读取失败 · ${failures.join('、')}`, 'monitor-warning'));
      const ids = new Set(selected.map(asset => asset.id));
      const events = recent.filter(event => ids.has(event.assetId));
      const panels = el('div', null, 'monitor-charts');
      for (const [metric, title, format] of [
        ['transactionsPerSecond', '事务吞吐', rate], ['averageReplicationLagBytes', '复制回放差距', bytes]
      ]) {
        const points = Array.from({ length: time.count }, () => ({ values: [] }));
        for (const [index, result] of results.entries()) {
          if (result.status !== 'fulfilled') continue;
          const { id: assetId, name: label } = selected[index];
          for (const [i, point] of aligned(result.value.series, time, row => row[metric]).entries()) {
            if (finite(point)) points[i].values.push({ key: assetId, value: point, label, color: colors.get(assetId) });
          }
        }
        panels.append(graph(title, time, points.map(point => point.values.length ? point : null), events, format));
      }
      picker.append(panels);
    }

    function followWait(item) {
      if (group === 'type') { waitType = item.waitType; waitEvent = ''; group = 'event'; }
      else if (group === 'event') { waitType = item.waitType; waitEvent = item.waitEvent; group = 'query'; }
      else if (item.queryId) { queryId = item.queryId; mode = 'sql'; }
      change();
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
        const [context, data, waits] = await Promise.all([
          api(`${path}monitoring-context?${time.params}`), section ? api(`${path}${section}?${query}`) : null,
          mode === 'metrics' ? api(`${path}wait-history?${time.params}&group=type`).catch(() => null) : null
        ]);
        if (key !== request || !visible) return;
        const events = context.events.map(event => ({ ...event, assetName: asset.name }));
        time.cursors.clear();
        const fragment = document.createDocumentFragment();
        if (mode === 'metrics') {
          if (waits?.buckets.some(bucket => bucket.observed > 0)) {
            fragment.append(waitChart(waits.buckets, time, events, 'type', item => {
              resetWaitFilters(); mode = 'waits'; followWait(item);
            }));
          } else {
            const load = metricCharts(data.series, time, events, [
              ['transactionsPerSecond', '数据库负载 · 事务吞吐', rate, palette[0]]
            ]).firstElementChild;
            load.classList.add('monitor-focus-chart');
            load.append(el('p', '此时段缺少等待采样。可在等待分析查看采集覆盖。', 'monitor-note'));
            const split = el('div', null, 'monitor-split');
            split.append(load, eventList(events, '此实例的问题事件')); fragment.append(split);
          }
          const indicators = el('details', null, 'monitor-panel monitor-topic');
          indicators.append(el('summary', '连接、事务、WAL 与复制'), metricCharts(data.series, time, events, [
          ['transactionsPerSecond', '事务吞吐', rate, 'var(--plot-teal)'], ['averageConnectionUsagePercent', '连接占用率', percent, 'var(--plot-blue)'],
          ['averageReplicationLagBytes', '复制回放差距', bytes, 'var(--plot-orange)'], ['walBytesPerSecond', 'WAL 产生速率', value => `${bytes(value)}/s`, 'var(--plot-violet)'],
          ['averageBlockedSessions', '阻塞会话', number, 'var(--plot-orange)'], ['averageLongTransactions', '长事务', number, 'var(--plot-pink)'],
          ['averageDatabaseBytes', '数据库容量', bytes, 'var(--plot-blue)'], ['averageConnections', '平均连接数', number, 'var(--plot-teal)']
          ]));
          fragment.append(indicators);
        }
        if (mode === 'waits') {
          const filter = el('div', null, 'monitor-wait-controls');
          const dimension = select('等待分组', value => { group = value; waitType = ''; waitEvent = ''; change(); });
          options(dimension, [['type', '等待类型'], ['event', '具体等待事件'], ['query', 'Query ID']], group);
          filter.append(labeled('分组方式', dimension));
          if (waitType) filter.append(button(`返回上一级 · ${waitType}${waitEvent ? '/' + waitEvent : ''}`, () => {
            if (waitEvent) { waitEvent = ''; group = 'event'; }
            else { resetWaitFilters(); }
            change();
          }));
          fragment.append(filter, waitChart(data.buckets, time, events, group, followWait));
        }
        if (mode === 'sql') {
          const sql = panel(queryId ? `Query ID ${queryId}` : '区间 SQL 性能');
          if (queryId) sql.append(button('查看此范围内的 Top SQL', () => { queryId = ''; change(); }));
          if (data.performance.status === 'ok' && queryId && !data.performance.statements.length) {
            sql.append(el('p', '该 Query ID 在此范围的 SQL 性能快照里没有匹配指纹。', 'monitor-empty'));
          } else renderSqlPerformance(sql, data.performance);
          if (queryId) {
            const source = button('查看此实例的执行计划证据', () => openPlans(asset.id));
            sql.append(source);
          }
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
            ['averageLinuxCpuPercent', 'Linux CPU · node_exporter', percent, 'var(--plot-teal)'],
            ['averageLinuxMemoryPercent', 'Linux 内存 · node_exporter', percent, 'var(--plot-blue)']
          ]));
          fragment.append(renderInfra(context.snapshot, asset));
        }
        if (mode !== 'metrics' || waits?.buckets.some(bucket => bucket.observed > 0)) fragment.append(eventList(events, '此实例的问题事件'));
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
      if (!enabled) {
        details.hidden = true;
        fleet.hidden = false;
        fleet.replaceChildren(el('p', '监控工作台使用 Go 版和 PostgreSQL 18 长期存储。', 'monitor-note'));
        return;
      }
      const clusters = new Map(assets.map(asset => [clusterKey(asset), clusterLabel(asset)]));
      if (!clusters.has(cluster)) cluster = '';
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
      route, restore, openInstance,
      openIncident(event) {
        const asset = assets.find(item => item.id === event.assetId);
        if (!asset) return;
        const start = Date.parse(event.startedAt);
        const to = Math.min(Date.now(), event.endedAt ? Date.parse(event.endedAt) + 15 * minute : Date.now());
        const from = Math.min(to - minute, Math.max(to - 10080 * minute, start - 15 * minute));
        fixed = validRange(from, to) ? { from, to } : null;
        activeWindow = null;
        cluster = clusterKey(asset);
        database = asset.database;
        instance = asset.id;
        mode = 'waits';
        eventAnchor = event.id;
        resetWaitFilters();
      }
    };
  }

  window.DbSphereMonitoring = { create };
})();
