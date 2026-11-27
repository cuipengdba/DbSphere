'use strict';

window.DbSphereChangeGovernance = (() => {
  const states = {requested:'待审批', approved:'已批准', rejected:'已驳回', started:'执行中',
    succeeded:'外部执行成功', failed:'外部执行失败', rolled_back:'外部已回退'};
  const risks = {ready:'当前检查通过', lock_wait:'观察到锁等待', limited:'采集范围有限', failed:'采集失败'};

  function create(asset, selected, {api, role, formatTime}) {
    const node = (tag, text) => {
      const element = document.createElement(tag);
      element.textContent = text || '';
      return element;
    };
    const panel = node('details');
    panel.className = 'settings-details';
    panel.dataset.changeGovernance = '';
    panel.append(node('summary', '数据库变更治理'));
    const body = node('div'), status = node('p');
    status.setAttribute('role', 'status');
    panel.append(status, body);
    const endpoint = `/api/assets/${asset.id}/change-governance`;
    let loaded = false, busy = false;
    const operator = role === 'admin' || role === 'operator';
    const route = order => 'assets?' + new URLSearchParams({asset:asset.id, change:order.id});
    const button = (label, type = 'button') => {
      const element = node('button', label);
      element.type = type;
      return element;
    };
    async function submit(suffix, data) {
      if (busy) return;
      busy = true;
      panel.setAttribute('aria-busy', 'true');
      status.textContent = '正在提交…';
      try {
        const result = await api(endpoint + suffix, 'POST', data);
        selected = result.id || selected;
        await load();
        status.textContent = '已保存';
      } catch (error) {
        status.textContent = error.message;
      } finally {
        busy = false;
        panel.setAttribute('aria-busy', 'false');
      }
    }
    function field(form, name, label, type = 'text', max = 120) {
      const wrapper = node('label', label), input = node('input');
      input.type = type;
      input.name = name;
      input.required = type !== 'checkbox';
      if (type === 'text') input.maxLength = max;
      wrapper.append(input);
      form.append(wrapper);
      return input;
    }
    function form(label, suffix, fill, values) {
      const element = node('form');
      element.className = 'response-form';
      fill(element);
      element.append(button(label, 'submit'));
      element.addEventListener('submit', event => {
        event.preventDefault();
        void submit(suffix, values(new FormData(element)));
      });
      return element;
    }
    function snapshot(order, point, title) {
      const row = node('p', `${title} · ${formatTime(point.at)} · ${risks[point.risk] || '待核对'}` +
        ` · 连接 ${point.connections ?? '缺采'} · 等待 ${point.lockWaits ?? '缺采'}` +
        ` · 未关闭事件 ${point.openEvents} · SQL 统计 ${point.sqlCoverage === 'available' ? '可用' : '缺采'}`);
      const link = node('a', '原始采样');
      link.href = '#sample?' + new URLSearchParams({asset:asset.id, sample:point.sampleId, return:route(order)});
      row.append(' · ', link);
      return row;
    }
    function renderOrder(order) {
      const details = node('details');
      details.className = 'settings-details';
      details.dataset.changeId = order.id;
      details.open = selected === order.id;
      details.append(node('summary', `${order.title} · ${states[order.status] || order.status}`));
      details.append(node('p', `外部单号 ${order.externalId} · 变更 ID ${order.id}`),
        node('p', `${order.windowTitle} · ${formatTime(order.startsAt)} 至 ${formatTime(order.endsAt)}`),
        node('p', `申请人 ${order.requester}${order.reviewer ? ` · 审批人 ${order.reviewer}` : ''} · ${order.reason}`));
      for (const [key, title] of [['baseline','上线前基线'], ['after','上线后复核'], ['recovery','回退后核对']]) {
        if (order[key]) details.append(snapshot(order, order[key], title));
      }
      for (const [note, actor] of [[order.reviewNote,order.reviewActor],[order.recoveryNote,order.recoveryActor]]) {
        if (note) details.append(node('p', `${actor} · ${note}`));
      }
      for (const receipt of order.receipts || []) {
        details.append(node('p', `回执 ${receipt.receiptId} · ${states[receipt.state] || receipt.state} · ${formatTime(receipt.receivedAt)}`));
      }
      if (order.eventId) {
        const link = node('a', '打开关联事件');
        link.href = '#incidents?' + new URLSearchParams({event:order.eventId});
        details.append(link);
      }
      if (order.events?.length) {
        const summary = node('p', '变更时段同资产事件');
        for (const event of order.events) {
          const link = node('a', event.title);
          link.href = '#incidents?' + new URLSearchParams({event:event.id});
          summary.append(' · ', link);
        }
        details.append(summary);
      }
      const actions = node('div');
      actions.className = 'actions';
      const add = (label, action) => {
        const control = button(label);
        control.addEventListener('click', () => { selected = order.id; void submit(`/${order.id}/${action}`, {}); });
        actions.append(control);
      };
      if (role === 'approver' && order.status === 'requested') {
        add('批准变更', 'approve');
        add('驳回变更', 'reject');
      }
      if (operator && ['requested','approved'].includes(order.status)) add('执行只读预检', 'baseline');
      details.append(actions);
      if (operator && ['succeeded','failed','rolled_back'].includes(order.status)) {
        const recovery = order.status === 'rolled_back';
        const review = form(recovery ? '保存回退核对' : '保存上线复核', `/${order.id}/${recovery ? 'recovery' : 'review'}`, fields => {
          field(fields, 'note', '人工核对记录', 'text', 500);
          const event = field(fields, 'eventId', '关联事件 ID（可选）', 'text', 36);
          event.required = false;
          event.value = order.eventId || '';
          event.readOnly = !!order.eventId;
        }, data => { selected = order.id; return Object.fromEntries(data); });
        details.append(review);
      }
      return details;
    }
    async function load() {
      const value = await api(endpoint);
      const result = document.createDocumentFragment();
      if (role === 'admin') {
        result.append(form('创建变更窗口', '/windows', fields => {
          field(fields, 'title', '窗口名称');
          field(fields, 'startsAt', '开始时间', 'datetime-local');
          field(fields, 'endsAt', '结束时间', 'datetime-local');
          field(fields, 'organization', '适用当前资产所在组织', 'checkbox');
        }, data => ({title:data.get('title'), startsAt:new Date(data.get('startsAt')).toISOString(),
          endsAt:new Date(data.get('endsAt')).toISOString(), organization:data.has('organization')})));
      }
      if (operator && value.windows.length) {
        result.append(form('提交变更申请', '/requests', fields => {
          const label = node('label', '变更窗口'), choices = node('select');
          choices.name = 'windowId';
          for (const window of value.windows) {
            const option = node('option', `${window.title} · ${formatTime(window.startsAt)}`);
            option.value = window.id;
            choices.append(option);
          }
          label.append(choices);
          fields.append(label);
          field(fields, 'title', '变更标题');
          field(fields, 'externalId', '外部单号');
          field(fields, 'reason', '变更目的', 'text', 500);
        }, data => Object.fromEntries(data)));
      } else if (operator) {
        result.append(node('p', '暂无可用变更窗口。'));
      }
      if (!value.orders.length) result.append(node('p', '暂无变更申请。'));
      for (const order of value.orders) result.append(renderOrder(order));
      body.replaceChildren(result);
      loaded = true;
    }
    panel.addEventListener('toggle', () => {
      if (panel.open && !loaded) void load().catch(error => { status.textContent = error.message; });
    });
    const refresh = button('刷新变更记录');
    refresh.className = 'ghost small-button';
    refresh.addEventListener('click', () => { if (!busy) void load().catch(error => { status.textContent = error.message; }); });
    panel.insertBefore(refresh, status);
    panel.open = !!selected;
    return panel;
  }
  return {create};
})();
