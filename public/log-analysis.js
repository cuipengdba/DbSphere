(function () {
  'use strict';
  const base = '/api/log-analysis/';
  const coverageLabels = { unknown: '待确认', slow: '仅慢日志', sampled: '抽样日志', full: '全量语句' };
  const fileStates = { uploading: '上传中', queued: '等待或解析中', complete: '已完成', partial: '部分解析',
    failed: '需要确认格式', duplicate: '已导入' };
  const kinds = { statement: 'SQL', lock: '锁等待', deadlock: '死锁', error: '错误', checkpoint: 'Checkpoint',
    autovacuum: 'Autovacuum', temp: '临时文件', unknown: '未识别', other: '其他' };
  const number = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 });
  const safeId = value => /^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$/.test(value || '') ? value : '';
  const bytes = value => value >= 1048576 ? number.format(value / 1048576) + ' MiB' : number.format(value / 1024) + ' KiB';
  const formatted = value => typeof value === 'number' ? number.format(value) : '—';
  const changes = { resolved:'已解决', improved:'明显改善', regressed:'性能劣化', new:'新增问题',
    unobserved:'本批次未检出', uncertain:'覆盖不足', stable:'稳定' };
  const retestStates = { met: '已达标', missed: '未达标', regressed: '性能回退', insufficient: '覆盖不足' };

  function element(tag, value, className) {
    const item = document.createElement(tag);
    if (value !== undefined) item.textContent = value;
    if (className) item.className = className;
    return item;
  }

  function button(value, action) {
    const item = element('button', value, 'ghost');
    item.type = 'button';
    item.addEventListener('click', action);
    return item;
  }

  function table(labels, rows, empty) {
    if (!rows.length) return element('p', empty, 'empty');
    const result = element('table', undefined, 'log-table');
    const head = result.createTHead().insertRow();
    for (const label of labels) head.append(element('th', label));
    const body = result.createTBody();
    for (const row of rows) {
      const target = body.insertRow();
      for (const value of row) {
        const cell = target.insertCell();
        if (value instanceof Node) cell.append(value);
        else cell.textContent = value ?? '—';
      }
    }
    return result;
  }

  function options(select, values, value, empty = '') {
    select.replaceChildren(...(empty ? [new Option(empty, '')] : []),
      ...values.map(([id, label]) => new Option(label, id)));
    if ([...select.options].some(option => option.value === value)) select.value = value;
  }

  function logChart(title, points, lines, chartTime) {
    const figure = element('figure', undefined, 'log-report-chart');
    const legend = element('div', undefined, 'log-chart-labels');
    const canvas = element('canvas');
    canvas.setAttribute('aria-label', title);
    const axis = element('div', undefined, 'log-chart-time');
    axis.append(element('span', chartTime(points[0]?.at)), element('span', chartTime(points.at(-1)?.at)));
    lines.forEach(([name, read]) => {
      const latest = points.map(read).findLast(value => value != null);
      legend.append(element('span', name + ' ' + formatted(latest)));
    });
    figure.append(element('strong', title), legend, canvas, axis);
    return { figure, draw() {
      const width = canvas.clientWidth, height = canvas.clientHeight;
      if (!width || !height) return;
      const ratio = Math.min(window.devicePixelRatio || 1, 2);
      canvas.width = width * ratio;
      canvas.height = height * ratio;
      const ctx = canvas.getContext('2d');
      ctx.scale(ratio, ratio);
      const css = getComputedStyle(figure);
      ctx.strokeStyle = css.getPropertyValue('--border').trim();
      for (const y of [4, height / 2, height - 4]) {
        ctx.beginPath();
        ctx.moveTo(0, y);
        ctx.lineTo(width, y);
        ctx.stroke();
      }
      let peak = 1;
      for (const point of points) for (const [, read] of lines) peak = Math.max(peak, read(point) ?? 0);
      const span = Math.max(60000, new Date(points.at(-1)?.at) - new Date(points[0]?.at));
      lines.forEach(([, read], index) => {
        ctx.strokeStyle = index === 0 ? css.getPropertyValue('--accent').trim() :
          document.documentElement.dataset.theme === 'dark' ? '#f0a82f' : '#a25c06';
        ctx.fillStyle = ctx.strokeStyle;
        ctx.lineWidth = 1.6;
        let previous = null;
        for (const row of points) {
          const value = read(row);
          if (value == null) { previous = null; continue; }
          const x = 4 + (new Date(row.at) - new Date(points[0].at)) / span * (width - 8);
          const y = height - 4 - value / peak * (height - 8);
          ctx.beginPath();
          if (previous) {
            ctx.moveTo(previous[0], previous[1]);
            ctx.lineTo(x, y);
            ctx.stroke();
          } else {
            ctx.arc(x, y, 2, 0, Math.PI * 2);
            ctx.fill();
          }
          previous = [x, y];
        }
      });
    } };
  }

  function create(root, { api, formatTime, notice, openEvent }) {
    const get = name => root.querySelector('[data-logs="' + name + '"]');
    const run = (job, target = get('status')) => void job().catch(error => {
      if (active) target.textContent = error.message;
    });
    let directory = { projects: [], instances: [] };
    let active = false, generation = 0, timer, page = 0, current = '', batches = [];
    let retryFile = '', resumeFile = '', uploading = false, previewRequest = 0, recordRequest = 0, reportRequest = 0;
    const pendingUploads = new Map();
    let comparing = false, baseline = '', comparisonData, compareRequest = 0;
    let caseId = '', sourceId = '', returnIncident = '';
    let taskRequest = 0, taskNext = null;
    let reportCharts = [];
    let archiveOrganization = null, archiveOffset = 0, archiveStep = 40, archiveRequest = 0;
    const filters = get('record-filters');
    const repaint = () => { if (active) reportCharts.forEach(chart => chart.draw()); };
    const resize = new ResizeObserver(repaint);
    new MutationObserver(repaint).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    resize.observe(get('report'));

    function selectBatch(id) {
      if (current !== id) {
        baseline = '';
        comparing = false;
        caseId = '';
        sourceId = '';
      }
      current = safeId(id);
      page = 0;
      filters.reset();
      saveSelection();
    }

    function saveSelection() {
      if (!active) return;
      const query = new URLSearchParams();
      if (current) query.set('batch', current);
      if (comparing && baseline) query.set('baseline', baseline);
      if (caseId) query.set('case', caseId);
      if (sourceId) query.set('source', sourceId);
      if (returnIncident) query.set('incident', returnIncident);
      history.replaceState(null, '', '#logs' + (query.size ? '?' + query : ''));
    }

    function configureDirectory() {
      const selected = batches.find(item => item.id === current);
      const instance = directory.instances.find(item => item.id === selected?.instanceId);
      options(get('project'), directory.projects.map(item => [item.id, item.organization + ' · ' + item.name]),
        instance?.projectId || get('project').value);
      const instances = directory.instances.filter(item => item.projectId === get('project').value);
      const environments = [...new Set(instances.map(item => item.environment))];
      options(get('environment'), environments.map(value => [value, value]), instance?.environment || get('environment').value, '全部环境');
      const visible = instances.filter(item => !get('environment').value || item.environment === get('environment').value);
      options(get('instance'), visible.map(item => [item.id, item.name + ' · ' + item.version]), instance?.id || get('instance').value);
      get('new-instance').disabled = !get('project').value;
      get('new-batch').disabled = !get('instance').value;
      get('open-health').disabled = !get('project').value;
    }

    async function loadDirectory() {
      const request = generation;
      const value = await api(base + 'directory');
      if (!active || request !== generation) return;
      directory = value;
      if (current) {
        const selected = await api(base + 'batches/' + current);
        if (!active || request !== generation) return;
        batches = [selected.batch];
      }
      configureDirectory();
      await loadBatches();
    }

    async function loadBatches() {
      const request = ++generation;
      const instance = get('instance').value;
      clearTimeout(timer);
      get('details').hidden = true;
      get('upload').disabled = true;
      get('open-comparison').disabled = true;
      get('comparison').hidden = true;
      if (!instance) {
        options(get('batch'), [], '', '暂无批次');
        get('status').textContent = '先在项目下登记实例，再新建分析批次。';
        return;
      }
      const result = await api(base + 'batches?instance=' + instance);
      if (!active || request !== generation) return;
      batches = result.batches;
      options(get('batch'), batches.map(item => [item.id, item.name]), current);
      selectBatch(get('batch').value);
      options(get('baseline'), batches.filter(item => item.id !== current).map(item => [item.id, item.name]), baseline);
      get('open-comparison').disabled = batches.length < 2;
      if (!current) {
        get('status').textContent = '新建批次后可以上传一组日志。';
        return;
      }
      get('upload').disabled = false;
      await loadBatch();
      if (comparing && !get('open-comparison').disabled) await toggleComparison(true);
    }

    async function loadBatch() {
      const id = current;
      if (!id) return;
      clearTimeout(timer);
      const { batch, files, totals } = await api(base + 'batches/' + id);
      if (id !== current || !active) return;
      get('status').textContent = [batch.project, batch.environment, batch.instance, batch.version,
        totals.from ? formatTime(totals.from) + ' — ' + formatTime(totals.to) : '等待读取日志时间'].join(' · ');
      get('details').hidden = false;
      const cards = [['已入库记录', totals.records], ['慢 SQL ≥ 1 秒', totals.slow], ['错误与死锁', totals.errors], ['未识别记录', totals.unknown]];
      get('summary').replaceChildren(...cards.map(([label, total]) => {
        const item = element('div', undefined, 'log-metric');
        item.append(element('span', label), element('strong', number.format(total)));
        return item;
      }));
      get('coverage').textContent = ['日志覆盖 ' + coverageLabels[batch.coverage], batch.coverageNote].filter(Boolean).join(' · ');
      get('files').replaceChildren(table(['文件', '来源及格式', '状态', '解析记录', '原始 / 归档', '操作'], files.map(file => {
        const state = element('div', fileStates[file.state] || file.state);
        if (file.reason) state.append(element('small', file.reason));
        const action = element('div', undefined, 'log-actions');
        if (file.archiveExpiredAt) state.append(element('small', '原文归档已到期'));
        if (file.verifiedAt) state.append(element('small', '已通过完整性校验 · ' + formatTime(file.verifiedAt)));
        if (file.state === 'failed' && !file.archiveExpiredAt) action.append(button('重新解析', () => showUpload(file.id)));
        if (file.state === 'uploading') action.append(button('继续上传', () => showUpload('', file)));
        if (file.storedSize > 0 && ['complete', 'partial', 'failed'].includes(file.state)) {
          action.append(button('校验归档', () => run(async () => {
            await api(base + 'files/' + file.id + '/verify', 'POST');
            notice('归档校验通过');
            await loadBatch();
          })));
        }
        if (file.state === 'duplicate' && safeId(file.duplicateBatch)) {
          const link = element('a', '查看原批次');
          link.href = '#logs?batch=' + file.duplicateBatch;
          action.append(link);
        }
        return [file.filename, [file.node, file.detectedFormat || file.format].filter(Boolean).join(' · '),
          state, file.parsedCount + ' / ' + file.recordCount, bytes(file.rawSize) + ' / ' + bytes(file.storedSize), action];
      }), '等待上传日志文件。'));
      await loadRecords();
      await loadReport();
      if (active && id === current && files.some(file => file.state === 'queued')) timer = setTimeout(() => run(loadBatch), 3200);
    }

    async function toggleComparison(on = !comparing) {
      comparing = on;
      get('open-comparison').setAttribute('aria-pressed', String(on));
      get('comparison').hidden = !on;
      if (!on) {
        saveSelection();
        return;
      }
      baseline = safeId(get('baseline').value);
      saveSelection();
      const request = ++compareRequest;
      get('current-name').textContent = batches.find(item => item.id === current)?.name || '';
      get('compare-coverage').textContent = '正在对齐两个批次';
      get('compare-summary').replaceChildren();
      get('compare-rows').replaceChildren();
      comparisonData = null;
      const query = new URLSearchParams({baseline, current});
      const value = await api(base + 'compare?' + query);
      if (!active || !comparing || request !== compareRequest) return;
      comparisonData = value;
      const score = [value.baseline.health, value.current.health].map(formatted).join(' → ');
      const windowLabel = batch => [batch.from && formatTime(batch.from), batch.to && formatTime(batch.to)].filter(Boolean).join(' – ');
      get('compare-coverage').textContent = [
        windowLabel(value.baseline), windowLabel(value.current), value.coverageReason || '日志口径可比',
        '健康评分 ' + score, value.limited ? '显示优先级最高的 500 项' : ''
      ].filter(Boolean).join(' · ');
      const summary = [
        ['慢 SQL 改善或解决', value.slowImproved],
        ['锁等待与死锁记录', (value.baseline.locks + value.baseline.deadlocks) + ' → ' + (value.current.locks + value.current.deadlocks)],
        ['劣化与新增风险', value.counts.regressed + ' / ' + value.counts.new]
      ];
      get('compare-summary').replaceChildren(...summary.map(([label, metric]) => {
        const card = element('div', undefined, 'log-metric');
        card.append(element('span', label), element('strong', metric));
        return card;
      }));
      renderComparison();
    }

    function renderComparison() {
      const value = comparisonData;
      if (!value) return;
      const mode = get('compare-filter').value;
      const rows = value.rows.filter(row => !mode || row.status === mode);
      const displayMeasure = (row, key) => row.kind === 'statement' ? row[key]?.p95Ms : row[key]?.perHour;
      get('compare-rows').replaceChildren(table(['演进状态', '类型与来源', 'SQL 或问题对象', '基线与本次', '变化幅度', '原始证据'], rows.map(row => {
        const state = element('span', changes[row.status], 'log-change');
        state.dataset.change = row.status;
        const label = element('div', undefined, 'log-record');
        label.append(element(row.kind === 'statement' ? 'code' : 'span', row.label), element('small', row.key.slice(0, 20)));
        const values = element('div', undefined, 'log-compare-meters');
        const baseValue = displayMeasure(row, 'baseline'), currentValue = displayMeasure(row, 'current');
        const max = Math.max(baseValue ?? 0, currentValue ?? 0, 1);
        const unit = row.kind === 'statement' ? ' ms' : ' 次/小时';
        for (const [name, numberValue] of [['基线', baseValue], ['本次', currentValue]]) {
          values.append(element('span', name + ' ' + formatted(numberValue) + (numberValue == null ? '' : unit)));
          if (numberValue != null) {
            const meter = element('meter');
            meter.max = max;
            meter.value = numberValue;
            meter.setAttribute('aria-label', name + unit);
            values.append(meter);
          }
        }
        const breakdown = element('details');
        breakdown.append(element('summary', '统计明细'));
        for (const [name, sample] of [['基线', row.baseline], ['本次', row.current]]) {
          if (!sample) continue;
          breakdown.append(element('p', [name + ' ' + formatted(sample.count) + ' 次',
            formatted(sample.perHour) + ' 次/小时', sample.totalMs == null ? '' : '累计 ' + formatted(sample.totalMs) + ' ms',
            sample.p95Ms == null ? '' : 'P95 ' + formatted(sample.p95Ms) + ' ms'].filter(Boolean).join(' · '), 'hint'));
        }
        values.append(breakdown);
        const delta = baseValue > 0 && currentValue != null ? formatted((currentValue / baseValue - 1) * 100) + '%' : '—';
        const actions = element('div', undefined, 'log-actions');
        for (const [name, item] of [['基线', row.baseline], ['本次', row.current]]) {
          if (item) actions.append(button(name, () => run(() => openOriginal(item.evidenceId))));
        }
        const primary = row.current || row.baseline;
        if (primary) actions.append(button('调查', () => run(() => investigate(primary.evidenceId, row.current && row.baseline?.evidenceId))));
        return [state, (kinds[row.kind] || row.kind) + ' · ' + (row.application || '未标识'), label, values, delta, actions];
      }), '当前筛选暂无可对齐的问题。'));
    }

    async function openHealth() {
      const data = await api(base + 'health');
      if (!active) return;
      const rows = data.history.map(row => {
        const link = element('a', row.name);
        link.href = '#logs?batch=' + row.id;
        return [row.project, link, row.instance, row.health == null ? '覆盖不足' : row.health + ' 分',
          row.slow, row.errors, row.locks, row.deadlocks, row.from ? formatTime(row.from) : '—'];
      });
      get('health-history').replaceChildren(table(['项目', '批次', '实例', '健康评分', '慢 SQL', '错误', '锁等待', '死锁', '开始'],
        rows, '当前范围暂无分析批次。'));
      get('health-dialog').showModal();
    }

    async function loadRecords() {
      const id = current;
      if (!id) return;
      const request = ++recordRequest;
      const query = new URLSearchParams(new FormData(filters));
      for (const field of ['from', 'to']) {
        const value = query.get(field);
        if (value) query.set(field, new Date(value).toISOString());
        else query.delete(field);
      }
      query.set('offset', String(page * 50));
      const data = await api(base + 'batches/' + id + '/records?' + query);
      if (!active || id !== current || request !== recordRequest) return;
      get('records').replaceChildren(table(['时间', '类型', '应用', 'SQL / 文件', '耗时', '原始证据'], data.records.map(row => {
        const content = element('div', undefined, 'log-record');
        if (row.sql) content.append(element('code', row.sql));
        content.append(element('small', row.file + ' · ' + row.offset));
        const type = [kinds[row.kind] || row.kind, row.sqlstate].filter(Boolean).join(' · ');
        const actions = element('div', undefined, 'log-actions');
        if (row.archiveExpiredAt) actions.append(element('small', '原文已到期'));
        else actions.append(button('查看原文', () => run(() => openOriginal(row.id))));
        actions.append(button('调查', () => run(() => investigate(row.id))));
        return [row.at ? formatTime(row.at) : '—', type, row.application || '未标识', content,
          row.durationMs == null ? '—' : number.format(row.durationMs) + ' ms', actions];
      }), '这个范围暂无日志记录。'));
      get('previous').disabled = page === 0;
      get('next').disabled = !data.more;
      get('page').textContent = '第 ' + (page + 1) + ' 页';
      get('record-paging').hidden = page === 0 && !data.more;
    }

    async function loadReport() {
      const id = current;
      if (!id) return;
      const request = ++reportRequest;
      const query = new URLSearchParams(new FormData(filters));
      for (const field of ['from', 'to']) {
        const value = query.get(field);
        if (value) query.set(field, new Date(value).toISOString());
        else query.delete(field);
      }
      const report = await api(base + 'batches/' + id + '/report?' + query);
      if (id !== current || !active || request !== reportRequest) return;
      const panel = get('report');
      const scope = [query.get('application'), query.get('fingerprint'), kinds[query.get('kind')]].filter(Boolean);
      panel.replaceChildren(element('h2', scope.length ? '性能下钻' : '全景分析'));
      if (scope.length) {
        const currentScope = element('div', undefined, 'log-actions');
        currentScope.append(element('span', scope.join(' · ')), button('清除下钻', () => focusReport({ application: '', fingerprint: '', kind: '' })));
        panel.append(currentScope);
      }
      reportCharts = [];
      if (!report.series.length) {
        panel.append(element('p', '当前范围暂无带时间的采样记录。', 'empty'));
        return;
      }
      const totals = report.summary;
      const rateWord = report.coverage === 'full' && totals.valid === totals.records ? '全量日志 SQL' : '已记录 SQL';
      const monitoring = report.monitoringCounters?.databaseTps;
      panel.append(element('p', [
        rateWord + ' ' + formatted(totals.statements) + ' 条',
        '覆盖 ' + coverageLabels[report.coverage],
        '延迟样本 ' + formatted(totals.timed) + ' 条',
        '缺少耗时 ' + formatted(totals.statements - totals.timed) + ' 条',
        'P95 ' + formatted(totals.p95) + ' ms', 'P99 ' + formatted(totals.p99) + ' ms',
        monitoring == null ? '显式事务记录 ' + formatted(totals.explicitTransactions) : '同期监控 TPS ' + formatted(monitoring)
      ].join(' · '), 'hint'));
      const rate = key => point => point[key] / report.bucketSeconds;
      const point = key => row => row[key];
      const definitions = [
        [rateWord + '读写速率', [['读 次/s', rate('read')], ['写 次/s', rate('write')]]],
        ['SQL 耗时', [['P95 ms', point('p95')], ['P99 ms', point('p99')]]],
        ['锁等待与死锁', [['锁等待', point('lock')], ['死锁', point('deadlock')]]],
        ['错误与显式事务速率', [['错误 次/s', rate('error')], ['显式事务 次/s', rate('transactions')]]]
      ];
      if (report.series.some(row => row.checkpoint || row.autovacuum)) definitions.push(
        ['Checkpoint 与 Autovacuum', [['Checkpoint', point('checkpoint')], ['Autovacuum', point('autovacuum')]]]);
      if (report.series.some(row => row.checkpointBuffers != null)) definitions.push(
        ['Checkpoint 刷脏', [['已写缓冲页', point('checkpointBuffers')]]]);
      if (report.series.some(row => row.temp)) definitions.push(
        ['临时文件', [['临时文件 MiB', row => row.tempBytes / 1048576], ['溢出记录', point('temp')]]]);
      const chartGrid = element('div', undefined, 'log-report-charts');
      reportCharts = definitions.map(([title, lines]) => logChart(title, report.series, lines,
        at => at ? new Date(at).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' }) : ''));
      chartGrid.append(...reportCharts.map(chart => chart.figure));
      panel.append(chartGrid);
      const block = (title, labels, rows, empty) => {
        const section = element('section', undefined, 'log-report-section');
        const scroll = element('div', undefined, 'log-scroll');
        scroll.append(table(labels, rows, empty));
        section.append(element('h3', title), scroll);
        panel.append(section);
      };
      if (totals.timed > 0) {
        const intervals = ['< 10 ms', '10–<50 ms', '50–<100 ms', '100–<500 ms',
          '500–<1,000 ms', '1–<5 s', '≥ 5 s'];
        const ranges = element('div', undefined, 'log-latency-profile');
        const counts = totals.latencyBuckets;
        intervals.forEach((label, index) => {
          const item = element('div');
          const meter = element('meter');
          meter.max = totals.timed;
          meter.value = counts[index];
          meter.setAttribute('aria-label', label + ' · ' + formatted(counts[index]) + ' 条');
          item.append(element('span', label), meter, element('strong', formatted(counts[index])));
          ranges.append(item);
        });
        panel.append(element('h3', 'SQL 延迟分布'), ranges);
      }
      if (report.examples.length) {
        block('代表 SQL 记录', ['时间', '应用', 'SQL', '耗时 ms', '证据'], report.examples.map(row => {
          const actions = element('div', undefined, 'log-actions');
          if (row.retained) actions.append(button('查看原文', () => run(() => openOriginal(row.id))));
          else actions.append(element('small', '原文已到期'));
          actions.append(button('调查', () => run(() => investigate(row.id))));
          return [row.at ? formatTime(row.at) : '—', row.application || '未标识',
            element('code', row.sql, 'log-record'), formatted(row.durationMs), actions];
        }), '当前窗口没有计时样本。');
      }
      block('Application 流量与资源矩阵', ['应用', '已记录 SQL', 'SQL 累计 ms', 'P95 ms', '锁等待 ms', '临时文件', '错误'],
        report.applications.map(app => [app.application ? button(app.application, () => focusReport({ application: app.application })) :
          '未标识', formatted(app.statements), formatted(app.sqlMs), formatted(app.p95), formatted(app.waitMs),
          app.tempBytes > 0 ? bytes(app.tempBytes) : '—', formatted(app.errors)]), '无可归属的应用记录。');
      block('Top 慢 SQL', ['归一化 SQL', '次数', '≥1 秒', '累计 ms', '最大 ms', 'P95 ms', '分析'], report.slowSQL.map(row =>
        [element('code', row.sql, 'log-record'), formatted(row.calls), formatted(row.slowCalls),
          formatted(row.totalMs), formatted(row.maxMs), formatted(row.p95),
          button('分析 SQL', () => focusReport({ fingerprint: row.fingerprint, kind: '' }))]), '当前日志缺少可比较的带耗时 SQL。');
      block('锁阻塞证据', ['时间', '等待者 → 阻塞者', '应用与锁对象', '等待 ms', '证据'], report.locks.map(row => {
        const chain = element('div', undefined, 'log-chain');
        chain.append(element('span', row.waiter || '未知会话'), element('strong', '→'),
          element('span', row.blocker || '阻塞者缺采'));
        const actions = element('div', undefined, 'log-actions');
        actions.append(button('查看原文', () => run(() => openOriginal(row.id))));
        actions.append(button('调查', () => run(() => investigate(row.id, '', row.nearbyEvent))));
        if (safeId(row.nearbyEvent)) actions.append(button('同窗事件', () => openEvent(row.nearbyEvent)));
        return [formatTime(row.at), chain, [row.application, row.object].filter(Boolean).join(' · ') || '未标识',
          formatted(row.durationMs), actions];
      }), '当前范围未记录锁阻塞。');
      requestAnimationFrame(repaint);
    }

    function focusReport(selection) {
      for (const [key, value] of Object.entries(selection)) filters.elements[key].value = value;
      page = 0;
      run(async () => {
        await Promise.all([loadRecords(), loadReport()]);
        if (active) get('report').scrollIntoView({ block: 'start' });
      });
    }

    async function openOriginal(id) {
      const selected = current;
      const result = await api(base + 'records/' + id + '/original', 'POST');
      if (!active || current !== selected) return;
      get('raw-info').textContent = result.file + ' · 偏移 ' + result.offset + (result.truncated ? ' · 已截取长记录' : '');
      get('raw-text').textContent = result.text;
      get('raw-dialog').showModal();
    }

    async function investigate(record, baselineRecord, event) {
      const batch = current;
      let data = await api(base + 'records/' + record + '/investigate', 'POST', { eventId: safeId(event) });
      if (safeId(baselineRecord)) {
        data = await api(base + 'investigations/' + data.id + '/records', 'POST', { recordId: baselineRecord });
      }
      if (active && current === batch) showInvestigation(data);
    }

    function showInvestigation(data) {
      caseId = data.id;
      saveSelection();
      get('investigation-status').textContent = '';
      const view = get('investigation');
      const diagnosis = data.diagnosis;
      view.replaceChildren(element('h2', diagnosis.priority + ' · 根因调查'),
        element('p', diagnosis.reason),
        element('p', [diagnosis.confidence, data.version, coverageLabels[data.coverage], '调查编号 ' + data.id].join(' · '), 'hint'));
      if (safeId(data.eventId)) {
        const link = element('a', '打开事件及 AI 证据问答', 'monitor-link');
        link.href = '#incidents?event=' + data.eventId;
        link.addEventListener('click', event => {
          event.preventDefault();
          const params = new URLSearchParams(location.hash.split('?')[1]);
          const observed = Date.parse(data.observedAt);
          if (Number.isFinite(observed)) {
            params.set('from', new Date(observed - 900000).toISOString());
            params.set('to', new Date(observed + 900000).toISOString());
          }
          openEvent(data.eventId, true, 'logs?' + params);
        });
        view.append(link);
      }
      view.append(element('h3', '调优与复测'));
      for (const item of diagnosis.recommendations) {
        const section = element('section', undefined, 'log-investigation-step');
        section.append(element('strong', item.title), element('p', item.action),
          element('p', '复测 · ' + item.validation, 'hint'), element('p', '回退 · ' + item.rollback, 'hint'));
        view.append(section);
      }
      view.append(element('h3', '同一调查的证据'));
      const scroll = element('div', undefined, 'log-scroll');
      scroll.append(table(['批次', '时间', '类型', 'SQL / 应用', '原文'], data.records.map(row => [
        row.batchName, row.at ? formatTime(row.at) : '—', kinds[row.kind] || row.kind,
        row.sql ? element('code', row.sql, 'log-record') : row.app || '—',
        row.archiveExpiredAt ? '原文已到期' : button('查看原文', () => run(() => openOriginal(row.id), get('investigation-status')))
      ]), '暂无关联证据。'));
      view.append(scroll);
      if (data.targetSQL) view.append(retestPanel(data));
      const dialog = get('investigation-dialog');
      if (!dialog.open) dialog.showModal();
    }

    function retestPanel(investigation) {
      const panel = element('section', undefined, 'log-report-section');
      panel.append(element('h3', '调优复测结果'));
      const batches = [...new Map(investigation.records.map(row => [row.batch, row.batchName]))];
      if (batches.length > 1) {
        const form = element('form', undefined, 'log-toolbar');
        function select(label, name, value) {
          const wrapper = element('label', label);
          const field = element('select');
          field.name = name;
          options(field, batches, value);
          wrapper.append(field);
          return wrapper;
        }
        const targetLabel = element('label', '目标 P95（毫秒）');
        const target = element('input');
        target.name = 'target';
        target.type = 'number';
        target.min = '0.01';
        target.max = '3600000';
        target.step = 'any';
        target.required = true;
        targetLabel.append(target);
        const save = element('button', '保存复测结果');
        save.type = 'submit';
        form.append(select('基线批次', 'before', batches[0][0]), select('复测批次', 'after', batches.at(-1)[0]), targetLabel, save);
        form.addEventListener('submit', event => {
          event.preventDefault();
          const baselineBatchId = form.elements.before.value;
          const currentBatchId = form.elements.after.value;
          if (baselineBatchId === currentBatchId) {
            get('investigation-status').textContent = '请选择两个批次。';
            return;
          }
          save.disabled = true;
          run(async () => {
            try {
              const url = base + 'investigations/' + investigation.id;
              await api(url + '/retests', 'POST', { baselineBatchId, currentBatchId, targetP95Ms: Number(target.value) });
              const data = await api(url);
              if (active && caseId === investigation.id) {
                showInvestigation(data);
                get('investigation-status').textContent = '复测结果已保存。';
              }
            } finally {
              save.disabled = false;
            }
          }, get('investigation-status'));
        });
        panel.append(form);
      } else {
        panel.append(element('p', '关联至少两个批次的同 SQL 样本后可保存复测。', 'hint'));
      }
      const scroll = element('div', undefined, 'log-scroll');
      scroll.append(table(['保存时间', '判定依据', '基线 P95', '复测 P95', '目标 P95', '报告'], investigation.retests.map(row => [
        formatTime(row.createdAt), [retestStates[row.status], row.reason].join(' · '),
        formatted(row.baseline.p95Ms), formatted(row.current.p95Ms), formatted(row.targetP95Ms),
        button('下载复测报告', () => downloadRetest(row))
      ]), '暂无复测记录。'));
      panel.append(scroll);
      return panel;
    }

    function downloadRetest(row) {
      function evidenceLink(sample) {
        const url = new URL(location.href);
        const params = { batch: sample.batchId, source: sample.recordId };
        if (row.eventId) params.incident = row.eventId;
        url.hash = 'logs?' + new URLSearchParams(params);
        return url.href;
      }
      const text = [
        '# DbSphere 调优复测', '', '复测编号 ' + row.id, '调查编号 ' + row.caseId,
        '事件编号 ' + (row.eventId || '离线调查'), '保存时间 ' + row.createdAt,
        '判定 ' + retestStates[row.status] + ' · ' + row.reason, '目标 P95 ' + row.targetP95Ms + ' ms', '',
        '| 样本 | 计时记录数 | P95（毫秒） | 原始证据 |', '| --- | --- | --- | --- |',
        ...[['基线', row.baseline], ['复测', row.current]].map(([label, sample]) =>
          '| ' + label + ' | ' + sample.timed + ' | ' + formatted(sample.p95Ms) +
          ' | [' + sample.recordId + '](' + evidenceLink(sample) + ') |'), ''
      ].join('\n');
      const link = element('a');
      link.href = URL.createObjectURL(new Blob([text], { type: 'text/markdown;charset=utf-8' }));
      link.download = 'dbsphere-retest-' + row.id + '.md';
      link.click();
      setTimeout(() => URL.revokeObjectURL(link.href), 1500);
    }

    function showUpload(file = '', resume) {
      if (uploading || !current) return;
      retryFile = file;
      resumeFile = resume?.id || '';
      const form = get('upload-form');
      form.reset();
      form.elements.files.required = !file;
      form.elements.files.multiple = !resumeFile;
      form.elements.node.value = resume?.node || '';
      get('file-label').hidden = !!file;
      get('node-label').hidden = !!file;
      get('preview').textContent = '';
      get('upload-status').textContent = resumeFile ? '请选择原文件 ' + resume.filename : '';
      get('progress').hidden = true;
      get('upload-title').textContent = file ? '重新解析文件' : resumeFile ? '继续上传' : '上传日志';
      get('upload-dialog').showModal();
    }

    function saveUpload(key, id) {
      if (id) pendingUploads.set(key, id);
      else pendingUploads.delete(key);
      try {
        if (id) localStorage.setItem(key, id);
        else localStorage.removeItem(key);
      } catch {}
    }

    async function savedUpload(key, batch, file) {
      let id = resumeFile || pendingUploads.get(key);
      try { id ||= localStorage.getItem(key); } catch {}
      if (!safeId(id)) return null;
      let status;
      try { status = await api(base + 'files/' + id + '/status'); }
      catch (error) {
        if (!String(error.message).includes('找不到该日志分析资源')) throw error;
        return null;
      }
      return status.batchId === batch && status.size === file.size && status.name === file.name ? status : null;
    }

    async function upload(form) {
      const batch = current;
      const { format, prefix, zone, node } = Object.fromEntries(new FormData(form));
      if (retryFile) {
        await api(base + 'files/' + retryFile + '/queue', 'POST', { format, prefix, zone });
      } else {
        const files = [...form.elements.files.files];
        get('progress').hidden = false;
        const total = files.reduce((sum, file) => sum + file.size, 0);
        let sent = 0;
        for (const file of files) {
          get('upload-status').textContent = file.name;
          const key = 'dbsphere:log-upload:' + JSON.stringify([batch, file.name, file.size, file.lastModified, node]);
          const resumed = await savedUpload(key, batch, file);
          if (!resumed && resumeFile) throw new Error('请选择原来的文件。已过期的上传需要重新提交。');
          const created = resumed || await api(base + 'batches/' + batch + '/files', 'POST',
            { name: file.name, size: file.size, format, prefix, zone, node });
          saveUpload(key, created.id);
          if (created.state !== 'uploading') {
            saveUpload(key, '');
            sent += file.size;
            get('progress').value = sent / total;
            continue;
          }
          for (let offset = 0, part = 0; offset < file.size; offset += created.chunkBytes, part++) {
            const block = file.slice(offset, offset + created.chunkBytes);
            if (part < created.parts && window.crypto?.subtle) {
              const digest = await crypto.subtle.digest('SHA-256', await block.arrayBuffer());
              const hash = Array.from(new Uint8Array(digest), octet => octet.toString(16).padStart(2, '0')).join('');
              if (hash !== created.digests[part]) {
                saveUpload(key, '');
                throw new Error('选择的文件与已上传内容不同。请重新提交此文件。');
              }
            } else {
              await api(base + 'files/' + created.id + '/blocks?part=' + part, 'PUT', block);
            }
            sent += block.size;
            get('progress').value = sent / total;
          }
          await api(base + 'files/' + created.id + '/queue', 'POST');
          saveUpload(key, '');
        }
      }
      get('upload-dialog').close();
      if (current === batch && active) await loadBatch();
    }

    for (const name of ['project', 'environment', 'instance']) {
      get(name).addEventListener('change', () => {
        selectBatch('');
        configureDirectory();
        run(loadBatches);
      });
    }
    get('batch').addEventListener('change', () => {
      selectBatch(get('batch').value);
      run(loadBatch);
    });
    get('refresh').addEventListener('click', () => run(loadDirectory));
    get('open-comparison').addEventListener('click', () => run(toggleComparison, get('compare-coverage')));
    get('baseline').addEventListener('change', () => run(() => toggleComparison(true), get('compare-coverage')));
    get('compare-filter').addEventListener('change', renderComparison);
    get('open-health').addEventListener('click', () => run(openHealth));
    const taskStatus = get('task-status');
    get('open-tasks').addEventListener('click', () => {
      get('tasks-dialog').showModal();
      run(loadTasks, taskStatus);
    });
    for (const name of ['task-refresh', 'task-first']) {
      get(name).addEventListener('click', () => run(loadTasks, taskStatus));
    }
    get('task-state').addEventListener('change', () => run(loadTasks, taskStatus));
    get('task-next').addEventListener('click', () => run(() => loadTasks(taskNext), taskStatus));

    async function loadTasks(before = null) {
      const request = ++taskRequest;
      const query = new URLSearchParams({ state: get('task-state').value });
      if (before) {
        query.set('before', before.createdAt);
        query.set('beforeId', before.id);
      }
      taskStatus.textContent = '正在读取导入任务';
      get('task-next').hidden = true;
      const data = await api(base + 'tasks?' + query);
      if (!active || !get('tasks-dialog').open || request !== taskRequest) return;
      const summary = data.summary;
      const limit = data.organizationScoped ? ' / ' + bytes(data.byteLimit) : ' · 单组织上限 ' + bytes(data.byteLimit);
      get('task-summary').textContent = [
        '上传中 ' + summary.uploading, '等待或解析 ' + summary.queued,
        '预留 ' + bytes(summary.reservedBytes) + limit,
        summary.oldestQueuedAt ? '最早等待自 ' + formatTime(summary.oldestQueuedAt) : ''
      ].filter(Boolean).join(' · ');
      get('tasks').replaceChildren(table(['文件', '项目 · 实例', '批次', '状态', '分块进度', '更新时间', '操作'], data.tasks.map(row => {
        const link = element('a', row.batch, 'monitor-link');
        link.href = '#logs?' + new URLSearchParams({ batch: row.batchId });
        link.addEventListener('click', () => get('tasks-dialog').close());
        const state = element('div', fileStates[row.state] || row.state);
        if (row.attempts) state.append(element('small', '自动重试 ' + row.attempts + ' 次'));
        if (row.reason) state.append(element('small', row.reason));
        if (row.state === 'queued' && Date.parse(row.nextAttemptAt) > Date.now()) {
          state.append(element('small', '下次重试 ' + formatTime(row.nextAttemptAt)));
        }
        const action = row.state === 'uploading' ? button('取消上传', () => {
          if (!confirm('取消此上传会释放未提交的分块。继续？')) return;
          run(async () => {
            await api(base + 'files/' + row.id + '/cancel', 'POST');
            if (current === row.batchId) await loadBatch();
            await loadTasks();
            taskStatus.textContent = '上传已取消，分块和配额已释放。';
          }, taskStatus);
        }) : '—';
        return [row.filename, [row.project, row.environment, row.instance].join(' · '), link, state,
          bytes(row.receivedBytes) + ' / ' + bytes(row.expectedBytes), formatTime(row.updatedAt), action];
      }), '当前范围没有导入任务。'));
      taskNext = data.next;
      get('task-next').hidden = !taskNext;
      get('task-first').hidden = !before;
      taskStatus.textContent = '';
    }
    get('open-archive').addEventListener('click', () => run(openArchive));
    get('rebuild').addEventListener('click', event => {
      const trigger = event.currentTarget;
      trigger.disabled = true;
      run(async () => {
        try {
          await api(base + 'batches/' + current + '/rebuild', 'POST');
          notice('对比索引已重建');
          if (active && comparing) await toggleComparison(true);
        } finally {
          trigger.disabled = false;
        }
      });
    });

    async function openArchive() {
      const data = await api(base + 'archive');
      if (!active) return;
      const dialog = get('archive-dialog');
      const status = get('archive-status');
      const rows = data.organizations.map(row => {
        const policy = element('form', undefined, 'log-actions');
        const days = element('input');
        days.type = 'number';
        days.min = '0';
        days.max = '3650';
        days.value = row.keepDays;
        days.required = true;
        days.setAttribute('aria-label', row.name + '原文保留天数');
        const action = element('div', undefined, 'log-actions');
        const estimate = element('p', '', 'hint');
        const save = element('button', '保存', 'ghost');
        let reviewedDays = null;
        save.disabled = true;
        days.addEventListener('input', () => {
          reviewedDays = null;
          save.disabled = true;
          estimate.textContent = '';
        });
        const preview = button('预览影响', () => {
          const keepDays = Number(days.value);
          if (!days.reportValidity() || (keepDays > 0 && keepDays < 7)) {
            estimate.textContent = '请输入 0 或 7 至 3650 天。';
            return;
          }
          run(async () => {
            const result = await api(base + 'archive/' + row.organizationId + '/preview?' + new URLSearchParams({ keepDays }));
            if (Number(days.value) !== keepDays || !policy.isConnected) return;
            estimate.textContent = ['预计到期 ' + result.expiringFiles + ' 个',
              '可释放 ' + bytes(result.reclaimableBytes), '调查保护 ' + result.protectedFiles + ' 个（' +
              bytes(result.protectedBytes) + '）'].join(' · ');
            reviewedDays = keepDays;
            save.disabled = false;
          }, estimate);
        });
        action.append(preview, save);
        policy.append(days, action, estimate);
        policy.addEventListener('submit', event => {
          event.preventDefault();
          const keepDays = Number(days.value);
          if (keepDays !== reviewedDays || save.disabled) return;
          if (keepDays > 0 && (row.keepDays === 0 || keepDays < row.keepDays) &&
            !confirm('超过新保留期限且未被调查引用的原文将自动清理。继续保存？')) return;
          run(async () => {
            await api(base + 'archive/' + row.organizationId + '/policy', 'POST', { keepDays });
            await openArchive();
            status.textContent = '保留期限已保存';
          }, status);
        });
        const ratio = row.rawSize > 0 ? number.format(row.storedSize / row.rawSize * 100) + '%' : '—';
        return [button(row.name, () => {
          archiveOrganization = row;
          archiveOffset = 0;
          get('archive-file-state').value = '';
          get('archive-files-title').textContent = row.name + ' · 归档文件';
          get('archive-files-dialog').showModal();
          run(loadArchiveFiles, get('archive-files-status'));
        }), bytes(row.rawSize), bytes(row.storedSize), ratio, row.uploading, row.queued, row.expired, policy];
      });
      get('archive-overview').replaceChildren(
        element('p', '每个组织的上传与待解析任务上限为 ' + data.uploadLimit + ' 个文件 / ' + bytes(data.pendingBytesLimit), 'hint'),
        table(['组织', '留存原文', '归档空间', '压缩后占比', '上传中', '待解析', '已到期', '保留天数'], rows, '暂无组织归档。'));
      if (!dialog.open) dialog.showModal();
    }

    async function loadArchiveFiles() {
      const request = ++archiveRequest;
      const selected = archiveOrganization;
      if (!selected) return;
      const state = get('archive-file-state').value;
      const status = get('archive-files-status');
      status.textContent = '正在读取归档';
      get('archive-files-prev').disabled = true;
      get('archive-files-next').disabled = true;
      const query = new URLSearchParams({ state, offset: archiveOffset });
      const result = await api(base + 'archive/' + selected.organizationId + '/files?' + query);
      if (!active || request !== archiveRequest || !get('archive-files-dialog').open) return;
      get('archive-files').replaceChildren(table(['文件', '批次', '原始 / 归档', '状态', '操作'], result.files.map(row => {
        const cell = element('div');
        cell.append(element('strong', row.file), element('small', row.project + ' · ' + row.instance));
        const action = element('div', undefined, 'log-actions');
        const link = element('a', row.protectionCaseId ? '查看保护调查' : '查看批次', 'monitor-link');
        const params = new URLSearchParams({ batch: row.batchId });
        if (row.protectionCaseId) params.set('case', row.protectionCaseId);
        link.href = '#logs?' + params;
        link.addEventListener('click', () => {
          get('archive-files-dialog').close();
          get('archive-dialog').close();
        });
        action.append(link);
        if (row.storedBytes > 0 && !row.expiredAt) action.append(button('校验归档', () => run(async () => {
          await api(base + 'files/' + row.id + '/verify', 'POST');
          await loadArchiveFiles();
          status.textContent = '归档校验通过。';
        }, status)));
        const state = [row.expiredAt ? '原文已到期' : '留存中', row.protectionCaseId ? '调查保护' : '',
          row.verifiedAt ? '校验 ' + formatTime(row.verifiedAt) : ''].filter(Boolean).join(' · ');
        return [cell, row.batch, bytes(row.rawBytes) + ' / ' + bytes(row.storedBytes), state, action];
      }), '这个范围没有归档文件。'));
      archiveStep = result.pageSize;
      get('archive-files-prev').disabled = archiveOffset === 0;
      get('archive-files-next').disabled = !result.more;
      status.textContent = '第 ' + (Math.floor(archiveOffset / archiveStep) + 1) + ' 页';
    }

    get('archive-file-state').addEventListener('change', () => {
      archiveOffset = 0;
      run(loadArchiveFiles, get('archive-files-status'));
    });
    get('archive-files-prev').addEventListener('click', () => {
      archiveOffset = Math.max(0, archiveOffset - archiveStep);
      run(loadArchiveFiles, get('archive-files-status'));
    });
    get('archive-files-next').addEventListener('click', () => {
      archiveOffset += archiveStep;
      run(loadArchiveFiles, get('archive-files-status'));
    });
    get('new-project').addEventListener('click', () => run(async () => {
      const form = get('project-form');
      form.reset();
      const data = await api('/api/admin/organizations');
      if (!active) return;
      options(form.elements.organizationId, data.organizations.map(item => [item.id, item.name]), '');
      get('project-dialog').showModal();
    }));
    get('new-instance').addEventListener('click', () => run(async () => {
      const form = get('instance-form');
      form.reset();
      const org = directory.projects.find(item => item.id === get('project').value)?.organizationId;
      const data = await api('/api/assets/page?limit=100');
      if (!active) return;
      options(form.elements.assetId, (data.assets || []).filter(item => item.organizationId === org)
        .map(item => [item.id, item.name]), '', '仅离线日志分析');
      get('instance-dialog').showModal();
    }));
    get('new-batch').addEventListener('click', () => {
      get('batch-form').reset();
      get('batch-dialog').showModal();
    });
    for (const name of ['project', 'instance', 'batch']) {
      const form = get(name + '-form');
      form.addEventListener('submit', event => {
        event.preventDefault();
        const submit = form.querySelector('button[type=submit]');
        submit.disabled = true;
        run(async () => {
          try {
            const details = Object.fromEntries(new FormData(form));
            if (name === 'instance') details.projectId = get('project').value;
            if (name === 'batch') details.instanceId = get('instance').value;
            const result = await api(base + name + (name === 'batch' ? 'es' : 's'), 'POST', details);
            get(name + '-dialog').close();
            if (name === 'project') get('project').add(new Option(result.name, result.id, false, true));
            if (name === 'instance') {
              get('environment').value = '';
              get('instance').add(new Option(result.name, result.id, false, true));
            }
            selectBatch(name === 'batch' ? result.id : '');
            await loadDirectory();
          } catch (error) {
            notice(error.message, true);
            throw error;
          } finally {
            submit.disabled = false;
          }
        });
      });
    }
    get('upload').addEventListener('click', () => showUpload());
    get('upload-form').elements.files.addEventListener('change', () => {
      const request = ++previewRequest;
      const files = [...get('upload-form').elements.files.files];
      run(async () => {
        const results = [];
        for (const file of files.slice(0, 5)) {
          const preview = await api(base + 'preview', 'POST', file.slice(0, 8192));
          results.push(file.name + ' · ' + (preview.format || '待确认') + ' · ' + preview.reason);
        }
        if (active && request === previewRequest) get('preview').replaceChildren(...results.map(text => element('div', text)));
      }, get('upload-status'));
    });
    get('upload-form').addEventListener('submit', event => {
      event.preventDefault();
      if (uploading) return;
      uploading = true;
      get('submit-upload').disabled = true;
      run(async () => {
        try {
          await upload(event.currentTarget || get('upload-form'));
        } finally {
          uploading = false;
          get('submit-upload').disabled = false;
        }
      }, get('upload-status'));
    });
    filters.addEventListener('submit', event => {
      event.preventDefault();
      page = 0;
      run(async () => { await loadRecords(); await loadReport(); });
    });
    get('previous').addEventListener('click', () => { page = Math.max(0, page - 1); run(loadRecords); });
    get('next').addEventListener('click', () => { page++; run(loadRecords); });
    for (const close of root.querySelectorAll('[data-log-close]')) close.addEventListener('click', () => close.closest('dialog').close());
    get('raw-dialog').addEventListener('close', () => {
      get('raw-info').textContent = '';
      get('raw-text').textContent = '';
      if (active && sourceId) {
        sourceId = '';
        saveSelection();
      }
    });
    get('investigation-dialog').addEventListener('close', () => {
      if (!active) return;
      caseId = '';
      saveSelection();
    });
    return { show(visible, params) {
      active = visible;
      generation++;
      clearTimeout(timer);
      if (!visible) {
        for (const dialog of root.querySelectorAll('dialog[open]')) dialog.close();
        return;
      }
      current = safeId(params?.get('batch')) || current;
      baseline = safeId(params?.get('baseline'));
      comparing = !!baseline;
      caseId = safeId(params?.get('case'));
      sourceId = safeId(params?.get('source'));
      returnIncident = safeId(params?.get('incident'));
      get('return-incident').hidden = !returnIncident;
      if (returnIncident) get('return-incident').href = '#incidents?' + new URLSearchParams({ event: returnIncident });
      const requestedCase = caseId, requestedSource = sourceId;
      run(async () => {
        await loadDirectory();
        if (active && requestedCase === caseId && caseId) {
          const data = await api(base + 'investigations/' + caseId);
          if (active && requestedCase === caseId) showInvestigation(data);
        }
        if (active && requestedSource === sourceId && sourceId) await openOriginal(sourceId);
      });
    } };
  }

  window.DbSphereLogAnalysis = { create };
})();
