export function forecastDatabaseGrowth(points) {
  const ordered = points.filter(item => item.databaseBytes != null && Number.isFinite(Number(item.databaseBytes)) &&
    Number(item.databaseBytes) >= 0 && Number.isFinite(new Date(item.day).getTime()))
    .map(item => ({ day: new Date(item.day).toISOString(), databaseBytes: Number(item.databaseBytes),
      sampleId: item.sampleId ?? null })).sort((a, b) => a.day.localeCompare(b.day));
  const first = ordered[0];
  const latest = ordered.at(-1);
  const spanDays = first && latest ? (new Date(latest.day) - new Date(first.day)) / 86400000 : 0;
  if (ordered.length < 3 || spanDays < 2) return { status: 'insufficient',
    reason: '至少需要跨 3 个自然日的有效数据库容量样本', points: ordered };
  const slopes = [];
  for (let i = 1; i < ordered.length; i++) {
    const days = (new Date(ordered[i].day) - new Date(ordered[i - 1].day)) / 86400000;
    if (days > 0) slopes.push(Math.max(0, (ordered[i].databaseBytes - ordered[i - 1].databaseBytes) / days));
  }
  slopes.sort((a, b) => a - b);
  const middle = Math.floor(slopes.length / 2);
  const dailyGrowthBytes = slopes.length % 2
    ? slopes[middle] : (slopes[middle - 1] + slopes[middle]) / 2;
  return { status: 'estimate', points: ordered, observedDays: spanDays,
    dailyGrowthBytes: Math.round(dailyGrowthBytes),
    projected30DayBytes: Math.round(latest.databaseBytes + dailyGrowthBytes * 30),
    latestBytes: latest.databaseBytes, latestSampleId: latest.sampleId,
    confidence: ordered.length >= 7 && spanDays >= 7 ? 'medium' : 'low',
    method: '最近 30 天每日末次样本的相邻增长速率中位数；下降按零增长处理' };
}
