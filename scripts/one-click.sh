#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
compose_file="$repo_dir/deploy/compose/compose.yaml"
external_compose_file="$repo_dir/deploy/compose/compose.external.yaml"
env_file="$repo_dir/deploy/compose/.env"
migrate_from=''

if [[ $# -gt 0 ]]; then
  if [[ $# -ne 2 || "$1" != '--migrate-from' ]]; then
    echo 'Usage: scripts/one-click.sh [--migrate-from /absolute/path/to/v0.1/data]' >&2
    exit 1
  fi
  migrate_from="$(readlink -f -- "$2")"
  if [[ ! -f "$migrate_from/dbsphere.sqlite" ]]; then
    echo 'Migration source dbsphere.sqlite was not found.' >&2
    exit 1
  fi
fi

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  echo 'Docker Engine and Docker Compose v2 are required.' >&2
  exit 1
fi

new_admin_password=''
if [[ ! -e "$env_file" ]]; then
  umask 077
  new_admin_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  store_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  master_key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_STORE_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_WEB_PORT=8080\nDBSPHERE_COOKIE_SECURE=false\n' \
    "$new_admin_password" "$store_password" "$master_key" > "$env_file"
  chmod 0600 "$env_file"
  unset store_password master_key
  echo "Created $env_file with random credentials. Back it up securely."
fi

if grep -q '^DBSPHERE_DATABASE_URL=' "$env_file"; then
  compose_file="$external_compose_file"
  echo 'Using the independent external PostgreSQL 18 store from .env.'
fi

compose_files=(-f "$compose_file")
kubeconfig_host_path="$(sed -n 's/^DBSPHERE_KUBECONFIG_HOST_PATH=//p' "$env_file" | tail -n 1)"
if [[ -n "$kubeconfig_host_path" ]]; then
  if [[ "$kubeconfig_host_path" != /* || ! -f "$kubeconfig_host_path" ]]; then
    echo 'DBSPHERE_KUBECONFIG_HOST_PATH must be an existing absolute file path.' >&2
    exit 1
  fi
  compose_files+=(-f "$repo_dir/deploy/compose/compose.k8s.yaml")
fi
backup_info_host_path="$(sed -n 's/^DBSPHERE_BACKUP_INFO_HOST_PATH=//p' "$env_file" | tail -n 1)"
if [[ -n "$backup_info_host_path" ]]; then
  if [[ "$backup_info_host_path" != /* || ! -d "$backup_info_host_path" ]]; then
    echo 'DBSPHERE_BACKUP_INFO_HOST_PATH must be an existing absolute directory path.' >&2
    exit 1
  fi
  compose_files+=(-f "$repo_dir/deploy/compose/compose.backup.yaml")
fi

docker compose --env-file "$env_file" "${compose_files[@]}" config -q
if [[ -n "$migrate_from" ]]; then
  docker compose --env-file "$env_file" "${compose_files[@]}" build app
  docker compose --env-file "$env_file" "${compose_files[@]}" stop app >/dev/null 2>&1 || true
  if [[ "$compose_file" != "$external_compose_file" ]]; then
    docker compose --env-file "$env_file" "${compose_files[@]}" up -d --wait --wait-timeout 180 store
  fi
  extra_env=()
  if [[ -n "${DBSPHERE_OLD_MASTER_KEY:-}" ]]; then extra_env=(-e DBSPHERE_OLD_MASTER_KEY); fi
  docker compose --env-file "$env_file" "${compose_files[@]}" run --rm --no-deps \
    -v "$migrate_from:/import:ro" -e DBSPHERE_SQLITE_DATA_DIR=/import \
    "${extra_env[@]}" app node scripts/migrate-sqlite-to-pg18.mjs
  docker compose --env-file "$env_file" "${compose_files[@]}" up -d --wait --wait-timeout 180 app
else
  docker compose --env-file "$env_file" "${compose_files[@]}" up -d --build --wait --wait-timeout 180
fi
web_port="$(sed -n 's/^DBSPHERE_WEB_PORT=//p' "$env_file" | tail -n 1)"
echo "DbSphere is ready at http://127.0.0.1:${web_port:-8080}"
if [[ -n "$new_admin_password" && "${DBSPHERE_NO_PRINT_PASSWORD:-false}" != true ]]; then
  echo "Initial administrator password: $new_admin_password"
else
  echo "Administrator password is in $env_file"
fi
