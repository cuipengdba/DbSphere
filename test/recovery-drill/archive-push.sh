#!/usr/bin/env bash
set -euo pipefail
if [[ -f /lab/archive-paused ]]; then
  exit 1
fi
exec pgbackrest --config=/etc/dbsphere-pgbackrest.conf --stanza=ledger archive-push "$1"
