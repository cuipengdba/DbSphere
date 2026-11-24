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

  function create(root, { api, formatTime, notice }) {
    const get = name => root.querySelector('[data-logs="' + name + '"]');
    const run = (job, target = get('status')) => void job().catch(error => {
      if (active) target.textContent = error.message;
    });
    let directory = { projects: [], instances: [] };
    let active = false, generation = 0, timer, page = 0, current = '', batches = [];
    let retryFile = '', uploading = false, previewRequest = 0, recordRequest = 0;
    const filters = get('record-filters');

    function selectBatch(id) {
      current = safeId(id);
      page = 0;
      filters.reset();
      if (active) history.replaceState(null, '', '#logs' + (current ? '?batch=' + current : ''));
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
      if (!current) {
        get('status').textContent = '新建批次后可以上传一组日志。';
        return;
      }
      get('upload').disabled = false;
      await loadBatch();
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
        let action = '—';
        if (file.state === 'failed') action = button('重新解析', () => showUpload(file.id));
        if (file.state === 'duplicate' && safeId(file.duplicateBatch)) {
          action = element('a', '查看原批次');
          action.href = '#logs?batch=' + file.duplicateBatch;
        }
        return [file.filename, [file.node, file.detectedFormat || file.format].filter(Boolean).join(' · '),
          state, file.parsedCount + ' / ' + file.recordCount, bytes(file.rawSize) + ' / ' + bytes(file.storedSize), action];
      }), '等待上传日志文件。'));
      await loadRecords();
      if (active && id === current && files.some(file => file.state === 'queued')) timer = setTimeout(() => run(loadBatch), 3200);
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
        return [row.at ? formatTime(row.at) : '—', type, row.application || '未标识', content,
          row.durationMs == null ? '—' : number.format(row.durationMs) + ' ms',
          button('查看原文', () => run(() => openOriginal(row.id)))];
      }), '这个范围暂无日志记录。'));
      get('previous').disabled = page === 0;
      get('next').disabled = !data.more;
      get('page').textContent = '第 ' + (page + 1) + ' 页';
      get('record-paging').hidden = page === 0 && !data.more;
    }

    async function openOriginal(id) {
      const selected = current;
      const result = await api(base + 'records/' + id + '/original', 'POST');
      if (!active || current !== selected) return;
      get('raw-info').textContent = result.file + ' · 偏移 ' + result.offset + (result.truncated ? ' · 已截取长记录' : '');
      get('raw-text').textContent = result.text;
      get('raw-dialog').showModal();
    }

    function showUpload(file = '') {
      if (uploading || !current) return;
      retryFile = file;
      const form = get('upload-form');
      form.reset();
      form.elements.files.required = !file;
      get('file-label').hidden = !!file;
      get('node-label').hidden = !!file;
      get('preview').textContent = '';
      get('upload-status').textContent = '';
      get('progress').hidden = true;
      get('upload-title').textContent = file ? '重新解析文件' : '上传日志';
      get('upload-dialog').showModal();
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
          const created = await api(base + 'batches/' + batch + '/files', 'POST',
            { name: file.name, size: file.size, format, prefix, zone, node });
          for (let offset = 0, part = 0; offset < file.size; offset += created.chunkBytes, part++) {
            const block = file.slice(offset, offset + created.chunkBytes);
            await api(base + 'files/' + created.id + '/blocks?part=' + part, 'PUT', block);
            sent += block.size;
            get('progress').value = sent / total;
          }
          await api(base + 'files/' + created.id + '/queue', 'POST');
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
      run(loadRecords);
    });
    get('previous').addEventListener('click', () => { page = Math.max(0, page - 1); run(loadRecords); });
    get('next').addEventListener('click', () => { page++; run(loadRecords); });
    for (const close of root.querySelectorAll('[data-log-close]')) close.addEventListener('click', () => close.closest('dialog').close());
    get('raw-dialog').addEventListener('close', () => {
      get('raw-info').textContent = '';
      get('raw-text').textContent = '';
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
      run(loadDirectory);
    } };
  }

  window.DbSphereLogAnalysis = { create };
})();
