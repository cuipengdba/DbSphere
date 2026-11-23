(() => {
  'use strict';

  const uuid = /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i;
  const hour = 3600000;
  const statusNames = { ok: '采样正常', unbound: '待绑定', unavailable: '采集中断', denied: '权限不足',
    not_found: '容器暂未发现', unsupported: '指标不可用', stale: '数据已过期', ambiguous: '采集源重复' };
  const findings = { cache_reread: '缓存回收与重读', writeback: '写回压力', io_limit: '接近 I/O 限流', node_congestion: '节点设备忙碌' };
  const number = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 });
  const clock = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
  const charts = [
    ['块设备读写', 'throughput', [['readBps', '读吞吐', 'blue'], ['writeBps', '写吞吐', 'write']]],
    ['文件缓存变化', 'bytes', [['active', 'Active file', 'active'], ['inactive', 'Inactive file', 'inactive'], ['file', 'File cache', 'blue']]],
    ['磁盘操作', 'ops', [['readIops', '读 IOPS', 'blue'], ['writeIops', '写 IOPS', 'write']]],
    ['I/O 与内存压力', 'percent', [['ioPressure', 'I/O PSI', 'write'], ['memoryPressure', '内存 PSI', 'inactive']]],
    ['脏页与写回', 'bytes', [['dirty', '脏页', 'blue'], ['writeback', '写回', 'write']]],
    ['文件重读与回收', 'ops', [['refault', 'Refault', 'inactive'], ['reclaims', '回收', 'active']]],
    ['PostgreSQL I/O 等待', 'count', [['pgIOWait', '等待会话', 'blue']]]
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
    return number.format(value) + ({ percent: '%', ops: ' /s', count: '' }[unit] || '');
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
    let route = { mode: 'live', hours: 1, asset: '', container: '', device: '', event: '', from: '', to: '' };
    const views = charts.map(([title, unit, fields]) => {
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
      canvas.addEventListener('pointermove', event => {
        if (!result?.buckets?.length) return;
        const fraction = Math.min(1, Math.max(0, (event.offsetX - 61) / Math.max(1, canvas.clientWidth - 72)));
        focus = Date.parse(result.from) + fraction * (Date.parse(result.to) - Date.parse(result.from));
        draw();
      });
      canvas.addEventListener('pointerleave', () => { focus = null; draw(); });
      canvas.addEventListener('dblclick', () => {
        if (!focus || !result) return;
        const duration = Date.parse(result.to) - Date.parse(result.from);
        if (duration <= 300000) return;
        route.mode = 'history';
        route.from = new Date(Math.max(Date.parse(result.from), focus - duration / 5)).toISOString();
        route.to = new Date(Math.min(Date.parse(result.to), focus + duration / 5)).toISOString();
        restart();
      });
      return { canvas, summary, unit, fields };
    });

    function routeHash() {
      const params = new URLSearchParams();
      for (const [key, value] of Object.entries(route)) if (value) params.set(key, value);
      return 'storage?' + params;
    }

    function settings() {
      for (const mode of ['live', 'history']) get(mode).setAttribute('aria-pressed', route.mode === mode ? 'true' : 'false');
      get('range').value = route.from && route.to ? 'custom' : String(route.hours);
      get('dates').hidden = !route.from || !route.to;
      get('compare-wrap').hidden = route.mode !== 'history';
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
      route = { mode: params.get('mode') === 'history' || custom ? 'history' : 'live',
        hours: [1, 6, 24, 168].includes(Number(params.get('hours'))) ? Number(params.get('hours')) : 1,
        asset: uuid.test(params.get('asset') || '') ? params.get('asset') : '',
        container: (params.get('container') || '').slice(0, 63), device: (params.get('device') || '').slice(0, 128),
        event: uuid.test(params.get('event') || '') ? params.get('event') : '',
        from: custom ? new Date(start).toISOString() : '', to: custom ? new Date(end).toISOString() : '' };
      result = comparison = undefined;
      refreshOptions();
      settings();
      render();
      const request = generation;
      api('/api/live/integrations').then(integrations => { if (request === generation) { grafana = integrations.grafana; showGrafana(); } }).catch(() => {});
      void load();
      poll = setInterval(() => { if (route.mode === 'live' && !document.hidden) void load(); }, 5000);
    }

    function refreshOptions() {
      const eligible = assets.filter(asset => ['k8s', 'linux'].includes(asset.runtime));
      if (!route.asset || (eligible.length && !eligible.some(asset => asset.id === route.asset))) {
        route.asset = (eligible.find(asset => asset.runtime === 'k8s') || eligible[0])?.id || '';
      }
      selectOptions(get('asset'), [['', '选择数据库资产'], ...eligible.map(asset => [asset.id, asset.name])], route.asset);
    }

    async function load() {
      if (!active || !route.asset || pending) return;
      pending = true;
      const request = generation;
      const endpoint = `/api/assets/${route.asset}/container-storage`;
      const to = route.to ? Date.parse(route.to) : Date.now();
      const from = route.from ? Date.parse(route.from) : to - route.hours * hour;
      const params = (a, b) => new URLSearchParams({ from: new Date(a).toISOString(), to: new Date(b).toISOString(), container: route.container, device: route.device });
      try {
        const tasks = [api(`${endpoint}?${params(from, to)}`)];
        if (route.mode === 'history' && get('compare').checked) tasks.push(api(`${endpoint}?${params(from - (to - from), from)}`));
        const values = await Promise.all(tasks);
        if (request !== generation || !active) return;
        [result, comparison] = values;
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
      if (clear) { result = comparison = undefined; render(); }
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
      const sections = [['containerIO', '容器磁盘 I/O'], ['containerCache', '文件缓存与内存回收'],
        ['containerVolumes', '节点与存储卷'], ['containerRCA', '存储等待与根因']];
      nav.replaceChildren(node('strong', 'Grafana 专项'), ...sections.map(([section, title]) => {
        const link = node('a', title);
        link.href = window.DbSphereLive.grafanaPath({ section, asset: route.asset, container: route.container,
          event: route.event || result?.events?.[0]?.id, from: Date.parse(result?.from), to: Date.parse(result?.to) });
        return link;
      }));
    }

    function render() {
      const latest = result?.latest, storage = latest?.storage;
      const containerNames = Object.keys(storage?.containers || {}).sort();
      const containerName = route.container || storage?.primary;
      const sample = storage?.containers?.[containerName];
      const devices = Object.entries(sample?.deviceRates || {}).sort(([, a], [, b]) => (b.readBps || 0) + (b.writeBps || 0) - (a.readBps || 0) - (a.writeBps || 0));
      selectOptions(get('container'), [['', 'PostgreSQL 容器'], ...containerNames.map(name => [name, name])], route.container);
      selectOptions(get('device'), [['', '全部可比设备'], ...Object.keys(sample?.devices || {}).sort().map(name => [name, name])], route.device);
      const outdated = route.mode === 'live' && Date.now() - Date.parse(sample?.observedAt) > Math.max(30, 2 * (storage?.cadenceSeconds || 60)) * 1000;
      const state = outdated ? 'stale' : sample?.status || storage?.status || 'unbound';
      setStatus(state);
      get('context').textContent = [storage?.runtime === 'k8s' ? 'Kubernetes' : storage?.runtime === 'docker' ? 'Docker' : '',
        storage?.namespace, storage?.pod, storage?.node, sample?.source].filter(Boolean).join(' / ') || '选择已接入容器采集的 PostgreSQL。';
      const values = route.device ? sample?.deviceRates?.[route.device] : sample?.rates;
      const stats = [['读吞吐', values?.readBps, 'throughput', 'blue'], ['写吞吐', values?.writeBps, 'throughput', 'write'],
        ['Active file', sample?.memory?.active_file, 'bytes', 'active'], ['Inactive file', sample?.memory?.inactive_file, 'bytes', 'inactive'],
        ['I/O 压力', sample?.pressure?.io_some_avg10, 'percent', 'write']];
      get('stats').replaceChildren(...stats.map(([title, value, unit, color]) => {
        const card = node('div', null, 'storage-stat'); card.dataset.color = color;
        card.append(node('span', title), node('strong', fmt(value, unit)), node('small', sample?.observedAt ? formatTime(sample.observedAt) : '暂无样本'));
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
      const volumeNodes = (storage?.pvcs || []).map(pvc => node('p', `${pvc.name} · ${pvc.phase || pvc.status || '未知'} · ${fmt(pvc.usage?.usedPercent, 'percent')}`));
      for (const [dev, rates] of Object.entries(latest?.nodeRates || {}).slice(0, 6)) {
        const io = (rates.reads || 0) + (rates.writes || 0);
        const ms = io && Number.isFinite(rates.readSeconds) && Number.isFinite(rates.writeSeconds) ? (rates.readSeconds + rates.writeSeconds) * 1000 / io : null;
        volumeNodes.push(node('p', `${dev} · 平均 ${fmt(ms, '')} ms · 忙碌 ${fmt(rates.ioSeconds * 100, 'percent')}`));
      }
      get('volumes').replaceChildren(...(volumeNodes.length ? volumeNodes : [node('p', '暂无节点设备或 PVC 样本。', 'storage-empty')]));
      const evidence = [node('p', `I/O 等待会话 ${fmt(storage?.pgIOWait, 'count')}`)];
      for (const code of storage?.finding?.codes || []) evidence.push(node('p', findings[code] || code, 'storage-finding'));
      for (const item of (latest?.pgSQL || []).slice(0, 3)) {
        if (item.queryId) evidence.push(node('p', 'SQL ' + item.queryId, 'storage-fingerprint'));
      }
      if (latest?.pgIO?.rows) evidence.push(node('p', `pg_stat_io ${latest.pgIO.rows.length} 项 · 计时${latest.pgIO.timing ? '开启' : '关闭'}`));
      get('evidence').replaceChildren(...evidence);
      const buckets = result?.buckets || [];
      const count = buckets.reduce((sum, row) => sum + row.available, 0);
      const total = buckets.reduce((sum, row) => sum + row.samples, 0);
      get('coverage').textContent = total ? `${count}/${total} 有效样本` : '暂无样本';
      const timeline = [];
      for (const event of result?.events || []) {
        const link = node('a', `${formatTime(event.at)}  ${event.title}`);
        link.href = '#incidents?' + new URLSearchParams({ event: event.id, asset: route.asset, from: result.from, to: result.to });
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
      for (const row of buckets.filter(item => item.identityChanged || item.counterReset).slice(-8)) {
        timeline.push(node('span', `${formatTime(row.at)}  ${row.identityChanged ? '容器实例变更' : '计数器重置'}`));
      }
      get('events').replaceChildren(...(timeline.length ? timeline : [node('p', '当前时段没有记录到存储相关事件。', 'storage-empty')]));
      showGrafana();
      requestAnimationFrame(draw);
    }

    function draw() {
      if (!active) return;
      const rows = result?.buckets || [], prior = comparison?.buckets || [];
      const start = Date.parse(result?.from), end = Date.parse(result?.to), previousShift = end - start;
      const styles = getComputedStyle(root);
      const dpr = Math.min(2, window.devicePixelRatio || 1);
      for (const { canvas, summary, fields, unit } of views) {
        const width = canvas.clientWidth, height = canvas.clientHeight;
        if (width < 80 || !height) continue;
        const ctx = canvas.getContext('2d');
        canvas.width = width * dpr; canvas.height = height * dpr;
        ctx.scale(dpr, dpr);
        const plot = { left: 61, top: 9, width: width - 72, height: height - 32 };
        const max = Math.max(1, ...[...rows, ...prior].flatMap(row => fields.map(([key]) => row.series?.[key]?.max || 0)));
        const x = at => plot.left + (at - start) / (end - start) * plot.width;
        const y = value => plot.top + (1 - value / max) * plot.height;
        ctx.strokeStyle = styles.getPropertyValue('--live-line'); ctx.fillStyle = styles.getPropertyValue('--live-muted'); ctx.font = '11px system-ui';
        for (let i = 0; i <= 4; i++) {
          const value = max * i / 4, line = y(value);
          ctx.beginPath(); ctx.moveTo(plot.left, line); ctx.lineTo(width - 10, line); ctx.stroke();
          ctx.fillText(fmt(value, unit), 2, line + 3, 55);
          if (start < end) ctx.fillText(clock.format(start + (end - start) * i / 4), Math.min(width - 39, plot.left + plot.width * i / 4), height - 3);
        }
        for (const [key, , color] of fields) {
          ctx.strokeStyle = styles.getPropertyValue('--store-' + color); ctx.lineWidth = 1.75;
          for (const [points, shift] of [[rows, 0], [prior, previousShift]]) {
            ctx.setLineDash(shift ? [4, 4] : []); ctx.beginPath();
            let last;
            for (const point of points) {
              const value = point.series?.[key]?.max, at = Date.parse(point.at) + shift;
              if (!Number.isFinite(value)) { last = undefined; continue; }
              if (!last || point.identityChanged || point.counterReset || at - last > (result.stepSeconds + 4) * 1500) ctx.moveTo(x(at), y(value));
              else ctx.lineTo(x(at), y(value));
              last = at;
            }
            ctx.stroke(); ctx.setLineDash([]);
          }
        }
        if (Number.isFinite(focus)) {
          ctx.strokeStyle = styles.getPropertyValue('--live-muted'); ctx.lineWidth = 1;
          ctx.beginPath(); ctx.moveTo(x(focus), plot.top); ctx.lineTo(x(focus), plot.top + plot.height); ctx.stroke();
        }
        const nearest = focus ? rows.reduce((a, b) => !a || Math.abs(Date.parse(b.at) - focus) < Math.abs(Date.parse(a.at) - focus) ? b : a, null) : rows.at(-1);
        summary.textContent = nearest ? [formatTime(nearest.at), ...fields.map(([key, label]) => `${label} ${fmt(nearest.series?.[key]?.max, unit)}`)].join(' · ') : '暂无样本';
      }
    }

    for (const mode of ['live', 'history']) get(mode).onclick = () => {
      route.mode = mode;
      if (mode === 'live') { route.from = route.to = ''; route.hours = 1; }
      restart();
    };
    for (const key of ['asset', 'container', 'device']) get(key).onchange = () => {
      route[key] = get(key).value;
      if (key === 'asset') { route.event = route.container = route.device = ''; }
      if (key === 'container') route.device = '';
      restart(true);
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
    new ResizeObserver(draw).observe(get('charts'));
    new MutationObserver(draw).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return { show, setAssets(value) { assets = value; if (active) { refreshOptions(); void load(); } } };
  }

  window.DbSphereContainerStorage = { create };
})();
