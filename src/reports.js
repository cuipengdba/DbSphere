const md = value => String(value ?? '').replaceAll('\\', '\\\\')
  .replaceAll('|', '\\|').replaceAll('\n', ' ').replaceAll('\r', ' ');
const html = value => String(value ?? '').replaceAll('&', '&amp;').replaceAll('<', '&lt;')
  .replaceAll('>', '&gt;').replaceAll('"', '&quot;');
const gib = value => `${(value / 1024 ** 3).toFixed(2)} GiB`;

export function inspectionReportMarkdown(run, asset, forecast = null) {
  const lines = [
    `# DbSphere 巡检报告：${md(asset.name)}`,
    '',
    `- 资产 ID：${md(asset.id)}`,
    `- 模板：${md(run.templateId)} v${run.templateVersion}`,
    `- 巡检 ID：${md(run.id)}`,
    `- 时间：${md(run.startedAt)} 至 ${md(run.completedAt)}`,
    `- 执行状态：${run.status === 'failed' ? '失败' : '完成'}`,
    `- 原始采样 ID：${md(run.sampleId ?? '无')}`,
    `- 同期开放事件 ID：${run.eventIds?.length ? run.eventIds.map(md).join('、') : '无'}`,
    `- 结果：风险 ${run.summary.risk}、通过 ${run.summary.pass}、未知 ${run.summary.unknown}、不适用 ${run.summary.notApplicable ?? 0}、相对上次变化 ${run.summary.changed}`,
    ''
  ];
  if (run.error) lines.push(`执行错误：${md(run.error)}`, '');
  lines.push('## 检查项', '', '| 检查项 | 状态 | 观测值 | 阈值 | 来源/原因 | 上次状态 |',
    '| --- | --- | ---: | ---: | --- | --- |');
  for (const finding of run.findings) lines.push(`| ${md(finding.title)} | ${md(finding.status)} | ${md(finding.observed ?? '未采集')} | ${md(finding.threshold)} | ${md(finding.source)}${finding.reason ? '；' + md(finding.reason) : ''} | ${md(finding.previousStatus ?? '无')} |`);
  if (!run.findings.length) lines.push('| 无 | 未执行 | — | — | — | — |');
  lines.push('', '## 数据库容量趋势', '');
  if (forecast?.status === 'estimate') lines.push(`最近 ${forecast.observedDays.toFixed(0)} 天 ${forecast.points.length} 个每日样本；当前 ${gib(forecast.latestBytes)}，日增长估计 ${gib(forecast.dailyGrowthBytes)}，30 天后 ${gib(forecast.projected30DayBytes)}；置信度 ${forecast.confidence}。最新容量样本 ID：${md(forecast.latestSampleId)}。${md(forecast.method)}。`);
  else lines.push(`无法预测：${md(forecast?.reason ?? '未接入 PostgreSQL 18 长期存储')}`);
  lines.push('', '容量预测是趋势估计，不代表磁盘剩余空间。巡检未知项不等于健康。原始样本可经已认证的 `/api/assets/' + md(asset.id) + '/samples` 查询；事件证据可经 `/api/events/<事件 ID>` 查询。', '');
  return lines.join('\n');
}

export function inspectionReportHtml(run, asset, forecast = null) {
  const title = `DbSphere 巡检报告：${asset.name}`;
  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>${html(title)}</title></head><body><pre>${html(inspectionReportMarkdown(run, asset, forecast))}</pre></body></html>`;
}
