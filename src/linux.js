const excludedFilesystems = new Set([
  'tmpfs', 'devtmpfs', 'overlay', 'squashfs', 'proc', 'sysfs', 'cgroup', 'cgroup2',
  'nsfs', 'tracefs', 'autofs', 'securityfs', 'configfs', 'debugfs', 'fusectl',
  'mqueue', 'ramfs', 'devpts', 'efivarfs'
]);

function labels(text = '') {
  const result = {};
  for (const match of text.matchAll(/([a-zA-Z_][\w]*)="((?:\\.|[^"\\])*)"/g)) {
    result[match[1]] = match[2].replaceAll('\\"', '"').replaceAll('\\\\', '\\');
  }
  return result;
}

export function parseNodeExporter(text) {
  const scalar = new Map();
  const filesystems = new Map();
  let cpuTotalSeconds = 0;
  let cpuIdleSeconds = 0;
  let diskReadBytesTotal = 0;
  let diskWrittenBytesTotal = 0;
  let networkReceiveBytesTotal = 0;
  let networkTransmitBytesTotal = 0;
  for (const line of text.split('\n')) {
    if (!line || line.startsWith('#')) continue;
    const match = line.match(/^([a-zA-Z_:][\w:]*)(\{[^}]*\})?\s+([^\s]+)/);
    if (!match) continue;
    const [, name, rawLabels, rawValue] = match;
    const value = Number(rawValue);
    if (!Number.isFinite(value)) continue;
    const tag = labels(rawLabels);
    if (name === 'node_cpu_seconds_total') {
      cpuTotalSeconds += value;
      if (tag.mode === 'idle') cpuIdleSeconds += value;
    } else if (name === 'node_disk_read_bytes_total' && !/^(loop|ram|zram)/.test(tag.device || '')) {
      diskReadBytesTotal += value;
    } else if (name === 'node_disk_written_bytes_total' && !/^(loop|ram|zram)/.test(tag.device || '')) {
      diskWrittenBytesTotal += value;
    } else if (name === 'node_network_receive_bytes_total' && !/^(lo|veth|docker)/.test(tag.device || '')) {
      networkReceiveBytesTotal += value;
    } else if (name === 'node_network_transmit_bytes_total' && !/^(lo|veth|docker)/.test(tag.device || '')) {
      networkTransmitBytesTotal += value;
    } else if (name === 'node_filesystem_size_bytes' || name === 'node_filesystem_avail_bytes') {
      if (!tag.mountpoint || excludedFilesystems.has(tag.fstype) ||
          ['/etc/hosts', '/etc/hostname', '/etc/resolv.conf'].includes(tag.mountpoint)) continue;
      const entry = filesystems.get(tag.mountpoint) ?? { mountpoint: tag.mountpoint, device: tag.device ?? null };
      if (name === 'node_filesystem_size_bytes') entry.sizeBytes = value;
      else entry.availableBytes = value;
      filesystems.set(tag.mountpoint, entry);
    } else if (!rawLabels) scalar.set(name, value);
  }
  const totalBytes = scalar.get('node_memory_MemTotal_bytes');
  const availableBytes = scalar.get('node_memory_MemAvailable_bytes');
  return {
    cpu: { totalSeconds: cpuTotalSeconds || null, idleSeconds: cpuIdleSeconds || null,
      load1: scalar.get('node_load1') ?? null },
    memory: { totalBytes: totalBytes ?? null, availableBytes: availableBytes ?? null,
      usedPercent: totalBytes && availableBytes != null ? Number(((1 - availableBytes / totalBytes) * 100).toFixed(1)) : null },
    filesystems: [...filesystems.values()].filter(f => f.sizeBytes > 0 && f.availableBytes != null)
      .map(f => ({ ...f, usedPercent: Number(((1 - f.availableBytes / f.sizeBytes) * 100).toFixed(1)) }))
      .sort((a, b) => b.sizeBytes - a.sizeBytes).slice(0, 10),
    disk: { readBytesTotal: diskReadBytesTotal, writtenBytesTotal: diskWrittenBytesTotal },
    network: { receiveBytesTotal: networkReceiveBytesTotal, transmitBytesTotal: networkTransmitBytesTotal }
  };
}

export async function collectLinux(asset) {
  if (!asset.linux_host || !asset.linux_exporter_port) {
    return { status: 'not_configured', reason: 'Set Linux host and node_exporter port for enhanced host metrics' };
  }
  const host = asset.linux_host.includes(':') ? `[${asset.linux_host}]` : asset.linux_host;
  const response = await fetch(`http://${host}:${asset.linux_exporter_port}/metrics`, {
    signal: AbortSignal.timeout(4000), redirect: 'error'
  });
  if (!response.ok) throw new Error(`node_exporter HTTP ${response.status}`);
  let length = 0;
  const chunks = [];
  for await (const chunk of response.body) {
    length += chunk.length;
    if (length > 5 * 1024 * 1024) throw new Error('node_exporter response exceeds 5 MiB');
    chunks.push(chunk);
  }
  const metrics = parseNodeExporter(Buffer.concat(chunks).toString('utf8'));
  if (!metrics.memory.totalBytes && !metrics.cpu.totalSeconds) throw new Error('node_exporter host metrics not found');
  return { status: 'ok', source: 'node_exporter', observedAt: new Date().toISOString(), ...metrics };
}

export function deriveLinuxRates(current, previous) {
  if (current?.status !== 'ok' || previous?.status !== 'ok') return current;
  const seconds = (new Date(current.observedAt).getTime() - new Date(previous.observedAt).getTime()) / 1000;
  if (!Number.isFinite(seconds) || seconds <= 0) return current;
  const delta = (a, b) => Number.isFinite(a) && Number.isFinite(b) && a >= b ? a - b : null;
  const rate = (a, b) => { const difference = delta(a, b); return difference == null ? null : Math.round(difference / seconds); };
  const cpuTotal = delta(current.cpu.totalSeconds, previous.cpu.totalSeconds);
  const cpuIdle = delta(current.cpu.idleSeconds, previous.cpu.idleSeconds);
  return {
    ...current,
    cpu: { ...current.cpu, usedPercent: cpuTotal > 0 && cpuIdle != null
      ? Number(((1 - cpuIdle / cpuTotal) * 100).toFixed(1)) : null },
    disk: { ...current.disk,
      readBytesPerSecond: rate(current.disk.readBytesTotal, previous.disk.readBytesTotal),
      writtenBytesPerSecond: rate(current.disk.writtenBytesTotal, previous.disk.writtenBytesTotal) },
    network: { ...current.network,
      receiveBytesPerSecond: rate(current.network.receiveBytesTotal, previous.network.receiveBytesTotal),
      transmitBytesPerSecond: rate(current.network.transmitBytesTotal, previous.network.transmitBytesTotal) }
  };
}
