#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo 'Usage: bash scripts/one-click-go.sh' >&2
  exit 1
fi
repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
env_file="$repo_dir/deploy/compose/.env.go"
compose_file="$repo_dir/deploy/compose/compose.go.yaml"
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
  printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_STORE_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_WEB_PORT=8081\nDBSPHERE_COOKIE_SECURE=false\n' \
    "$new_admin_password" "$store_password" "$master_key" > "$env_file"
  chmod 0600 "$env_file"
  unset store_password master_key
fi
if grep -q '^DBSPHERE_DATABASE_URL=' "$env_file"; then
  compose_file="$repo_dir/deploy/compose/compose.go.external.yaml"
fi
compose_args=(-f "$compose_file")
if grep -q '^DBSPHERE_KUBECONFIG_HOST_PATH=' "$env_file"; then
  kubeconfig_path="$(sed -n 's/^DBSPHERE_KUBECONFIG_HOST_PATH=//p' "$env_file" | tail -n 1)"
  if [[ "$kubeconfig_path" != /* || ! -f "$kubeconfig_path" ]]; then
    echo 'DBSPHERE_KUBECONFIG_HOST_PATH must be an existing absolute file path.' >&2
    exit 1
  fi
  compose_args+=(-f "$repo_dir/deploy/compose/compose.go.k8s.yaml")
fi
docker compose --env-file "$env_file" "${compose_args[@]}" config -q
docker compose --env-file "$env_file" "${compose_args[@]}" up -d --build --wait --wait-timeout 180
web_port="$(sed -n 's/^DBSPHERE_WEB_PORT=//p' "$env_file" | tail -n 1)"
echo "DbSphere Go development server is ready at http://127.0.0.1:${web_port:-8081}"
if [[ -n "$new_admin_password" && "${DBSPHERE_NO_PRINT_PASSWORD:-false}" != true ]]; then
  echo "Initial administrator password: $new_admin_password"
else
  echo "Administrator password is in $env_file"
fi
