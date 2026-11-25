(() => {
  'use strict';

  const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i;
  const hour = 3600000;
  const statusNames = { ok: '采样正常', unbound: '待绑定', unavailable: '采集中断', denied: '权限不足',
    not_configured: '未配置采集源', error: '采集失败', forbidden: '权限不足',
    not_found: '容器暂未发现', unsupported: '指标不可用', stale: '数据已过期', ambiguous: '采集源重复' };
  const findings = { cache_reread: '缓存回收与重读', writeback: '写回压力', io_limit: '接近 I/O 限流', node_congestion: '节点设备忙碌' };
  const number = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 });
  const clock = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
  const topics = ['resources', 'containers', 'cache', 'claims'];
  const rangeHours = [0.25, 1, 6, 24, 168];
  const cacheFields = [['active', '活跃文件页', 'active'], ['inactive', '非活跃文件页', 'write']];
  const cacheEvidence = [['refault', '重读', 'ops'], ['reclaims', '回收', 'ops'], ['dirty', '脏页', 'bytes'],
    ['writeback', '写回', 'bytes'], ['ioPressure', 'I/O PSI', 'percent'], ['memoryPressure', '内存 PSI', 'percent']];
  const cacheKey = /^[0-9a-f-]{36}:.{0,63}$/i;
  const dayClock = new Intl.DateTimeFormat('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false });
  const charts = [
    ['块设备读写', 'throughput', [['readBps', '读吞吐', 'blue'], ['writeBps', '写吞吐', 'write']]],
    ['文件缓存变化', 'bytes', [['active', 'Active file', 'active'], ['inactive', 'Inactive file', 'inactive'], ['file', 'File cache', 'blue']]],
    ['磁盘操作', 'ops', [['readIops', '读 IOPS', 'blue'], ['writeIops', '写 IOPS', 'write']]],
    ['I/O 与内存压力', 'percent', [['ioPressure', 'I/O PSI', 'write'], ['memoryPressure', '内存 PSI', 'inactive']]],
    ['脏页与写回', 'bytes', [['dirty', '脏页', 'blue'], ['writeback', '写回', 'write']]],
    ['文件重读与回收', 'ops', [['refault', 'Refault', 'inactive'], ['reclaims', '回收', 'active']]],
    ['PostgreSQL I/O 等待', 'count', [['pgIOWait', '等待会话', 'blue']]]
  ];
  const resourceCharts = [
    ['节点 CPU 与内存 · node_exporter', 'percent', [['hostCpu', 'CPU', 'blue'], ['hostMemory', '内存', 'inactive']]],
    ['节点磁盘吞吐 · node_exporter', 'throughput', [['hostRead', '读', 'blue'], ['hostWrite', '写', 'write']]],
    ['节点网络 · node_exporter', 'throughput', [['hostRx', '接收', 'blue'], ['hostTx', '发送', 'active']]],
    ['Pod CPU · metrics.k8s.io', 'count', [['podCPU', '核', 'blue']]],
    ['Pod 内存 · metrics.k8s.io', 'bytes', [['podMemory', '内存', 'inactive']]]
  ];
  const claimCharts = [['所选 PVC 容量 · Kubelet', 'bytes', [['pvcUsed', '已用', 'write'], ['pvcCapacity', '容量', 'blue']]]];
  const deviceCharts = [
    ['节点设备延迟', 'ms', [['nodeReadMs', '读', 'blue'], ['nodeWriteMs', '写', 'write']]],
    ['节点设备 IOPS', 'ops', [['nodeReadIops', '读', 'blue'], ['nodeWriteIops', '写', 'write']]],
    ['节点设备吞吐', 'throughput', [['nodeReadBps', '读', 'blue'], ['nodeWriteBps', '写', 'write']]],
    ['节点设备平均队列深度', 'count', [['queueDepth', '队列深度', 'inactive']]],
    ['节点设备忙碌比例', 'percent', [['nodeBusy', '设备忙碌', 'active']]]
  ];
  const pgCharts = [
    ['PostgreSQL WAL 产生速率', 'throughput', [['walBps', 'WAL', 'active']]],
    ['PostgreSQL 临时写入增量', 'bytes', [['tempDelta', '临时写入', 'blue']]],
    ['PostgreSQL 检查点同步增量', 'ms', [['checkpointDelta', '同步耗时', 'write']]]
  ];

  function node(tag, text, className) {
    const result = document.createElement(tag);
    if (text != null) result.textContent = text;
    if (className) result.className = className;
    return result;
  }

  function fmt(value, unit) {
    if (!Number.isFinite(value)) return '—';
    if (unit === 'bytes' || unit === 'throughput') {
      const power = Math.min(4, Math.floor(Math.log(Math.max(value, 1)) / Math.log(1024)));
      return number.format(value / 1024 ** power) + ' ' + ['B', 'KiB', 'MiB', 'GiB', 'TiB'][power] + (unit === 'throughput' ? '/s' : '');
    }
    return number.format(value) + ({ percent: '%', ops: ' /s', ms: ' ms' }[unit] || '');
  }

  function selectOptions(control, choices, selected) {
    const key = JSON.stringify(choices);
    if (control.dataset.options !== key) {
      control.dataset.options = key;
      control.replaceChildren(...choices.map(([value, label]) => new Option(label, value)));
    }
    if (choices.some(([value]) => value === selected)) control.value = selected;
  }

  function create(root, { api, openEvent, formatTime }) {
    const get = name => root.querySelector(`[data-storage="${name}"]`);
    let assets = [], result, comparison, poll, active = false, pending = false, generation = 0, focus = null, grafana = false;
    let capacityAt = 0, capacity = [];
    let cachePins = [], cacheSeries = [], cacheLoadedAt = 0;
    const cacheViews = new Map();
    let route = { mode: 'live', topic: 'resources', hours: 1, asset: '', container: '', device: '', nodeDevice: '', pvc: '', pvcUid: '', event: '', from: '', to: '', cursor: '' };
    const viewSpecs = [...charts.slice(0, -1).map(spec => ['containers', spec]), ['', charts.at(-1)],
      ...resourceCharts.map(spec => ['resources', spec]), ...claimCharts.map(spec => ['claims', spec]),
      ...deviceCharts.map(spec => ['device', spec]), ...pgCharts.map(spec => ['containers', spec])];
    const views = viewSpecs.map(([topic, [title, unit, fields]]) => {
      const card = node('section', null, 'storage-chart');
      const hdr = node('header');
      hdr.append(node('h3', title));
      const legend = node('div', null, 'storage-legend');
      for (const [, label, color] of fields) {
        const item = node('span', label); item.dataset.color = color; legend.append(item);
      }
      hdr.append(legend);
      const canvas = node('canvas'); canvas.setAttribute('role', 'img'); canvas.setAttribute('aria-label', title + '时间趋势');
      const summary = node('p', '暂无样本', 'storage-chart-readout');
      card.append(hdr, canvas, summary);
      get('charts').append(card);
      bindCursor(canvas, () => result);
      canvas.addEventListener('dblclick', () => {
        if (!focus || !result) return;
        const duration = Date.parse(result.to) - Date.parse(result.from);
        if (duration <= 300000) return;
        route.mode = 'history';
        route.from = new Date(Math.max(Date.parse(result.from), focus - duration / 5)).toISOString();
        route.to = new Date(Math.min(Date.parse(result.to), focus + duration / 5)).toISOString();
        restart();
      });
      return { card, canvas, summary, unit, fields, topic };
    });

    function bindCursor(canvas, source) {
      canvas.tabIndex = 0;
      const pointAt = event => {
        const data = source();
        if (!data?.buckets?.length) return null;
        const fraction = Math.min(1, Math.max(0, (event.offsetX - 61) / Math.max(1, canvas.clientWidth - 72)));
        return Date.parse(data.from) + fraction * (Date.parse(data.to) - Date.parse(data.from));
      };
      const pin = at => {
        if (at !== null && !Number.isFinite(at)) return;
        focus = at;
        route.cursor = at == null ? '' : new Date(at).toISOString();
        history.replaceState(null, '', '#' + routeHash());
        draw();
      };
      canvas.addEventListener('pointermove', event => {
        if (!route.cursor) { focus = pointAt(event); draw(); }
      });
      canvas.addEventListener('pointerleave', () => { focus = route.cursor ? Date.parse(route.cursor) : null; draw(); });
      canvas.addEventListener('click', event => pin(pointAt(event)));
      canvas.addEventListener('keydown', event => {
        if (event.key === 'Escape' && route.cursor) { event.preventDefault(); pin(null); return; }
        if (!['ArrowLeft', 'ArrowRight'].includes(event.key)) return;
        const data = source();
        if (!data?.buckets?.length) return;
        event.preventDefault();
        const step = data.stepSeconds * 1000;
        const at = Number.isFinite(focus) ? focus + (event.key === 'ArrowLeft' ? -step : step) : Date.parse(data.buckets.at(-1).at);
        pin(Math.min(Date.parse(data.to), Math.max(Date.parse(data.from), at)));
      });
    }

    function routeHash() {
      const params = new URLSearchParams();
      for (const [key, value] of Object.entries(route)) if (value) params.set(key, value);
      for (const key of cachePins) params.append('watch', key);
      return 'storage?' + params;
    }

    function settings() {
      for (const mode of ['live', 'history']) get(mode).setAttribute('aria-pressed', route.mode === mode ? 'true' : 'false');
      for (const topic of topics) get(topic).setAttribute('aria-pressed', String(route.topic === topic));
      get('range').value = route.from && route.to ? 'custom' : String(route.hours);
      get('dates').hidden = !route.from || !route.to;
      get('compare-wrap').hidden = route.mode !== 'history' || route.topic === 'cache';
      for (const button of get('cache-ranges').querySelectorAll('[data-hours]'))
        button.setAttribute('aria-pressed', String(button.dataset.hours === get('range').value));
      if (route.from && route.to) {
        for (const key of ['from', 'to']) {
          const date = new Date(route[key]);
          get(key).value = new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
        }
      }
    }

    function show(value, params = new URLSearchParams()) {
      active = value;
      clearInterval(poll);
      generation++;
      pending = false;
      if (!value) return;
      const start = Date.parse(params.get('from')), end = Date.parse(params.get('to'));
      const custom = Number.isFinite(start) && Number.isFinite(end) && end > start;
      const back = params.get('return') || '';
      const requestedTopic = params.get('topic');
      route = { mode: params.get('mode') === 'history' || custom ? 'history' : 'live',
        topic: topics.includes(requestedTopic) ? requestedTopic : params.get('asset') ? 'containers' : 'resources',
        hours: rangeHours.includes(Number(params.get('hours'))) ? Number(params.get('hours')) : requestedTopic === 'cache' ? 24 : 1,
        asset: uuid.test(params.get('asset') || '') ? params.get('asset') : '',
        container: (params.get('container') || '').slice(0, 63), device: (params.get('device') || '').slice(0, 128),
        nodeDevice: params.get('device') ? '' : (params.get('nodeDevice') || '').slice(0, 128),
        pvc: (params.get('pvc') || '').slice(0, 253),
        pvcUid: (params.get('pvcUid') || '').slice(0, 128),
        return: back.length <= 5200 && (/^(overview|monitoring|live)(?:\?|$)/.test(back) ||
          back.startsWith('incidents?') && uuid.test(params.get('event') || '') && new URLSearchParams(back.slice(10)).get('event') === params.get('event')) ? back : '',
        event: uuid.test(params.get('event') || '') ? params.get('event') : '',
        from: custom ? new Date(start).toISOString() : '', to: custom ? new Date(end).toISOString() : '',
        cursor: Number.isFinite(Date.parse(params.get('cursor'))) ? new Date(params.get('cursor')).toISOString() : '' };
      focus = route.cursor ? Date.parse(route.cursor) : null;
      result = comparison = undefined;
      cachePins = [...new Set(params.getAll('watch').filter(value => cacheKey.test(value) && uuid.test(value.slice(0, 36))))].slice(0, 5);
      cacheSeries = [];
      cacheLoadedAt = 0;
      capacityAt = 0; capacity = [];
      refreshOptions();
      settings();
      render();
      const request = generation;
      api('/api/live/integrations').then(integrations => { if (request === generation) { grafana = integrations.grafana; showGrafana(); } }).catch(() => {});
      void load();
      poll = setInterval(() => {
        if (route.mode === 'live' && !document.hidden && (route.topic !== 'cache' || Date.now() - cacheLoadedAt >= 15000)) void load();
      }, 5000);
    }

    function refreshOptions() {
      const eligible = assets.filter(asset => ['k8s', 'linux'].includes(asset.runtime));
      if (!route.asset) {
        route.asset = (eligible.find(asset => asset.runtime === 'k8s') || eligible[0])?.id || '';
      }
      const choices = eligible.map(asset => [asset.id, asset.name]);
      if (route.asset && !eligible.some(asset => asset.id === route.asset)) choices.push([route.asset, '当前链接资产']);
      selectOptions(get('asset'), [['', '选择数据库资产'], ...choices], route.asset);
    }

    async function load() {
      if (!active || !route.asset || pending) return;
      pending = true;
      const request = generation;
      const endpoint = `/api/assets/${route.asset}/container-storage`;
      const to = route.to ? Date.parse(route.to) : Date.now();
      const from = route.from ? Date.parse(route.from) : to - route.hours * hour;
      const params = (a, b) => new URLSearchParams({ from: new Date(a).toISOString(), to: new Date(b).toISOString(),
        container: route.container, device: route.device, nodeDevice: route.nodeDevice, pvcUid: route.pvcUid });
      try {
        if (!assets.some(asset => asset.id === route.asset)) {
          const { asset } = await api(`/api/assets/${route.asset}`);
          if (request !== generation || !active) return;
          if (asset && ['k8s', 'linux'].includes(asset.runtime)) {
            assets = [...assets, asset];
            refreshOptions();
          }
        }
        if (route.topic === 'cache') {
          cacheLoadedAt = Date.now();
          const current = `${route.asset}:${route.container}`;
          const keys = [...new Set([...cachePins, current])];
          const responses = await Promise.allSettled(keys.map(key => api(`/api/assets/${key.slice(0, 36)}/container-storage?` +
            new URLSearchParams({ from: new Date(from).toISOString(), to: new Date(to).toISOString(), container: key.slice(37) }))));
          if (request !== generation || !active) return;
          cacheSeries = keys.map((key, index) => ({ key, data: responses[index].value }));
          result = cacheSeries.find(item => item.key === current)?.data;
          render();
          return;
        }
        const current = params(from, to);
        const forecast = route.pvc && Date.now() - capacityAt >= 60000;
        if (forecast) {
          current.set('forecast', '1');
          current.set('pvc', route.pvc);
        }
        const tasks = [api(`${endpoint}?${current}`)];
        if (route.mode === 'history' && get('compare').checked) tasks.push(api(`${endpoint}?${params(from - (to - from), from)}`));
        const values = await Promise.all(tasks);
        if (request !== generation || !active) return;
        [result, comparison] = values;
        if (forecast) {
          capacity = result.pvcCapacity || [];
          capacityAt = Date.now();
        }
        render();
      } catch (error) {
        if (request === generation && active) setStatus('unavailable', error.message);
      } finally {
        if (request === generation) pending = false;
      }
    }

    function restart(clear = false) {
      if (!active) return;
      generation++;
      pending = false;
      focus = route.cursor ? Date.parse(route.cursor) : null;
      capacityAt = 0; capacity = [];
      if (clear) { result = comparison = undefined; cacheSeries = []; render(); }
      settings();
      history.replaceState(null, '', '#' + routeHash());
      void load();
    }

    function setStatus(key, label) {
      get('status').dataset.state = key;
      get('status').textContent = label || statusNames[key] || '等待采样';
    }

    function showGrafana() {
      const nav = get('grafana');
      nav.hidden = !grafana || !route.asset;
      if (nav.hidden) return;
      const sections = route.topic === 'cache' ? [['containerCache', '文件缓存与内存回收']] :
        [['infrastructure', '节点与工作负载'], ['containerIO', '容器磁盘 I/O'], ['containerCache', '文件缓存与内存回收'],
          ['containerVolumes', '节点与存储卷'], ['containerRCA', '存储等待与根因']];
      nav.replaceChildren(node('strong', 'Grafana 专项'), ...sections.map(([section, title]) => {
        const link = node('a', title);
        link.href = window.DbSphereLive.grafanaPath({ section, asset: route.asset, container: route.container,
          device: route.device, pvc: route.pvc, pvcUid: route.pvcUid, back: routeHash(),
          event: route.event || result?.events?.[0]?.id, from: Date.parse(result?.from), to: Date.parse(result?.to) });
        return link;
      }));
    }

    function render() {
      const latest = result?.latest, storage = latest?.storage;
      const asset = assets.find(item => item.id === route.asset);
      const containerNames = Object.keys(storage?.containers || {}).sort();
      const containerName = route.container || storage?.primary;
      const sample = storage?.containers?.[containerName];
      const bareHost = asset?.runtime === 'linux' && !storage?.runtime;
      const devices = Object.entries(sample?.deviceRates || {}).sort(([, a], [, b]) => (b.readBps || 0) + (b.writeBps || 0) - (a.readBps || 0) - (a.writeBps || 0));
      selectOptions(get('container'), [['', 'PostgreSQL 容器'], ...containerNames.map(name => [name, name])], route.container);
      const deviceChoices = Object.keys(bareHost ? latest?.nodeRates || {} : sample?.devices || {}).sort().map(name => [name, name]);
      selectOptions(get('device'), [['', '全部可比设备'], ...deviceChoices], route.nodeDevice || route.device);
      get('device').dataset.scope = bareHost ? 'node' : 'container';
      const outdated = route.mode === 'live' && Date.now() - Date.parse(sample?.observedAt) > Math.max(30, 2 * (storage?.cadenceSeconds || 60)) * 1000;
      const resourceState = (asset?.runtime === 'k8s' ? latest?.kubernetes?.status : latest?.host?.status) || 'unbound';
      const resourceStale = route.mode === 'live' && Date.now() - Date.parse(latest?.at) > Math.max(30, 2 * (storage?.cadenceSeconds || 60)) * 1000;
      const state = route.topic === 'resources' ? resourceStale ? 'stale' : resourceState :
        outdated ? 'stale' : sample?.status || storage?.status || 'unbound';
      setStatus(state);
      const source = asset?.runtime === 'k8s' ? 'Kubernetes' : storage?.runtime === 'docker' ? 'Docker' : 'Linux';
      get('context').textContent = asset ? [asset.name, source, storage?.namespace, storage?.pod, storage?.node || asset.linuxHost].filter(Boolean).join(' / ') : '选择要查看的数据库资产。';
      const back = get('back');
      back.href = '#' + (route.return || 'overview');
      back.textContent = route.return?.startsWith('incidents') ? '返回事件证据' : route.return?.startsWith('monitoring') ? '返回 PostgreSQL 同窗' :
        route.return?.startsWith('live') ? '返回实时观测' : '返回全域总览';
      root.dataset.topic = route.topic;
      get('heading').textContent = route.topic === 'cache' ? '文件页缓存' : '主机、容器与存储';
      get('cache-board').hidden = route.topic !== 'cache';
      renderIdentity(asset, latest, sample);
      if (route.topic === 'cache') {
        renderCache();
        return;
      }
      for (const view of views) view.card.hidden = view.topic === 'device' ? !((route.nodeDevice || route.device) && ['resources', 'containers'].includes(route.topic)) :
        !!view.topic && (view.topic !== route.topic || (view.fields[0][0].startsWith('pod') && source !== 'Kubernetes') || (view.topic === 'claims' && !route.pvcUid));
      const values = route.device ? sample?.deviceRates?.[route.device] : sample?.rates;
      const claim = storage?.pvcs?.find(item => route.pvcUid && item.uid === route.pvcUid);
      const storageStats = [['读吞吐', values?.readBps, 'throughput', 'blue'], ['写吞吐', values?.writeBps, 'throughput', 'write'],
        ['Active file', sample?.memory?.active_file, 'bytes', 'active'], ['Inactive file', sample?.memory?.inactive_file, 'bytes', 'inactive'],
        ['I/O 压力', sample?.pressure?.io_some_avg10, 'percent', 'write']];
      const resources = source === 'Kubernetes' ? [['Pod CPU（核）', latest?.kubernetes?.podUsage?.cpuCores, 'count', 'blue'],
        ['Pod 内存', latest?.kubernetes?.podUsage?.memoryBytes, 'bytes', 'inactive']] :
        [['节点 CPU', latest?.host?.cpu, 'percent', 'blue'], ['节点内存', latest?.host?.memory, 'percent', 'inactive'],
          ['1 分钟负载', latest?.host?.load, 'count', 'write']];
      const claimStats = [['PVC 已用', claim?.usage?.usedBytes, 'bytes', 'write'],
        ['PVC 容量', claim?.usage?.capacityBytes, 'bytes', 'blue'], ['PVC 使用率', claim?.usage?.usedPercent, 'percent', 'inactive']];
      const stats = route.topic === 'resources' ? resources : route.topic === 'claims' ? claimStats : storageStats;
      get('stats').replaceChildren(...stats.map(([title, value, unit, color]) => {
        const card = node('div', null, 'storage-stat'); card.dataset.color = color;
        const at = route.topic === 'resources' ? latest?.at : route.topic === 'claims' ? claim?.usage?.observedAt : sample?.observedAt;
        card.append(node('span', title), node('strong', fmt(value, unit)), node('small', at ? formatTime(at) : '暂无样本'));
        return card;
      }));
      const maximum = Math.max(1, ...devices.map(([, d]) => (d.readBps || 0) + (d.writeBps || 0)));
      get('devices').replaceChildren(...(devices.length ? devices.slice(0, 8).map(([device, rates]) => {
        const button = node('button', null, 'storage-device'); button.type = 'button';
        button.setAttribute('aria-pressed', String(device === route.device));
        const bar = document.createElement('progress'); bar.max = maximum; bar.value = (rates.readBps || 0) + (rates.writeBps || 0);
        bar.setAttribute('aria-label', `${device} 读写活动`);
        button.append(node('strong', device), node('small', `${fmt(rates.readBps, 'throughput')} 读 · ${fmt(rates.writeBps, 'throughput')} 写`), bar);
        button.addEventListener('click', () => { route.device = route.device === device ? '' : device; restart(); });
        return button;
      }) : [node('p', '等待相邻样本计算设备速率。', 'storage-empty')]));
      const claims = (storage?.pvcs || []).filter(pvc => (!route.pvc || pvc.name === route.pvc) && (!route.pvcUid || pvc.uid === route.pvcUid));
      const forecast = new Map(capacity.map(item => [item.pvcUID, item]));
      const volumeNodes = [node('p', 'PVC 通过挂载和 UID 核对。设备等待按所在节点统计。', 'storage-empty')];
      if (route.pvc || route.pvcUid) {
        const clear = node('button', '查看全部 PVC', 'ghost small-button');
        clear.type = 'button';
        clear.addEventListener('click', () => { route.pvc = route.pvcUid = ''; restart(); });
        volumeNodes.push(clear);
        if (!claims.length) volumeNodes.push(node('p', '此时间窗的最新采样未包含所选 PVC UID。', 'storage-empty'));
      }
      for (const pvc of claims) {
        const select = node('button', `${pvc.name} · ${pvc.phase || pvc.status || '未知'} · ${fmt(pvc.usage?.usedPercent, 'percent')}`, 'storage-device');
        select.type = 'button';
        select.setAttribute('aria-pressed', String(route.pvc === pvc.name));
        select.addEventListener('click', () => {
          route.pvc = pvc.name; route.pvcUid = pvc.uid || ''; route.topic = 'claims'; restart();
        });
        const links = (storage.volumeLinks || []).filter(link => link.pvc === pvc.name);
        const identity = links.map(link => `${link.container} · ${link.status === 'claim_verified' ? '已核对 PVC UID' : '仅挂载声明'}${link.sharedInPod ? ' · 同 Pod 共享' : ''}`).join(' / ');
        const capacity = forecast.get(pvc.uid);
        if (identity) select.append(node('small', identity));
        if (capacity) {
          const estimate = capacity.status === 'estimated' ? ` · 预计 ${number.format(capacity.hoursToFull)} 小时耗尽`
            : capacity.status === 'no_growth' ? ' · 观测窗无净增长' : ' · 可比历史不足';
          select.append(node('small', `可用余量 ${fmt(capacity.remainingBytes, 'bytes')}${estimate}`));
        } else if (Number.isFinite(pvc.usage?.capacityBytes) && Number.isFinite(pvc.usage?.usedBytes)) {
          select.append(node('small', `可用余量 ${fmt(Math.max(0, pvc.usage.capacityBytes - pvc.usage.usedBytes), 'bytes')}`));
        }
        volumeNodes.push(select);
      }
      for (const link of sample?.deviceLinks || []) {
        if (route.device && route.device !== link.device) continue;
        volumeNodes.push(node('p', `${link.device} → 节点设备 ${link.nodeDevice} · 队列深度 ${fmt(link.queueDepth, 'count')} · ${formatTime(link.to)}`));
      }
      const ranked = Object.entries(latest?.nodeRates || {}).filter(([dev]) => !route.device || latest?.nodeNumbers?.[dev]?.number === route.device)
        .sort(([, a], [, b]) => (b.ioSeconds || 0) - (a.ioSeconds || 0));
      for (const [dev, rates] of ranked.slice(0, 6)) {
        const io = (rates.reads || 0) + (rates.writes || 0);
        const ms = io && Number.isFinite(rates.readSeconds) && Number.isFinite(rates.writeSeconds) ? (rates.readSeconds + rates.writeSeconds) * 1000 / io : null;
        const label = `${dev} · 平均 ${fmt(ms, 'ms')} · 忙碌 ${fmt(rates.ioSeconds * 100, 'percent')}`;
        if (bareHost) {
          const button = node('button', label, 'storage-device');
          button.type = 'button';
          button.onclick = () => { route.nodeDevice = dev; route.device = ''; route.topic = 'resources'; restart(); render(); };
          volumeNodes.push(button);
        } else volumeNodes.push(node('p', label));
      }
      get('volumes').replaceChildren(...(volumeNodes.length ? volumeNodes : [node('p', '暂无节点设备或 PVC 样本。', 'storage-empty')]));
      const finding = storage?.finding;
      const selectedPrimary = containerName && containerName === storage?.primary;
      const evidence = [node('p', `PostgreSQL I/O 等待 ${fmt(storage?.pgIOWait ?? result?.buckets?.at(-1)?.series?.pgIOWait?.max, 'count')} 个会话`)];
      if (selectedPrimary && finding?.intervalStart) {
        evidence.push(node('p', `${formatTime(finding.intervalStart)} 至 ${formatTime(finding.observedAt)} · 重读 ${fmt(finding.rates?.refaultPerSecond, 'ops')} · I/O 压力 ${fmt(finding.ioPressure, 'percent')}`, 'storage-empty'));
      }
      const codes = selectedPrimary ? finding?.codes || [] : [];
      for (const code of codes) evidence.push(node('p', findings[code] || code, 'storage-finding'));
      if (!codes.length) evidence.push(node('p', '当前证据尚未支持存储根因，缓存下降需与重读和等待同时核对。', 'storage-empty'));
      for (const item of (latest?.pgSQL || []).slice(0, 3)) {
        if (item.queryId) evidence.push(node('p', 'SQL ' + item.queryId, 'storage-fingerprint'));
      }
      if (latest?.pgIO?.rows) evidence.push(node('p', `pg_stat_io ${latest.pgIO.rows.length} 项 · 计时${latest.pgIO.timing ? '开启' : '关闭'}`));
      get('evidence').replaceChildren(...evidence);
      const buckets = result?.buckets || [];
      const key = route.topic === 'resources' ? (source === 'Kubernetes' ? 'pods' : 'hosts') : route.topic === 'claims' && route.pvcUid ? 'claims' : 'available';
      const count = buckets.reduce((sum, row) => sum + (row[key] || 0), 0);
      const total = buckets.reduce((sum, row) => sum + row.samples, 0);
      get('coverage').textContent = total ? `${count}/${total} 有效样本` : '暂无样本';
      const timeline = [];
      for (const event of result?.events || []) {
        const link = node('a', `${formatTime(event.at)}  ${event.title}`);
        link.href = '#incidents?' + new URLSearchParams({ event: event.id, asset: route.asset, origin: 'storage',
          from: result.from, to: result.to, container: route.container, device: route.device, pvc: route.pvc, pvcUid: route.pvcUid });
        link.addEventListener('click', click => {
          click.preventDefault(); route.event = event.id;
          if (document.fullscreenElement === root) void document.exitFullscreen().catch(() => {});
          const window = new URLSearchParams(routeHash().split('?')[1]);
          window.set('from', result.from);
          window.set('to', result.to);
          window.set('mode', 'history');
          void openEvent(event.id, true, 'storage?' + window);
        });
        timeline.push(link);
      }
      for (const row of buckets.filter(item => item.changeSampleId).slice(-8)) {
        const changes = [row.identityChanged && '容器实例变更', row.counterReset && '计数器重置', row.podChanged && 'Pod UID 变更',
          row.nodeChanged && '节点 UID 变更', row.hostRebooted && 'Linux 重启'].filter(Boolean);
        const link = node('a', `${formatTime(row.at)}  ${changes.join(' · ')}`);
        link.href = `/api/assets/${route.asset}/samples/${row.changeSampleId}`;
        link.target = '_blank'; link.rel = 'noopener';
        timeline.push(link);
      }
      get('events').replaceChildren(...(timeline.length ? timeline : [node('p', '当前时段没有记录到存储相关事件。', 'storage-empty')]));
      showGrafana();
      requestAnimationFrame(draw);
    }

    function cacheCard(key) {
      const card = node('article', null, 'storage-chart cache-card');
      const header = node('header');
      const title = node('h3');
      const identity = node('small', '', 'cache-identity');
      const remove = node('button', '移除', 'ghost small-button');
      remove.type = 'button';
      remove.onclick = () => { cachePins = cachePins.filter(item => item !== key); restart(true); };
      header.append(title, identity, remove);
      const readings = node('div', null, 'cache-readings');
      const values = cacheFields.map(([, label, color]) => {
        const reading = node('div'); reading.dataset.color = color;
        const value = node('strong', '—');
        reading.append(node('span', label), value); readings.append(reading);
        return value;
      });
      const canvas = node('canvas'); canvas.setAttribute('role', 'img');
      const summary = node('p', '暂无样本', 'storage-chart-readout');
      const source = node('small', '', 'cache-sample');
      const evidence = node('p', '', 'cache-note');
      const raw = node('a', '查看时间桶原始采样', 'monitor-link');
      raw.hidden = true;
      card.append(header, readings, canvas, summary, evidence, source, raw);
      const view = { card, title, identity, remove, values, source, canvas, summary, evidence, raw, fields: cacheFields, unit: 'bytes', cache: true };
      bindCursor(canvas, () => view.data);
      return view;
    }

    function renderCache() {
      const area = get('cache-tiles');
      const current = `${route.asset}:${route.container}`;
      const first = cacheSeries.find(item => item.data)?.data;
      get('cache-window').textContent = first ? `${formatTime(first.from)} — ${formatTime(first.to)} · 所有容器同一纵轴` : '等待文件缓存采样';
      get('pin-cache').disabled = !route.asset || cachePins.includes(current) || cachePins.length >= 5;
      get('only-cache').disabled = !cachePins.length;
      const identities = new Set();
      const cards = [];
      for (const { key, data } of cacheSeries) {
        const asset = assets.find(row => row.id === key.slice(0, 36));
        const storage = data?.latest?.storage;
        const name = key.slice(37) || storage?.primary || '';
        const identity = `${key.slice(0, 36)}:${name}`;
        if (identities.has(identity)) continue;
        identities.add(identity);
        const sample = storage?.containers?.[name];
        const view = cacheViews.get(key) || cacheCard(key);
        cacheViews.set(key, view);
        view.data = data;
        view.title.textContent = name || asset?.name || 'PostgreSQL 容器';
        view.identity.textContent = [name && asset?.name, storage?.pod, sample?.id?.split('://').at(-1).slice(0, 16)].filter(Boolean).join(' · ');
        view.remove.hidden = !cachePins.includes(key);
        view.remove.setAttribute('aria-label', '移除 ' + view.title.textContent);
        view.canvas.setAttribute('aria-label', `${view.title.textContent} 文件页缓存趋势`);
        for (let i = 0; i < cacheFields.length; i++) {
          const amount = sample?.status === 'ok' ? sample.memory?.[cacheFields[i][0] + '_file'] : undefined;
          view.values[i].textContent = fmt(amount, 'bytes');
        }
        const stale = route.mode === 'live' && Date.now() - Date.parse(sample?.observedAt) > Math.max(30, 2 * (storage?.cadenceSeconds || 60)) * 1000;
        const state = stale ? statusNames.stale : statusNames[sample?.status];
        view.source.textContent = sample?.observedAt ? `采样 ${formatTime(sample.observedAt)} · ${state || '状态未知'}` :
          data ? '此时段没有所选容器的样本' : '暂时无法读取此容器';
        cards.push(view.card);
      }
      for (const [key, view] of cacheViews) if (!cards.includes(view.card)) cacheViews.delete(key);
      area.dataset.count = String(cards.length);
      if (!cards.length) area.replaceChildren(node('p', '选择已纳管的容器以查看文件页缓存。', 'storage-empty'));
      else if (area.children.length !== cards.length || !cards.every((card, index) => area.children[index] === card)) area.replaceChildren(...cards);
      setStatus(first ? 'ok' : 'unavailable', first ? `${route.mode === 'live' ? '实时' : '历史'} · ${cards.length} 个容器` : '等待采样');
      showGrafana();
      requestAnimationFrame(draw);
    }

    function renderIdentity(asset, latest, sample) {
      const storage = latest?.storage;
      const chain = node('div', '', 'storage-topology');
      const actions = node('div', '', 'storage-topology-actions');
      const add = (label, value, detail, action) => {
        if (!value) return;
        const item = node(action ? 'button' : 'span', null, 'storage-identity-item');
        item.append(node('small', label), node('strong', value));
        if (detail) item.append(node('small', detail));
        if (action) { item.type = 'button'; item.onclick = action; }
        chain.append(item);
      };
      const openResources = () => { route.topic = 'resources'; restart(); render(); };
      const pod = storage?.pod || asset?.pod;
      const host = storage?.node || asset?.linuxHost;
      const returnToEvent = route.return?.startsWith('incidents?') ? route.return : '';
      const monitoring = new URLSearchParams({ instance: route.asset, tab: 'infra' });
      if (result?.from && result?.to) {
        monitoring.set('from', result.from);
        monitoring.set('to', result.to);
      }
      if (route.event && returnToEvent) {
        monitoring.set('event', route.event);
        monitoring.set('incidentReturn', returnToEvent);
      }
      add('数据库实例', asset?.name, latest?.at ? '窗口内最近一次采样 · ' + formatTime(latest.at) : '暂无采样', () => {
        location.hash = 'monitoring?' + monitoring;
      });
      add('容器', route.container || storage?.primary, sample?.id ? `容器 ID ${sample.id}` : '容器身份未核对', () => {
        route.topic = 'containers'; restart(); render();
      });
      add('Kubernetes Pod', pod, storage?.podUID ? `${storage.namespace || ''} · UID ${storage.podUID}` : 'Pod UID 未核对', openResources);
      add(host && pod ? 'Kubernetes Node' : 'Linux 主机', host || latest?.host?.endpoint,
        storage?.nodeUID ? `UID ${storage.nodeUID}` : latest?.host?.endpoint || '节点采集源未知', openResources);
      for (const link of (sample?.deviceLinks || []).slice(0, 4)) {
        add('节点块设备', link.nodeDevice, `已核对设备号 ${link.device} · ${formatTime(link.to)}`, () => {
          route.device = link.device; route.nodeDevice = ''; route.topic = 'containers'; restart(); render();
        });
      }
      if (sample?.id && !(sample.deviceLinks || []).length) add('节点块设备', '映射待核对', '未找到同节点的设备号证据');
      if (route.nodeDevice) add('节点块设备', route.nodeDevice, 'node_exporter 同节点采样');
      for (const claim of (storage?.pvcs || []).slice(0, 4)) {
        const links = (storage.volumeLinks || []).filter(link => link.pvc === claim.name && link.container === (route.container || storage.primary));
        const verified = claim.uid && links.some(link => link.status === 'claim_verified');
        const scope = [verified ? `已核对 UID ${claim.uid}` : '挂载归属待核对', links.some(link => link.sharedInPod) ? '同 Pod 共享' : ''].filter(Boolean).join(' · ');
        add('持久卷声明', claim.name, scope, () => {
          route.pvc = claim.name; route.pvcUid = claim.uid || ''; route.topic = 'claims'; restart(); render();
        });
      }
      if (asset && latest?.sampleId) {
        const raw = node('a', '原始采样');
        raw.href = `/api/assets/${asset.id}/samples/${latest.sampleId}`;
        raw.target = '_blank'; raw.rel = 'noopener';
        actions.append(raw);
      }
      const cursor = node('a', '查看游标采样', 'monitor-link');
      cursor.dataset.storage = 'cursor-sample';
      cursor.hidden = true;
      actions.append(cursor);
      const buckets = result?.buckets || [];
      const total = buckets.reduce((sum, row) => sum + row.samples, 0);
      const coverage = node('div', '', 'storage-topology-coverage');
      const candidates = [['hosts', 'Linux / node_exporter', latest?.host?.status],
        ['pods', 'Kubernetes API', latest?.kubernetes?.status],
        ['available', '容器 / cgroup', sample?.status || storage?.status]];
      for (const [key, label, state] of candidates) {
        if (!state && !(key === 'pods' && asset?.runtime === 'k8s')) continue;
        const valid = buckets.reduce((sum, row) => sum + (row[key] || 0), 0);
        coverage.append(node('small', `${label} · ${valid}/${total} 有效 · ${statusNames[state] || '缺采'}`));
      }
      if (sample?.observedAt && latest?.at) {
        const skew = Math.round((Date.parse(latest.at) - Date.parse(sample.observedAt)) / 1000);
        if (Number.isFinite(skew)) coverage.append(node('small', `容器采样与平台采样相差 ${Math.abs(skew)} 秒`));
      }
      get('identity').replaceChildren(node('h3', '跨层身份与采集覆盖'), chain, coverage, actions);
    }

    function draw() {
      if (!active) return;
      const styles = getComputedStyle(root);
      const dpr = Math.min(2, window.devicePixelRatio || 1);
      const cacheMax = Math.max(1, ...cacheSeries.flatMap(({ data }) => (data?.buckets || [])
        .flatMap(row => cacheFields.map(([key]) => row.series?.[key]?.max || 0))));
      const cursorRow = rowAt(result);
      const cursorLink = get('cursor-sample');
      if (cursorLink) {
        cursorLink.hidden = !Number.isFinite(focus) || !cursorRow?.latestSampleId;
        if (!cursorLink.hidden) cursorLink.href = `/api/assets/${route.asset}/samples/${cursorRow.latestSampleId}`;
      }
      for (const { canvas, summary, fields, unit, cache, data, evidence, raw } of route.topic === 'cache' ? cacheViews.values() : views) {
        const source = cache ? data : result;
        const rows = source?.buckets || [], prior = cache ? [] : comparison?.buckets || [];
        const start = Date.parse(source?.from), end = Date.parse(source?.to), previousShift = end - start;
        const reading = cache ? 'mean' : 'max';
        const cadence = source?.latest?.storage?.cadenceSeconds || 60;
        const maxGap = Math.max((source?.stepSeconds || 0) + 4, cadence) * 1500;
        const width = canvas.clientWidth, height = canvas.clientHeight;
        if (width < 80 || !height) continue;
        const ctx = canvas.getContext('2d');
        canvas.width = width * dpr; canvas.height = height * dpr;
        ctx.scale(dpr, dpr);
        const plot = { left: 61, top: 9, width: width - 72, height: height - 32 };
        const max = cache ? cacheMax : Math.max(1, ...[...rows, ...prior].flatMap(row => fields.map(([key]) => row.series?.[key]?.max || 0)));
        canvas.dataset.scaleMax = String(max);
        const x = at => plot.left + (at - start) / (end - start) * plot.width;
        const y = value => plot.top + (1 - value / max) * plot.height;
        ctx.strokeStyle = styles.getPropertyValue('--live-line'); ctx.fillStyle = styles.getPropertyValue('--live-muted'); ctx.font = '11px system-ui';
        for (let i = 0; i <= 4; i++) {
          const value = max * i / 4, line = y(value);
          ctx.beginPath(); ctx.moveTo(plot.left, line); ctx.lineTo(width - 10, line); ctx.stroke();
          ctx.fillText(fmt(value, unit), 2, line + 3, 55);
          if (start < end) {
            const time = (cache && previousShift >= 24 * hour ? dayClock : clock).format(start + previousShift * i / 4);
            ctx.fillText(time, Math.min(width - ctx.measureText(time).width, plot.left + plot.width * i / 4), height - 3);
          }
        }
        for (const [key, , color] of fields) {
          ctx.strokeStyle = styles.getPropertyValue('--store-' + color); ctx.lineWidth = 1.75;
          if (cache && rows.length) {
            ctx.fillStyle = ctx.strokeStyle; ctx.globalAlpha = .18;
            const bar = Math.max(1, Math.min(6, plot.width * source.stepSeconds * 1000 / previousShift));
            for (const row of rows) {
              const band = row.series?.[key];
              if (band?.max > band?.min) ctx.fillRect(x(Date.parse(row.at)) - bar / 2, y(band.max), bar, y(band.min) - y(band.max));
            }
            ctx.globalAlpha = 1;
          }
          for (const [points, shift] of [[rows, 0], [prior, previousShift]]) {
            ctx.setLineDash(shift ? [4, 4] : []); ctx.beginPath();
            let last;
            for (const point of points) {
              const value = point.series?.[key]?.[reading], at = Date.parse(point.at) + shift;
              if (!Number.isFinite(value)) { last = undefined; continue; }
              if (!last || point.identityChanged || point.counterReset || at - last > maxGap) {
                if (cache) { ctx.fillStyle = ctx.strokeStyle; ctx.fillRect(x(at) - 1, y(value) - 1, 2, 2); }
                ctx.moveTo(x(at), y(value));
              } else ctx.lineTo(x(at), y(value));
              last = at;
            }
            ctx.stroke(); ctx.setLineDash([]);
          }
        }
        if (Number.isFinite(focus) && focus >= start && focus <= end) {
          ctx.strokeStyle = styles.getPropertyValue('--live-muted'); ctx.lineWidth = 1;
          ctx.beginPath(); ctx.moveTo(x(focus), plot.top); ctx.lineTo(x(focus), plot.top + plot.height); ctx.stroke();
        }
        const nearest = rowAt(source);
        summary.textContent = nearest ? [formatTime(nearest.at), ...fields.map(([key, label]) => `${label} ${fmt(nearest.series?.[key]?.[reading], unit)}`)].join(' · ') :
          Number.isFinite(focus) ? `${formatTime(focus)} · 该时间桶无采样` : '暂无样本';
        if (cache) {
          evidence.textContent = nearest ? [...cacheEvidence.map(([key, label, unit]) => `${label} ${fmt(nearest.series?.[key]?.mean, unit)}`),
            nearest.identityChanged ? '容器身份变化' : '', nearest.counterReset ? '计数器重置' : ''].filter(Boolean).join(' · ') : '';
          raw.hidden = !nearest?.latestSampleId;
          if (!raw.hidden) raw.href = `/api/assets/${source.assetId}/samples/${nearest.latestSampleId}`;
        }
      }
    }

    function rowAt(source) {
      if (!Number.isFinite(focus)) return source?.buckets?.at(-1);
      const step = source?.stepSeconds * 1000;
      return source?.buckets?.find(row => { const at = Date.parse(row.at); return focus >= at && focus < at + step; });
    }

    for (const mode of ['live', 'history']) get(mode).onclick = () => {
      route.mode = mode;
      if (mode === 'live') { route.from = route.to = route.cursor = ''; route.hours = 1; }
      restart();
    };
    for (const topic of topics) get(topic).onclick = () => { route.topic = topic; restart(); render(); };
    get('pin-cache').onclick = () => {
      const key = `${route.asset}:${route.container}`;
      if (!route.asset || cachePins.includes(key) || cachePins.length >= 5) return;
      cachePins.push(key);
      restart();
      renderCache();
    };
    get('only-cache').onclick = () => { cachePins = []; restart(true); };
    for (const button of get('cache-ranges').querySelectorAll('[data-hours]')) button.onclick = () => {
      if (button.dataset.hours === 'custom') { get('dates').hidden = false; get('from').focus(); return; }
      route.hours = Number(button.dataset.hours);
      route.from = route.to = '';
      restart(true);
    };
    for (const key of ['asset', 'container']) get(key).onchange = () => {
      route[key] = get(key).value;
      if (key === 'asset') { route.event = route.container = route.pvc = route.pvcUid = ''; }
      route.device = route.nodeDevice = '';
      restart(true);
    };
    get('device').onchange = () => {
      const device = get('device');
      route.nodeDevice = device.dataset.scope === 'node' ? device.value : '';
      route.device = route.nodeDevice ? '' : device.value;
      restart(); render();
    };
    get('range').onchange = () => {
      if (get('range').value === 'custom') { get('dates').hidden = false; get('from').focus(); return; }
      route.hours = Number(get('range').value); route.from = route.to = '';
      restart();
    };
    get('dates').onsubmit = event => {
      event.preventDefault();
      const from = Date.parse(get('from').value), to = Date.parse(get('to').value);
      if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from) { setStatus('unavailable', '请选择有效的起止时间'); return; }
      route.from = new Date(from).toISOString(); route.to = new Date(to).toISOString(); route.mode = 'history'; restart();
    };
    get('compare').onchange = () => restart();
    get('full').onclick = () => {
      if (document.fullscreenElement === root) void document.exitFullscreen();
      else if (root.requestFullscreen) void root.requestFullscreen().catch(() => {});
    };
    document.addEventListener('fullscreenchange', () => get('full').setAttribute('aria-pressed', String(document.fullscreenElement === root)));
    const resize = new ResizeObserver(draw);
    resize.observe(get('charts'));
    resize.observe(get('cache-tiles'));
    new MutationObserver(draw).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return { show, contextRoute() {
      const params = new URLSearchParams(routeHash().split('?')[1]);
      if (result?.from && result?.to) { params.set('from', result.from); params.set('to', result.to); }
      if (Number.isFinite(focus)) params.set('cursor', new Date(focus).toISOString());
      return 'storage?' + params;
    }, setAssets(value) { assets = value; if (active) { refreshOptions(); void load(); } } };
  }

  window.DbSphereContainerStorage = { create };
})();
