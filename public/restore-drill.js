'use strict';

window.DbSphereRestoreDrill = (() => {
  const states = {requested:'待审批', approved:'已批准', rejected:'已驳回', started:'已登记开工', verified:'证据已核对', failed:'演练失败'};

  function create(asset, selected, {api, role, formatTime}) {
    function node(tag, text) {
      const element = document.createElement(tag);
      if (text) element.textContent = text;
      return element;
    }
    function link(label, view, values) {
      const anchor = node('a', label);
      anchor.href = '#' + view + '?' + new URLSearchParams(values);
      return anchor;
    }
    const panel = node('details');
    panel.className = 'settings-details';
    panel.dataset.restoreDrill = '';
    panel.append(node('summary', '备份恢复演练'));
    const status = node('p');
    status.setAttribute('role', 'status');
    const body = node('div');
    panel.append(status, body);
    const endpoint = `/api/assets/${asset.id}/restore-drills`;
    const canOperate = role === 'admin' || role === 'operator';
    let loading = false;
    let loaded = false;

    async function submit(path, data) {
      if (loading) return;
      loading = true;
      panel.setAttribute('aria-busy', 'true');
      try {
        const result = await api(endpoint + path, 'POST', data);
        selected = result.id;
        await load();
        status.textContent = '已保存';
      } catch (error) {
        status.textContent = error.message;
      } finally {
        loading = false;
        panel.removeAttribute('aria-busy');
      }
    }
    function form(label, path, fields, convert = data => data) {
      const element = node('form');
      element.className = 'response-form';
      for (const [name, title, type, value, constraints] of fields) {
        const wrapper = node('label', title);
        const input = node('input');
        Object.assign(input, {name, type, required:true}, constraints);
        if (value !== undefined) input.value = value;
        wrapper.append(input);
        element.append(wrapper);
      }
      const button = node('button', label);
      button.type = 'submit';
      element.append(button);
      element.addEventListener('submit', event => {
        event.preventDefault();
        try {
          void submit(path, convert(Object.fromEntries(new FormData(element))));
        } catch (error) {
          status.textContent = error.message;
        }
      });
      return element;
    }
    function report(row) {
      const report = row.report;
      const proof = report.proof;
      const lines = [row.title, `演练 ID ${row.id}`, `资产 ID ${asset.id}`, `事件 ${row.eventId || '已过保留期'}`,
        `隔离目标 ${row.targetId} · ${row.targetAddress} 端口 ${row.targetPort}`, `恢复时点 ${proof.targetAt}`,
        `证据来源 ${report.source} · 采样 ${row.sampleId} · ${report.observedAt}`,
        `仓库 ${proof.repoKey} · ${proof.chain.join(' → ')}`,
        `目标前事务 ${proof.beforeMarker} · 已核对`, `目标后事务 ${proof.afterMarker} · 已排除`,
        `RPO 目标 ${row.targetRpoSeconds} s · 实测 ${proof.rpoSeconds} s · 差异 ${report.rpoDifferenceSeconds} s`,
        `RTO 目标 ${row.targetRtoSeconds} s · 实测 ${proof.rtoSeconds} s · 差异 ${report.rtoDifferenceSeconds} s`,
        report.targetsMet ? 'RPO 和 RTO 达标' : '至少一项恢复目标超出', `复核 ${row.submittedBy} · ${row.note}`];
      const content = node('div');
      content.append(...lines.slice(6).map(line => node('p', line)));
      const download = node('button', '下载恢复签收报告');
      download.type = 'button';
      download.addEventListener('click', () => {
        const url = URL.createObjectURL(new Blob([lines.join('\n') + '\n'], {type:'text/plain;charset=utf-8'}));
        const anchor = node('a');
        anchor.href = url;
        anchor.download = `DbSphere-restore-${row.id}.txt`;
        anchor.click();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      });
      content.append(download);
      return content;
    }
    function render(row) {
      const details = node('details');
      details.className = 'settings-details';
      details.dataset.restoreDrillId = row.id;
      details.open = selected === row.id;
      details.append(node('summary', `${row.title} · ${states[row.status]}`),
        node('p', `演练 ID ${row.id} · 申请人 ${row.requester}${row.reviewer ? ` · 审批人 ${row.reviewer}` : ''}`),
        node('p', `恢复目标 ${formatTime(row.targetAt)} · ${row.targetId} · ${row.targetAddress} 端口 ${row.targetPort}`));
      if (row.eventId) details.append(link('打开原风险事件', 'incidents', {event:row.eventId}), ' ');
      if (row.sampleId) details.append(link('恢复原始采样', 'sample', {asset:asset.id, sample:row.sampleId,
        return: row.eventId ? `incidents?${new URLSearchParams({event:row.eventId})}` : `assets?${new URLSearchParams({asset:asset.id, drill:row.id})}`}));
      if (row.report) details.append(report(row));
      if (row.status === 'failed') details.append(node('p', row.note));
      if (role === 'approver' && row.status === 'requested') {
        const actions = node('div');
        actions.className = 'actions';
        for (const [action, label] of [['approve', '批准恢复演练'], ['reject', '驳回恢复演练']]) {
          const button = node('button', label);
          button.type = 'button';
          button.addEventListener('click', () => { void submit(`/${row.id}/${action}`, {}); });
          actions.append(button);
        }
        details.append(actions);
      }
      if (canOperate && row.status === 'approved') {
        details.append(form('登记隔离目标和开工', `/${row.id}/start`, [
          ['targetId', '核对隔离目标标识', 'text', '', {maxLength:100}],
          ['isolated', '已现场核对隔离目录和独立监听', 'checkbox'],
        ], data => ({...data, isolated:data.isolated === 'on'})));
      }
      if (canOperate && row.status === 'started') {
        details.append(form('登记并核对恢复结果', `/${row.id}/complete`, [
          ['sampleId', '恢复采样 ID', 'text', '', {maxLength:36}], ['proofId', '清单演练证据 ID', 'text', '', {maxLength:100}],
          ['note', '人工复核记录', 'text', '', {maxLength:500}],
        ]), form('登记演练失败', `/${row.id}/fail`, [['note', '失败原因', 'text', '', {maxLength:500}]]));
      }
      return details;
    }
    async function load() {
      const {drills} = await api(endpoint);
      body.replaceChildren();
      if (canOperate) body.append(form('申请恢复演练', '', [
        ['eventId', '原风险事件 ID', 'text', '', {maxLength:36}], ['title', '演练标题', 'text', '', {maxLength:120}],
        ['targetId', '隔离目标标识', 'text', '', {maxLength:100}], ['targetAddress', '隔离回环地址', 'text', '127.0.0.1'],
        ['targetPort', '隔离端口', 'number', undefined, {min:1024,max:65535}],
        ['targetAt', '恢复目标时刻（含时区）', 'text', '', {placeholder:'2026-01-02T09:30:00+08:00'}],
        ['targetRpoSeconds', '目标 RPO（秒）', 'number', undefined, {min:0,max:604800}],
        ['targetRtoSeconds', '目标 RTO（秒）', 'number', undefined, {min:1,max:604800}],
      ], data => {
        if (!/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,6})?(?:Z|[+-]\d\d:\d\d)$/.test(data.targetAt)) {
          throw new Error('恢复目标时刻须包含秒和时区');
        }
        for (const key of ['targetPort', 'targetRpoSeconds', 'targetRtoSeconds']) data[key] = Number(data[key]);
        return data;
      }));
      if (!drills.length) body.append(node('p', '该资产暂无恢复演练。'));
      else body.append(...drills.map(render));
      loaded = true;
    }
    const open = () => {
      if (panel.open && !loaded) void load().catch(error => { status.textContent = error.message; });
    };
    panel.addEventListener('toggle', open);
    if (selected) panel.open = true;
    return panel;
  }
  return {create};
})();
