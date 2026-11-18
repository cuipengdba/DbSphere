#!/bin/bash
set -euo pipefail

mkdir -p "$PGDATA"
chown -R postgres:postgres /var/lib/postgresql
chmod 700 "$PGDATA"
printf 'primary:5432:*:dbsphere_replicator:%s\n' "$DBSPHERE_DRILL_REPL_PASSWORD" > /var/lib/postgresql/.pgpass
chown postgres:postgres /var/lib/postgresql/.pgpass
chmod 600 /var/lib/postgresql/.pgpass
export PGPASSFILE=/var/lib/postgresql/.pgpass

until pg_isready -h primary -U postgres -d postgres >/dev/null 2>&1; do sleep 1; done
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  gosu postgres pg_basebackup -h primary -p 5432 -U dbsphere_replicator \
    -D "$PGDATA" -R -X stream -C -S dbsphere_drill_slot
fi
exec gosu postgres postgres -D "$PGDATA"
