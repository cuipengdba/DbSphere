#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo 'Usage: bash install.sh' >&2
  exit 1
fi
bundle_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
if [[ "$(uname -s)" != 'Linux' || "$(uname -m)" != 'x86_64' ]]; then
  echo 'This offline bundle supports Linux x86_64 only.' >&2
  exit 1
fi
if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  echo 'Docker Engine and Docker Compose v2 must be installed before running this offline bundle.' >&2
  exit 1
fi
if [[ ! -f "$bundle_dir/SHA256SUMS" || ! -f "$bundle_dir/bundle.env" ||
  ! -f "$bundle_dir/images.tar.gz" ]]; then
  echo 'Offline bundle is incomplete.' >&2
  exit 1
fi
(cd -- "$bundle_dir" && sha256sum --check --quiet SHA256SUMS) || {
  echo 'Offline bundle checksum verification failed.' >&2
  exit 1
}
source "$bundle_dir/bundle.env"
if [[ ! "${DBSPHERE_OFFLINE_VERSION:-}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?$ ||
  "${DBSPHERE_OFFLINE_APP_IMAGE:-}" != "dbsphere/offline-app:$DBSPHERE_OFFLINE_VERSION" ]]; then
  echo 'Offline bundle manifest is invalid.' >&2
  exit 1
fi
export DBSPHERE_OFFLINE_APP_IMAGE

config_dir="${DBSPHERE_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere}"
if [[ "$config_dir" != /* ]]; then
  echo 'DBSPHERE_CONFIG_DIR must be an absolute path.' >&2
  exit 1
fi
mkdir -p -- "$config_dir"
chmod 0700 "$config_dir"
env_file="$config_dir/dbsphere.env"
new_admin_password=''
if [[ ! -e "$env_file" ]]; then
  umask 077
  new_admin_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  store_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  master_key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_STORE_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_WEB_PORT=8080\nDBSPHERE_COOKIE_SECURE=false\n' \
    "$new_admin_password" "$store_password" "$master_key" > "$env_file"
  unset store_password master_key
fi
chmod 0600 "$env_file"

base="$bundle_dir/deploy/compose/compose.yaml"
if grep -q '^DBSPHERE_DATABASE_URL=' "$env_file"; then
  base="$bundle_dir/deploy/compose/compose.external.yaml"
  echo 'Using the external PostgreSQL 18 store configured in dbsphere.env.'
fi
files=(-f "$base" -f "$bundle_dir/deploy/compose/compose.offline.yaml")
validate_mount() {
  local key="$1" expected="$2" overlay="$3" path
  path="$(sed -n "s/^${key}=//p" "$env_file" | tail -n 1)"
  if [[ -z "$path" ]]; then return; fi
  if [[ "$path" != /* || ( "$expected" == file && ! -f "$path" ) ||
    ( "$expected" == directory && ! -d "$path" ) ]]; then
    echo "$key must be an existing absolute $expected path." >&2
    exit 1
  fi
  files+=(-f "$bundle_dir/deploy/compose/$overlay")
}
validate_mount DBSPHERE_KUBECONFIG_HOST_PATH file compose.k8s.yaml
validate_mount DBSPHERE_BACKUP_INFO_HOST_PATH directory compose.backup.yaml
validate_mount DBSPHERE_STORE_BACKUP_HOST_PATH file compose.store-backup.yaml

docker image load --input "$bundle_dir/images.tar.gz" >/dev/null
docker image inspect "$DBSPHERE_OFFLINE_APP_IMAGE" >/dev/null
if [[ "$base" == "$bundle_dir/deploy/compose/compose.yaml" ]]; then
  docker image inspect postgres:18-bookworm >/dev/null
fi
docker compose --env-file "$env_file" "${files[@]}" config -q
docker compose --env-file "$env_file" "${files[@]}" up -d --no-build --pull never --wait --wait-timeout 180
web_port="$(sed -n 's/^DBSPHERE_WEB_PORT=//p' "$env_file" | tail -n 1)"
echo "DbSphere $DBSPHERE_OFFLINE_VERSION is ready at http://127.0.0.1:${web_port:-8080}"
if [[ -n "$new_admin_password" && "${DBSPHERE_NO_PRINT_PASSWORD:-false}" != true ]]; then
  echo "Initial administrator password: $new_admin_password"
else
  echo "Administrator password is in $env_file"
fi
