#!/usr/bin/env bash
set -euo pipefail

bundle_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
if [[ "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]]; then
  echo 'This Node.js offline bundle supports Linux x86_64 only.' >&2
  exit 1
fi
if ! command -v node >/dev/null 2>&1; then
  echo 'Node.js >=24.13 must already be installed on this host.' >&2
  exit 1
fi
node_version="$(node -p 'process.versions.node')"
IFS=. read -r major minor patch <<< "$node_version"
if (( major < 24 || (major == 24 && minor < 13) )); then
  echo 'Node.js >=24.13 is required.' >&2
  exit 1
fi
(cd -- "$bundle_dir" && sha256sum --check --quiet SHA256SUMS) || {
  echo 'Node.js offline bundle checksum verification failed.' >&2
  exit 1
}
config_dir="${DBSPHERE_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-native}"
if [[ "$config_dir" != /* ]]; then
  echo 'DBSPHERE_CONFIG_DIR must be an absolute path.' >&2
  exit 1
fi
mkdir -p -- "$config_dir"
chmod 0700 "$config_dir"
env_file="$config_dir/dbsphere.env"
new_admin_password=''
if [[ ! -e "$env_file" ]]; then
  db_url="${DBSPHERE_DATABASE_URL:-}"
  if [[ "$db_url" != postgres://* && "$db_url" != postgresql://* || "$db_url" == *$'\n'* ]]; then
    echo 'Set DBSPHERE_DATABASE_URL to an independent PostgreSQL 18 database before first start.' >&2
    exit 1
  fi
  umask 077
  new_admin_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  master_key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  printf 'DBSPHERE_DATABASE_URL=%s\nDBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\n' \
    "$db_url" "$new_admin_password" "$master_key" > "$env_file"
  unset master_key
fi
chmod 0600 "$env_file"
export DBSPHERE_DATABASE_URL="$(sed -n 's/^DBSPHERE_DATABASE_URL=//p' "$env_file" | tail -n 1)"
export DBSPHERE_ADMIN_PASSWORD="$(sed -n 's/^DBSPHERE_ADMIN_PASSWORD=//p' "$env_file" | tail -n 1)"
export DBSPHERE_MASTER_KEY="$(sed -n 's/^DBSPHERE_MASTER_KEY=//p' "$env_file" | tail -n 1)"
export DBSPHERE_DATA_DIR="$config_dir/data"
export DBSPHERE_HOST="${DBSPHERE_HOST:-127.0.0.1}"
export DBSPHERE_PORT="${DBSPHERE_PORT:-8080}"
mkdir -p -- "$DBSPHERE_DATA_DIR"
chmod 0700 "$DBSPHERE_DATA_DIR"
if [[ -n "$new_admin_password" && "${DBSPHERE_NO_PRINT_PASSWORD:-false}" != true ]]; then
  echo "Initial administrator password: $new_admin_password"
fi
echo "DbSphere native process starting at http://$DBSPHERE_HOST:$DBSPHERE_PORT"
exec node "$bundle_dir/app/src/server.js"
