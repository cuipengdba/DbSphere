#!/usr/bin/env bash
set -euo pipefail

rotate_admin=false
if [[ $# -eq 1 && "$1" == --rotate-admin-password ]]; then
  rotate_admin=true
elif [[ $# -ne 0 && ! ( $# -eq 2 && "$1" == --backup && "$2" == /* ) ]]; then
  echo 'Usage: bash install.sh [--backup /absolute/output/directory | --rotate-admin-password]' >&2
  exit 1
fi
bundle_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
if [[ "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]] ||
   ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  echo 'Linux x86_64, Docker Engine and Compose v2 are required.' >&2
  exit 1
fi
if [[ ! -f "$bundle_dir/SHA256SUMS" || ! -f "$bundle_dir/bundle.env" ||
      ! -f "$bundle_dir/images.tar.gz" ]]; then
  echo 'Go offline bundle is incomplete.' >&2
  exit 1
fi
(cd -- "$bundle_dir" && sha256sum --check --quiet SHA256SUMS) || {
  echo 'Go offline bundle checksum verification failed.' >&2
  exit 1
}
if [[ $# -eq 2 ]]; then
  bash "$bundle_dir/backup.sh" "$2"
  exit 0
fi
source "$bundle_dir/bundle.env"
if [[ ! "${DBSPHERE_GO_OFFLINE_VERSION:-}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?$ ||
  "${DBSPHERE_GO_OFFLINE_APP_IMAGE:-}" != "dbsphere/offline-go-app:$DBSPHERE_GO_OFFLINE_VERSION" ||
  ! "${DBSPHERE_GO_OFFLINE_APP_IMAGE_ID:-}" =~ ^sha256:[0-9a-f]{64}$ ||
  ! "${DBSPHERE_GO_OFFLINE_PG_IMAGE_ID:-}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo 'Go offline bundle manifest is invalid.' >&2
  exit 1
fi
export DBSPHERE_GO_OFFLINE_APP_IMAGE

config_dir="${DBSPHERE_GO_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-go}"
if [[ "$config_dir" != /* ]]; then echo 'DBSPHERE_GO_CONFIG_DIR must be absolute.' >&2; exit 1; fi
mkdir -p -- "$config_dir"
chmod 0700 "$config_dir"
env_file="$config_dir/dbsphere-go.env"
if [[ "$rotate_admin" == true && ! -f "$env_file" ]]; then
  echo 'Administrator password rotation requires an existing Go installation.' >&2
  exit 1
fi
new_admin_password=''
if [[ ! -e "$env_file" ]]; then
  umask 077
  new_admin_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  store_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  master_key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_STORE_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_WEB_PORT=8081\nDBSPHERE_COOKIE_SECURE=false\n' \
    "$new_admin_password" "$store_password" "$master_key" > "$env_file"
  unset store_password master_key
fi
chmod 0600 "$env_file"
base="$bundle_dir/deploy/compose/compose.go.yaml"
if grep -q '^DBSPHERE_DATABASE_URL=' "$env_file"; then
  base="$bundle_dir/deploy/compose/compose.go.external.yaml"
fi
compose_args=(-f "$base")
if grep -q '^DBSPHERE_KUBECONFIG_HOST_PATH=' "$env_file"; then
  kubeconfig_path="$(sed -n 's/^DBSPHERE_KUBECONFIG_HOST_PATH=//p' "$env_file" | tail -n 1)"
  if [[ "$kubeconfig_path" != /* || ! -f "$kubeconfig_path" ]]; then
    echo 'DBSPHERE_KUBECONFIG_HOST_PATH must be an existing absolute file path.' >&2
    exit 1
  fi
  compose_args+=(-f "$bundle_dir/deploy/compose/compose.go.k8s.yaml")
fi
docker image load --input "$bundle_dir/images.tar.gz" >/dev/null
docker image inspect "$DBSPHERE_GO_OFFLINE_APP_IMAGE" >/dev/null
if [[ "$(docker image inspect --format '{{.Id}}' "$DBSPHERE_GO_OFFLINE_APP_IMAGE")" != "$DBSPHERE_GO_OFFLINE_APP_IMAGE_ID" ||
      "$(docker image inspect --format '{{.Id}}' postgres:18-bookworm)" != "$DBSPHERE_GO_OFFLINE_PG_IMAGE_ID" ]]; then
  echo 'Loaded Go or PostgreSQL image does not match this offline bundle.' >&2
  exit 1
fi
docker compose --env-file "$env_file" "${compose_args[@]}" \
  -f "$bundle_dir/deploy/compose/compose.go.offline.yaml" config -q
if [[ "$rotate_admin" == true ]]; then
  docker compose --env-file "$env_file" "${compose_args[@]}" \
    -f "$bundle_dir/deploy/compose/compose.go.offline.yaml" \
    run --rm --no-deps --pull never app rotate-admin-password
  docker compose --env-file "$env_file" "${compose_args[@]}" \
    -f "$bundle_dir/deploy/compose/compose.go.offline.yaml" \
    up -d --no-build --pull never --no-deps --force-recreate app
fi
docker compose --env-file "$env_file" "${compose_args[@]}" \
  -f "$bundle_dir/deploy/compose/compose.go.offline.yaml" \
  up -d --no-build --pull never --wait --wait-timeout 180
web_port="$(sed -n 's/^DBSPHERE_WEB_PORT=//p' "$env_file" | tail -n 1)"
echo "DbSphere Go development server is ready at http://127.0.0.1:${web_port:-8081}"
if [[ -n "$new_admin_password" && "${DBSPHERE_NO_PRINT_PASSWORD:-false}" != true ]]; then
  echo "Initial administrator password: $new_admin_password"
else
  echo "Administrator password is in $env_file"
fi
