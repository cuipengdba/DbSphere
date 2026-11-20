// Dependency-free visualizations for the static Go console. Every line and
// marker below comes from an observed API node, edge, sample, or event.
(() => {
  'use strict';
  const ns = 'http://www.w3.org/2000/svg';
  const make = (tag, className, text) => {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text != null) node.textContent = text;
    return node;
  };
  const svg = (tag, attrs = {}) => {
    const node = document.createElementNS(ns, tag);
    for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, String(value));
    return node;
  };
  const titleNode = value => {
    const node = svg('title'); node.textContent = value; return node;
  };
  const time = value => value ? new Date(value).toLocaleString('zh-CN') : '时间未知';
  const short = (value, size = 20) => String(value ?? '').length > size
    ? `${String(value).slice(0, size - 1)}…` : String(value ?? '');
  const sourcePath = value => typeof value === 'string' && /^\/api\/[a-z0-9/_-]+$/i.test(value)
    ? value : null;
  const stages = ['应用与请求', '数据库调用', 'PostgreSQL', '基础设施', '事件与判断'];
  const nodeStage = {
    service: 0, service_span: 0, app_pod: 0,
    db_client_span: 1, pgbouncer: 1,
    postgres: 2, sample: 2, session: 2, wait: 2, sql_fingerprint: 2,
    slow_log: 2, plan: 2,
    linux: 3, pod: 3, k8s_node: 3, pvc: 3, pvc_status: 3,
    event: 4, evidence: 4, candidate: 4
  };
  const typeLabel = {
    service: '服务', service_span: '服务 Span', app_pod: '应用 Pod',
    db_client_span: '数据库 Span', pgbouncer: '连接池', postgres: '数据库',
    sample: '采样', session: '会话', wait: '等待', sql_fingerprint: 'SQL 指纹',
    slow_log: '慢日志', plan: '执行计划', linux: 'Linux', pod: '数据库 Pod',
    k8s_node: 'K8S Node', pvc: 'PVC', pvc_status: 'PVC 状态',
    event: '事件', evidence: '证据', candidate: '根因候选'
  };
  const evidenceLabel = {
    alert_firing: '告警触发', alert_recovered: '告警恢复',
    alert_signal_missing: '告警信号缺失', alert_disabled: '规则停用',
    alert_suppressed: '告警静默', alert_unsuppressed: '静默结束',
    collection_error: '采集失败', collection_recovered: '采集恢复',
    linux_state: 'Linux 状态', k8s_state: 'Kubernetes 状态',
    pgbouncer_state: 'PgBouncer 状态', patroni_state: 'Patroni 状态',
    root_cause_candidate: '根因候选', rca_decision: '人工根因决定',
    event_acknowledged: '事件确认', recovery_verified: '恢复验证',
    operator_response: '处置记录', work_order_requested: '工单申请',
    work_order_decision: '工单审批', work_order_executed: '工单执行',
    ai_invocation: 'AI 分析调用', ai_advisory: 'AI 分析建议'
  };
  const displayLabel = node => node.type === 'evidence'
    ? evidenceLabel[node.label] || node.label : node.label;
  const scopes = {
    all: null,
    trace: new Set(['service', 'service_span', 'app_pod', 'db_client_span', 'pgbouncer', 'postgres', 'sample']),
    database: new Set(['postgres', 'sample', 'session', 'wait', 'sql_fingerprint', 'slow_log', 'plan', 'pgbouncer']),
    infra: new Set(['postgres', 'sample', 'linux', 'pod', 'app_pod', 'k8s_node', 'pvc', 'pvc_status']),
    cause: new Set(['event', 'evidence', 'candidate', 'sample', 'postgres'])
  };
  const accent = ['var(--plot-blue)', 'var(--plot-violet)', 'var(--plot-teal)', 'var(--plot-orange)', 'var(--plot-orange)'];
  const important = new Set(['parent_of', 'calls', 'calls_service', 'via_pooler', 'routes_to',
    'same_backend_pid', 'blocked_by', 'query_id_matches', 'supported_by',
    'hypothesizes', 'originated_from', 'scheduled_on', 'references_pvc', 'observed_pvc_status']);
  const relationLabel = {
    affects: '影响', contains: '包含', observed_in: '见于样本', collected_from: '采自',
    collected_with: '同期采集', emitted: '产生', parent_of: '父链路', calls: '调用',
    calls_service: '调用服务', via_pooler: '经由连接池', forwards_to: '转发至',
    time_window: '处于事件时间窗', time_unreliable: '时间关联待核对',
    query_id_matches: 'Query ID 匹配', same_backend_pid: '后端 PID 匹配',
    observed_session: '观测到会话', blocked_by: '被阻塞于',
    observed_sql: '观测到 SQL', scheduled_on: '调度于',
    originated_from: '来自应用 Pod', references_pvc: '引用 PVC',
    observed_pvc_status: '核对 PVC 状态', routes_to: '路由至',
    compared_with: '与基线比较', hypothesizes: '提出候选',
    supported_by: '由证据支持', reviewed_by: '人工复核'
  };

  function eventGraph(graph, event) {
    const allNodes = Array.isArray(graph?.nodes) ? graph.nodes.filter(node => node?.id) : [];
    const allEdges = Array.isArray(graph?.edges) ? graph.edges.filter(edge => edge?.from && edge?.to) : [];
    const byID = new Map(allNodes.map(node => [node.id, node]));
    const section = make('section', 'obs-panel obs-evidence-graph');
    const heading = make('div', 'obs-panel-head');
    heading.append(make('h4', '', '全链路证据图'), make('small', '', '只绘制已观测关系；点击节点查看来源'));
    section.append(heading);
    const controls = make('div', 'obs-controls');
    const scope = make('select');
    scope.setAttribute('aria-label', '证据图范围');
    for (const [value, title] of [['all', '全部证据'], ['trace', '应用链路'],
      ['database', '数据库与 SQL'], ['infra', 'Linux / K8S'], ['cause', '根因证据']]) {
      const option = make('option', '', title); option.value = value; scope.append(option);
    }
    const search = make('input'); search.type = 'search'; search.placeholder = '查找节点、Pod、SQL 或 Trace';
    search.setAttribute('aria-label', '查找证据节点');
    controls.append(scope, search);
    section.append(controls);
    const legend = make('p', 'obs-legend', '应用与请求 → 数据库调用 → PostgreSQL → 基础设施 → 事件与判断');
    section.append(legend);
    const viewport = make('div', 'obs-graph-viewport');
    viewport.tabIndex = 0;
    viewport.setAttribute('aria-label', '可横向滚动的全链路证据图');
    section.append(viewport);
    const count = make('p', 'obs-caption'); section.append(count);
    const detail = make('div', 'obs-node-detail', '选择图中的节点，查看完整名称、时间、关联关系和原始数据。');
    detail.setAttribute('aria-live', 'polite'); section.append(detail);
    let selected = '';

    function nodeDetail(id) {
      selected = id;
      const node = byID.get(id);
      if (!node) return;
      const label = displayLabel(node);
      detail.replaceChildren(make('strong', '', `${typeLabel[node.type] || node.type} · ${label}${label === node.label ? '' : ` (${node.label})`}`),
        make('small', '', `观测时间 ${time(node.at)} · ${allEdges.filter(edge => edge.from === id || edge.to === id).length} 条关联`));
      const path = sourcePath(node.source);
      if (path) {
        const link = make('a', '', '查看原始数据 ↗'); link.href = path;
        link.target = '_blank'; link.rel = 'noopener noreferrer'; detail.append(link);
      }
      const relations = allEdges.filter(edge => edge.from === id || edge.to === id);
      if (relations.length) {
        const links = make('div', 'obs-related');
        for (const edge of relations.slice(0, 12)) {
          const outgoing = edge.from === id;
          const neighbor = byID.get(outgoing ? edge.to : edge.from);
          if (!neighbor) continue;
          const button = make('button', 'ghost small-button',
            `${outgoing ? '→' : '←'} ${relationLabel[edge.relation] || edge.relation} · ${short(displayLabel(neighbor), 35)}`);
          button.type = 'button'; button.addEventListener('click', () => nodeDetail(neighbor.id));
          links.append(button);
        }
        detail.append(links);
        if (relations.length > 12) detail.append(make('small', '', `另有 ${relations.length - 12} 条关联；可用下方完整证据关系查看。`));
      }
      draw();
    }
    function draw() {
      const query = search.value.trim().toLocaleLowerCase();
      const allowed = scopes[scope.value] || null;
      const visible = allNodes.filter(node => !allowed || allowed.has(node.type));
      const matches = query ? visible.filter(node =>
        `${node.label} ${displayLabel(node)} ${node.id} ${typeLabel[node.type] || node.type}`.toLocaleLowerCase().includes(query)) : visible;
      const ids = new Set(matches.map(node => node.id));
      if (query) {
        for (const edge of allEdges) if (ids.has(edge.from) || ids.has(edge.to)) {
          if (byID.has(edge.from) && (!allowed || allowed.has(byID.get(edge.from).type))) ids.add(edge.from);
          if (byID.has(edge.to) && (!allowed || allowed.has(byID.get(edge.to).type))) ids.add(edge.to);
        }
      }
      if (selected && (!allowed || allowed.has(byID.get(selected)?.type))) ids.add(selected);
      const degree = new Map();
      for (const edge of allEdges) {
        degree.set(edge.from, (degree.get(edge.from) || 0) + 1);
        degree.set(edge.to, (degree.get(edge.to) || 0) + 1);
      }
      const priority = node => (node.id === selected ? 1000 : 0) +
        ({ candidate: 110, service_span: 90, db_client_span: 90, postgres: 88,
          pod: 85, app_pod: 82, sql_fingerprint: 80, session: 78, pgbouncer: 76,
          sample: 72, event: 70, evidence: 32 }[node.type] || 55) +
        Math.min(degree.get(node.id) || 0, 20);
      const selectedNodes = allNodes.filter(node => ids.has(node.id))
        .sort((a, b) => priority(b) - priority(a) || a.id.localeCompare(b.id)).slice(0, 45);
      const selectedIDs = new Set(selectedNodes.map(node => node.id));
      const edges = allEdges.filter(edge => selectedIDs.has(edge.from) && selectedIDs.has(edge.to))
        .sort((a, b) => Number(important.has(b.relation)) - Number(important.has(a.relation))).slice(0, 90);
      const buckets = stages.map(() => []);
      for (const node of selectedNodes) buckets[nodeStage[node.type] ?? 4].push(node);
      const activeLanes = buckets.map((bucket, index) => ({ bucket, index }))
        .filter(lane => lane.bucket.length);
      legend.textContent = activeLanes.map(lane => stages[lane.index]).join(' → ') || '当前筛选下没有节点';
      const laneWidth = 238; const rowHeight = 78;
      const width = Math.max(300, activeLanes.length * laneWidth);
      const height = Math.max(245, 78 + Math.max(0, ...activeLanes.map(lane => lane.bucket.length)) * rowHeight);
      const canvas = svg('svg', { viewBox: `0 0 ${width} ${height}`, width, height,
        role: 'img', 'aria-label': `${selectedNodes.length} 个证据节点、${edges.length} 条已观测关系` });
      const markerID = `obs-arrow-${String(event?.id || 'incident').replace(/[^a-z0-9_-]/gi, '')}`;
      const defs = svg('defs');
      const marker = svg('marker', { id: markerID, markerWidth: 7, markerHeight: 7,
        refX: 6, refY: 3.5, orient: 'auto', markerUnits: 'userSpaceOnUse' });
      marker.append(svg('path', { d: 'M 0 0 L 7 3.5 L 0 7 z', fill: 'var(--plot-gray)' }));
      defs.append(marker); canvas.append(defs);
      const positions = new Map();
      for (let lane = 0; lane < activeLanes.length; lane++) {
        const { bucket, index: stageIndex } = activeLanes[lane];
        canvas.append(svg('rect', { x: lane * laneWidth + 3, y: 4, width: laneWidth - 6,
          height: height - 8, rx: 12, fill: lane % 2 ? 'var(--soft)' : 'var(--bg)' }));
        const title = svg('text', { x: lane * laneWidth + 17, y: 30, class: 'obs-lane-title' });
        title.textContent = stages[stageIndex]; canvas.append(title);
        bucket.forEach((node, row) => positions.set(node.id,
          { x: lane * laneWidth + 15, y: 49 + row * rowHeight, stageIndex }));
      }
      for (const edge of edges) {
        const from = positions.get(edge.from); const to = positions.get(edge.to);
        const x1 = from.x + 208; const y1 = from.y + 25;
        const x2 = to.x; const y2 = to.y + 25;
        const mid = (x1 + x2) / 2;
        const line = svg('path', { d: `M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`,
          fill: 'none', stroke: important.has(edge.relation) ? 'var(--plot-gray)' : 'var(--border)',
          'stroke-width': important.has(edge.relation) ? 1.8 : 1.2,
          'marker-end': `url(#${markerID})`,
          class: selected && edge.from !== selected && edge.to !== selected ? 'obs-muted' : '' });
        line.append(titleNode(relationLabel[edge.relation] || edge.relation));
        canvas.append(line);
      }
      for (const node of selectedNodes) {
        const at = positions.get(node.id);
        const group = svg('g', { class: `obs-graph-node${selected === node.id ? ' is-selected' : ''}`,
          role: 'button', tabindex: 0, 'aria-label': `${typeLabel[node.type] || node.type} ${displayLabel(node)}` });
        group.append(svg('rect', { x: at.x, y: at.y, width: 208, height: 52, rx: 9,
          fill: selected === node.id ? 'var(--selected)' : 'var(--surface)', stroke: accent[at.stageIndex], 'stroke-width': selected === node.id ? 2.7 : 1.3 }));
        const label = svg('text', { x: at.x + 11, y: at.y + 20, class: 'obs-node-label' });
        label.textContent = short(displayLabel(node), 24); group.append(label);
        const kind = svg('text', { x: at.x + 11, y: at.y + 39, class: 'obs-node-kind' });
        kind.textContent = typeLabel[node.type] || node.type; group.append(kind);
        group.append(titleNode(`${displayLabel(node)} · ${typeLabel[node.type] || node.type} · ${node.label}`));
        group.addEventListener('click', () => nodeDetail(node.id));
        group.addEventListener('keydown', event => {
          if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); nodeDetail(node.id); }
        });
        canvas.append(group);
      }
      viewport.replaceChildren(canvas);
      count.textContent = query && !matches.length ? '没有匹配的节点。' :
        `显示 ${selectedNodes.length}/${allNodes.length} 个节点、${edges.length}/${allEdges.length} 条关系${allNodes.length > selectedNodes.length ? '；可按范围或名称定位更多节点' : ''}。方向表示记录中的关系，不自动推断因果。`;
    }
    scope.addEventListener('change', draw);
    search.addEventListener('input', draw);
    draw();
    return section;
  }

  function incidentTimeline(event) {
    const evidence = Array.isArray(event.evidence) ? event.evidence : [];
    const section = make('section', 'obs-panel obs-incident-timeline');
    const header = make('div', 'obs-panel-head');
    header.append(make('h4', '', '故障形成与恢复时间轴'),
      make('small', '', `${evidence.length} 条事件记录`)); section.append(header);
    if (!evidence.length) { section.append(make('p', 'empty', '暂无时间线数据。')); return section; }
    const rows = evidence.slice(-60);
    const lanes = ['告警', '数据库', '基础设施', '判断/处置'];
    const lane = kind => /alert|anomaly/.test(kind) ? 0 :
      /collection|sample|sql|lock|replica/.test(kind) ? 1 :
        /k8s|pod|pvc|linux|node/.test(kind) ? 2 : 3;
    const points = rows.map(item => ({ item, at: Date.parse(item.observedAt), lane: lane(item.kind) }))
      .filter(point => Number.isFinite(point.at));
    if (!points.length) { section.append(make('p', 'empty', '事件记录没有可用时间戳。')); return section; }
    const min = Math.min(Date.parse(event.startedAt) || points[0].at, ...points.map(point => point.at));
    const max = Math.max(Date.parse(event.endedAt) || min, ...points.map(point => point.at));
    const span = Math.max(1000, max - min);
    const width = 550; const height = 202; const left = 70; const right = 16;
    const canvas = svg('svg', { viewBox: `0 0 ${width} ${height}`, width,
      role: 'img', 'aria-label': `事件时间轴：${points.length} 条记录` });
    const detail = make('div', 'obs-node-detail', '选择时间线上的标记，查看事件记录。');
    detail.setAttribute('aria-live', 'polite');
    for (let i = 0; i < lanes.length; i++) {
      const y = 36 + i * 41;
      const label = svg('text', { x: 9, y: y + 5, class: 'obs-time-label' });
      label.textContent = lanes[i]; canvas.append(label);
      canvas.append(svg('line', { x1: left, x2: width - right, y1: y, y2: y,
        stroke: 'var(--border)', 'stroke-width': 1 }));
    }
    const overlaps = new Map();
    for (const point of points) {
      const position = left + (point.at - min) / span * (width - left - right);
      const key = `${point.lane}:${Math.round(position / 12)}`;
      const overlap = overlaps.get(key) || 0; overlaps.set(key, overlap + 1);
      const x = position + Math.floor(overlap / 3) * 8;
      const y = 36 + point.lane * 41 + (overlap % 3 - 1) * 8;
      const marker = svg('circle', { cx: x, cy: y, r: 6.5,
        fill: point.item.kind.includes('recovered') ? 'var(--ok-line)' :
          point.item.kind.includes('error') || point.item.kind.includes('firing') ? 'var(--issue-line)' : 'var(--plot-blue)' });
      marker.append(titleNode(`${time(point.item.observedAt)} · ${evidenceLabel[point.item.kind] || point.item.kind}`));
      const group = svg('g', { role: 'button', tabindex: 0,
        'aria-label': `${time(point.item.observedAt)} ${evidenceLabel[point.item.kind] || point.item.kind}` });
      group.append(marker);
      const show = () => {
        detail.replaceChildren(
          make('strong', '', `${time(point.item.observedAt)} · ${evidenceLabel[point.item.kind] || point.item.kind} (${point.item.kind})`),
          make('small', '', `来源：${lanes[point.lane]} · 记录 ID ${point.item.id || '未提供'}`));
        const payload = point.item.payload || {};
        const summary = [payload.ruleId, payload.hypothesis, payload.reason,
          payload.error, payload.sampleId].filter(value => typeof value === 'string' && value);
        if (summary.length) detail.append(make('small', '', short(summary.join(' · '), 200)));
      };
      group.addEventListener('click', show);
      group.addEventListener('keydown', event => {
        if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); show(); }
      });
      canvas.append(group);
    }
    const start = svg('text', { x: left, y: 194, class: 'obs-time-label' });
    start.textContent = time(new Date(min)); canvas.append(start);
    const end = svg('text', { x: width - right, y: 194, 'text-anchor': 'end', class: 'obs-time-label' });
    end.textContent = time(new Date(max)); canvas.append(end);
    const viewport = make('div', 'obs-timeline-viewport'); viewport.append(canvas); section.append(viewport, detail);
    if (evidence.length > rows.length) section.append(make('small', 'obs-caption',
      `图中只显示最近 ${rows.length} 条记录；完整记录见下方事件时间线。`));
    return section;
  }

  function sampleTrend(series, hours, bucketHours) {
    const section = make('div', 'obs-trend-charts');
    const points = Array.isArray(series) ? series.filter(point => Number.isFinite(Date.parse(point.bucket))) : [];
    if (!points.length) { section.append(make('p', 'empty', '所选时间范围内没有采样；无法绘制趋势。')); return section; }
    const bucketMs = bucketHours * 3600000;
    const end = Math.floor(Date.now() / bucketMs) * bucketMs;
    const start = end - (Math.ceil(hours / bucketHours) - 1) * bucketMs;
    const data = new Map(points.map(point => [Math.floor(Date.parse(point.bucket) / bucketMs) * bucketMs, point]));
    const slots = Array.from({ length: Math.ceil(hours / bucketHours) }, (_, index) =>
      data.get(start + index * bucketMs) || null);
    const width = window.matchMedia('(max-width:680px)').matches ? 280 : 400;
    const height = 178; const left = 38; const right = 12;
    const plotWidth = width - left - right;
    const maxSamples = Math.max(1, ...slots.map(point => Number(point?.samples) || 0));
    const maxConnections = Math.max(1, ...slots.map(point => Number(point?.averageConnections) || 0));
    const chart = (title, kind) => {
      const block = make('div', 'obs-chart'); block.append(make('h4', '', title));
      const canvas = svg('svg', { viewBox: `0 0 ${width} ${height}`, width,
        role: 'img', 'aria-label': `${title}，${hours} 小时` });
      for (const y of [24, 66, 108, 150]) canvas.append(svg('line', {
        x1: left, x2: width - right, y1: y, y2: y, stroke: 'var(--border)' }));
      const top = svg('text', { x: left - 7, y: 29, 'text-anchor': 'end', class: 'obs-axis-label' });
      top.textContent = String(kind === 'samples' ? maxSamples : maxConnections); canvas.append(top);
      const zero = svg('text', { x: left - 7, y: 154, 'text-anchor': 'end', class: 'obs-axis-label' });
      zero.textContent = '0'; canvas.append(zero);
      let previous = null;
      slots.forEach((point, index) => {
        const x = left + index * plotWidth / slots.length + plotWidth / slots.length / 2;
        const stamp = new Date(start + index * bucketMs);
        if (kind === 'samples') {
          const samples = Number(point?.samples) || 0;
          const errors = Math.min(samples, Number(point?.errors) || 0);
          const success = samples - errors;
          const unit = 122 / maxSamples;
          const barWidth = Math.max(3, Math.min(22, plotWidth / slots.length - 3));
          const group = svg('g');
          if (point) {
            group.append(svg('rect', { x: x - barWidth / 2, y: 150 - success * unit,
              width: barWidth, height: Math.max(success * unit, 1), fill: 'var(--plot-blue)' }));
            if (errors) group.append(svg('rect', { x: x - barWidth / 2,
              y: 150 - samples * unit, width: barWidth, height: errors * unit, fill: 'var(--issue-line)' }));
          } else group.append(svg('line', { x1: x - 4, x2: x + 4, y1: 148, y2: 148,
            stroke: 'var(--plot-gray)', 'stroke-width': 2 }));
          group.append(titleNode(`${time(stamp)} · ${point ? `成功 ${success}、失败 ${errors}` : '无采样'}`));
          canvas.append(group);
        } else {
          const value = point?.averageConnections == null ? NaN : Number(point.averageConnections);
          if (Number.isFinite(value)) {
            const y = 150 - value / maxConnections * 122;
            if (previous) canvas.append(svg('line', { x1: previous.x, y1: previous.y,
              x2: x, y2: y, stroke: 'var(--plot-violet)', 'stroke-width': 2.5 }));
            const dot = svg('circle', { cx: x, cy: y, r: 3.6, fill: 'var(--plot-blue)' });
            dot.append(titleNode(`${time(stamp)} · 平均连接 ${value}`));
            canvas.append(dot); previous = { x, y };
          } else previous = null;
        }
      });
      const startText = svg('text', { x: left, y: 173, class: 'obs-axis-label' });
      startText.textContent = time(new Date(start)); canvas.append(startText);
      const endText = svg('text', { x: width - right, y: 173,
        'text-anchor': 'end', class: 'obs-axis-label' });
      endText.textContent = time(new Date(end)); canvas.append(endText);
      const viewport = make('div', 'obs-trend-viewport'); viewport.append(canvas); block.append(viewport);
      return block;
    };
    section.append(chart('采样成功与失败', 'samples'), chart('平均连接数', 'connections'));
    const latest = points.at(-1);
    section.append(make('p', 'obs-latest',
      `最新时间桶 ${time(latest.bucket)} · ${latest.samples} 次采样 · ${latest.errors} 次失败 · 平均连接 ${latest.averageConnections ?? '未采集'}`));
    section.append(make('p', 'obs-caption', '蓝色为成功采样，橙色为失败采样；短横线表示无采样。连接数只连接相邻且有值的时间桶，不推断缺失数据。'));
    return section;
  }

  function metricTrend(series, hours, bucketHours) {
    const section = make('section', 'obs-metric-panel');
    section.append(make('h3', '', 'PostgreSQL 核心指标'));
    const points = Array.isArray(series) ? series.filter(point => Number.isFinite(Date.parse(point.bucket))) : [];
    if (!points.length) {
      section.append(make('p', 'empty', '所选时间范围内没有成功采样；指标趋势不可用。'));
      return section;
    }
    const bucketMs = bucketHours * 3600000;
    const end = Math.floor(Date.now() / bucketMs) * bucketMs;
    const start = end - (Math.ceil(hours / bucketHours) - 1) * bucketMs;
    const data = new Map(points.map(point => [Math.floor(Date.parse(point.bucket) / bucketMs) * bucketMs, point]));
    const slots = Array.from({ length: Math.ceil(hours / bucketHours) }, (_, index) =>
      data.get(start + index * bucketMs) || null);
    const bytes = value => value >= 1024 ** 3 ? `${(value / 1024 ** 3).toFixed(1)} GiB` :
      value >= 1024 ** 2 ? `${(value / 1024 ** 2).toFixed(1)} MiB` :
        value >= 1024 ? `${(value / 1024).toFixed(1)} KiB` :
          `${value.toFixed(value > 0 && value < 1 ? 2 : 0)} B`;
    const count = value => Number(value).toFixed(1);
    const specs = [
      { key: 'averageConnectionUsagePercent', title: '连接占用率', format: value => `${value.toFixed(1)}%`, color: 'var(--plot-blue)' },
      { key: 'transactionsPerSecond', title: '事务吞吐', format: value => `${value.toFixed(2)} 次/秒`, color: 'var(--plot-violet)' },
      { key: 'averageBlockedSessions', title: '阻塞会话', format: value => `${count(value)} 个`, color: 'var(--plot-orange)' },
      { key: 'averageLongTransactions', title: '长事务', format: value => `${count(value)} 个`, color: 'var(--plot-pink)' },
      { key: 'walBytesPerSecond', title: 'WAL 产生速率', format: value => `${bytes(value)}/s`, color: 'var(--plot-teal)' },
      { key: 'averageDatabaseBytes', title: '数据库容量', format: bytes, color: 'var(--plot-gray)' },
      { key: 'averageReplicationLagBytes', title: '复制回放差距', format: bytes, color: 'var(--plot-orange)' }
    ];
    const grid = make('div', 'obs-metric-grid');
    const missing = [];
    const width = window.matchMedia('(max-width:680px)').matches ? 280 : 400;
    const height = 178; const left = 38; const right = 12;
    const plotWidth = width - left - right;
    for (const spec of specs) {
      const values = slots.map(point => point?.[spec.key] == null ? null : Number(point[spec.key]));
      if (!values.some(value => value != null && Number.isFinite(value))) {
        missing.push(spec.title); continue;
      }
      const observedMax = Math.max(...values.filter(value => value != null && Number.isFinite(value)));
      const scaleMax = observedMax > 0 ? observedMax : 1;
      const card = make('article', 'obs-chart'); card.append(make('h4', '', spec.title));
      const canvas = svg('svg', { viewBox: `0 0 ${width} ${height}`, width, height,
        role: 'img', 'aria-label': `${spec.title}，${hours} 小时` });
      for (const y of [24, 66, 108, 150]) canvas.append(svg('line', {
        x1: left, x2: width - right, y1: y, y2: y, stroke: 'var(--border)' }));
      const topLabel = svg('text', { x: left, y: 16, class: 'obs-axis-label' });
      topLabel.textContent = spec.format(observedMax); canvas.append(topLabel);
      const zero = svg('text', { x: left - 5, y: 154, 'text-anchor': 'end', class: 'obs-axis-label' });
      zero.textContent = '0'; canvas.append(zero);
      const selected = make('p', 'obs-metric-selected');
      let previous = null;
      values.forEach((value, index) => {
        if (value == null || !Number.isFinite(value)) { previous = null; return; }
        const x = left + index * plotWidth / values.length + plotWidth / values.length / 2;
        const y = 150 - Math.max(0, value) / scaleMax * 122;
        if (previous && previous.index === index - 1) canvas.append(svg('line', {
          x1: previous.x, y1: previous.y, x2: x, y2: y,
          stroke: spec.color, 'stroke-width': 2.5 }));
        const stamp = new Date(start + index * bucketMs);
        const label = `${time(stamp)} · ${spec.format(value)}`;
        const group = svg('g', { role: 'button', tabindex: 0, 'aria-label': label });
        const dot = svg('circle', { cx: x, cy: y, r: 4.2, fill: spec.color });
        dot.append(titleNode(label)); group.append(dot);
        const show = () => { selected.textContent = label; };
        group.addEventListener('click', show);
        group.addEventListener('keydown', event => {
          if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); show(); }
        });
        canvas.append(group);
        selected.textContent = label;
        previous = { index, x, y };
      });
      const firstAt = svg('text', { x: left, y: 173, class: 'obs-axis-label' });
      firstAt.textContent = time(new Date(start)); canvas.append(firstAt);
      const lastAt = svg('text', { x: width - right, y: 173, 'text-anchor': 'end', class: 'obs-axis-label' });
      lastAt.textContent = time(new Date(end)); canvas.append(lastAt);
      const viewport = make('div', 'obs-trend-viewport'); viewport.append(canvas);
      card.append(viewport, selected); grid.append(card);
    }
    section.append(grid);
    if (missing.length) section.append(make('p', 'obs-caption', `当前范围未采集：${missing.join('、')}。`));
    section.append(make('p', 'obs-caption',
      '点选数据点查看时间与数值。事务和 WAL 速率只用相邻有效样本计算；计数器重置、采集失败及超过 10 分钟的间隔不补值。'));
    return section;
  }

  window.DbSphereObservability = { eventGraph, incidentTimeline, sampleTrend, metricTrend };
})();
