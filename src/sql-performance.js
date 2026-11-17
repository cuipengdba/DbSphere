const number = value => value == null ? null : Number(value);
const identifier = value => `"${String(value).replaceAll('"', '""')}"`;

export async function collectSqlPerformance(client) {
  let schema;
  try {
    const extension = await client.query(`SELECT n.nspname AS schema_name
      FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace
      WHERE e.extname='pg_stat_statements'`);
    schema = extension.rows[0]?.schema_name;
  } catch (error) {
    return { status: 'unavailable', reason: `extension_catalog_${error.code || 'error'}` };
  }
  if (!schema) return { status: 'not_configured', reason: 'pg_stat_statements extension is not installed in this database' };
  try {
    const view = `${identifier(schema)}.pg_stat_statements`;
    const info = `${identifier(schema)}.pg_stat_statements_info`;
    const columns = (await client.query(`SELECT a.attname FROM pg_attribute a
      JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
      WHERE n.nspname=$1 AND c.relname='pg_stat_statements'
      AND a.attnum>0 AND NOT a.attisdropped`, [schema])).rows.map(row => row.attname);
    const required = ['dbid', 'userid', 'queryid', 'toplevel', 'calls', 'total_exec_time',
      'rows', 'shared_blks_hit', 'shared_blks_read', 'shared_blks_written',
      'temp_blks_read', 'temp_blks_written', 'wal_bytes'];
    if (required.some(name => !columns.includes(name))) {
      return { status: 'unavailable', reason: 'pg_stat_statements view is missing required columns' };
    }
    const rows = (await client.query(`SELECT dbid::text,userid::text,queryid::text,toplevel,
      calls::text,total_exec_time,rows::text,shared_blks_hit::text,
      shared_blks_read::text,shared_blks_written::text,
      temp_blks_read::text,temp_blks_written::text,wal_bytes::text
      FROM ${view} WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
      ORDER BY total_exec_time DESC LIMIT 50`)).rows;
    let resetAt = null;
    try { resetAt = (await client.query(`SELECT stats_reset FROM ${info}`)).rows[0]?.stats_reset ?? null; }
    catch { /* The statement view is still useful without reset metadata. */ }
    const statements = rows.filter(row => row.queryid != null).map(row => ({
      fingerprint: `${row.dbid}:${row.userid}:${row.queryid}:${row.toplevel ? 1 : 0}`,
      queryId: row.queryid, databaseOid: row.dbid, userOid: row.userid,
      topLevel: row.toplevel, calls: number(row.calls), totalExecMs: number(row.total_exec_time),
      rows: number(row.rows), sharedBlocksHit: number(row.shared_blks_hit),
      sharedBlocksRead: number(row.shared_blks_read),
      sharedBlocksWritten: number(row.shared_blks_written),
      tempBlocksRead: number(row.temp_blks_read),
      tempBlocksWritten: number(row.temp_blks_written),
      walBytes: number(row.wal_bytes)
    }));
    return { status: 'ok', source: 'pg_stat_statements', resetAt,
      limit: 50, truncated: rows.length === 50, hiddenQueryCount: rows.length - statements.length,
      statements };
  } catch (error) {
    return { status: 'unavailable', reason: `pg_stat_statements_${error.code || 'error'}` };
  }
}
