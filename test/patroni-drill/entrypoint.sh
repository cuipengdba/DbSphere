#!/usr/bin/env bash
set -euo pipefail

: "${PATRONI_NAME:?missing Patroni member name}"
: "${PATRONI_PG_MAJOR:?missing PostgreSQL major version}"
: "${DBSPHERE_DRILL_PG_PASSWORD:?missing test PostgreSQL password}"
: "${DBSPHERE_DRILL_REPL_PASSWORD:?missing test replication password}"

cat > /tmp/dbsphere-patroni.yml <<EOF
scope: dbsphere-ci-patroni
name: ${PATRONI_NAME}
restapi:
  listen: 0.0.0.0:8008
  connect_address: ${PATRONI_NAME}:8008
etcd3:
  host: etcd:2379
bootstrap:
  dcs:
    ttl: 30
    loop_wait: 10
    retry_timeout: 10
    maximum_lag_on_failover: 1048576
    postgresql:
      use_pg_rewind: true
      pg_hba:
        - local all all trust
        - host replication replicator 0.0.0.0/0 scram-sha-256
        - host all all 0.0.0.0/0 scram-sha-256
  initdb:
    - encoding: UTF8
    - data-checksums
postgresql:
  listen: 0.0.0.0:5432
  connect_address: ${PATRONI_NAME}:5432
  data_dir: /var/lib/postgresql/patroni
  bin_dir: /usr/lib/postgresql/${PATRONI_PG_MAJOR}/bin
  authentication:
    superuser:
      username: postgres
      password: ${DBSPHERE_DRILL_PG_PASSWORD}
    replication:
      username: replicator
      password: ${DBSPHERE_DRILL_REPL_PASSWORD}
  parameters:
    unix_socket_directories: /var/run/postgresql
EOF

exec patroni /tmp/dbsphere-patroni.yml
