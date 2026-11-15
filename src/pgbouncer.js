import pg from 'pg';

const { Client } = pg;
const number = value => Number.isFinite(Number(value)) ? Number(value) : 0;

export async function collectPgBouncer(asset, connectionString) {
  if (!connectionString) return { status: 'not_configured', reason: 'PgBouncer admin URL is not configured' };
  const url = new URL(connectionString);
  if (!['postgres:', 'postgresql:'].includes(url.protocol) || url.pathname !== '/pgbouncer') {
    throw new Error('PgBouncer URL must target the pgbouncer admin database');
  }
  const client = new Client({ connectionString, connectionTimeoutMillis: 4000, query_timeout: 5000,
    application_name: 'dbsphere-pgbouncer-monitor' });
  try {
    await client.connect();
    // PgBouncer's admin console only accepts the simple query protocol.
    const pools = (await client.query('SHOW POOLS')).rows.filter(row => row.database === asset.database_name).slice(0, 100);
    const stats = (await client.query('SHOW STATS')).rows.filter(row => row.database === asset.database_name).slice(0, 100);
    if (!pools.length && !stats.length) return { status: 'unmapped', reason: `No PgBouncer pool maps to ${asset.database_name}` };
    return { status: 'ok', source: 'PgBouncer SHOW POOLS/STATS', observedAt: new Date().toISOString(),
      database: asset.database_name,
      pools: pools.map(row => ({ user: row.user, mode: row.pool_mode ?? null,
        clientActive: number(row.cl_active), clientWaiting: number(row.cl_waiting),
        serverActive: number(row.sv_active), serverIdle: number(row.sv_idle),
        maxWaitSeconds: number(row.maxwait) + number(row.maxwait_us) / 1e6 })),
      stats: stats.map(row => ({ transactionsTotal: number(row.total_xact_count),
        queriesTotal: number(row.total_query_count), avgQueryMicroseconds: number(row.avg_query_time) })) };
  } finally { await client.end().catch(() => {}); }
}
