#!/bin/sh
set -eu

psql -v ON_ERROR_STOP=1 -v repl_password="$DBSPHERE_DRILL_REPL_PASSWORD" \
  -v monitor_password="$DBSPHERE_DRILL_MONITOR_PASSWORD" \
  --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE ROLE dbsphere_replicator WITH LOGIN REPLICATION PASSWORD :'repl_password';
CREATE ROLE dbsphere_monitor WITH LOGIN PASSWORD :'monitor_password';
GRANT pg_monitor TO dbsphere_monitor;
SQL

printf '%s\n' 'host replication dbsphere_replicator 0.0.0.0/0 scram-sha-256' >> "$PGDATA/pg_hba.conf"
