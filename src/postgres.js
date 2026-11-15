import pg from 'pg';

const { Client } = pg;

export async function collectPostgres(asset, password) {
  const client = new Client({
    host: asset.host,
    port: asset.port,
    database: asset.database_name,
    user: asset.username,
    password,
    ssl: asset.ssl_mode === 'require' ? { rejectUnauthorized: true } : false,
    connectionTimeoutMillis: 4000,
    query_timeout: 5000,
    application_name: 'dbsphere-monitor'
  });
  try {
    await client.connect();
    const identity = (await client.query("SELECT current_setting('server_version_num')::int AS version_num, pg_is_in_recovery() AS in_recovery")).rows[0];
    const major = Math.floor(identity.version_num / 10000);
    if (major < 14 || major > 18) throw new Error(`PostgreSQL ${major} is outside the supported 14–18 range`);
    const permission = (await client.query(`SELECT pg_has_role(current_user, 'pg_monitor', 'member') AS monitor_role,
      rolsuper, rolcreatedb, rolcreaterole, rolreplication FROM pg_roles WHERE rolname = current_user`)).rows[0];
    const stat = (await client.query('SELECT numbackends, xact_commit, xact_rollback FROM pg_stat_database WHERE datname = current_database()')).rows[0];
    const size = (await client.query('SELECT pg_database_size(current_database()) AS bytes')).rows[0];
    const metrics = {
      version_num: identity.version_num,
      pg14_eol: major === 14,
      role: identity.in_recovery ? 'standby' : 'primary',
      monitor_role: permission.monitor_role,
      privileged_role: permission.rolsuper || permission.rolcreatedb || permission.rolcreaterole || permission.rolreplication,
      connections: Number(stat?.numbackends ?? 0),
      xact_commit: Number(stat?.xact_commit ?? 0),
      xact_rollback: Number(stat?.xact_rollback ?? 0),
      database_bytes: Number(size.bytes),
      replicas: null,
      wal_receiver: null
    };
    try {
      if (identity.in_recovery) {
        metrics.wal_receiver = (await client.query('SELECT status FROM pg_stat_wal_receiver LIMIT 1')).rows[0]?.status ?? null;
      } else {
        metrics.replicas = Number((await client.query('SELECT count(*)::int AS count FROM pg_stat_replication')).rows[0].count);
      }
    } catch { /* Optional replication view is unavailable to this monitoring account. */ }
    return metrics;
  } finally {
    await client.end().catch(() => {});
  }
}
