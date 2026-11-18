#!/usr/bin/env bash
set -euo pipefail
shopt -s nullglob

if [[ $# -ne 1 || "$1" != /* || "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]]; then
  echo 'Usage on Linux x86_64: bash scripts/go-external-store-smoke.sh /absolute/test/directory' >&2
  exit 1
fi
for command in docker curl jq; do
  command -v "$command" >/dev/null 2>&1 || { echo "Missing $command" >&2; exit 1; }
done
docker compose version >/dev/null

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(realpath -m -- "$1")"
mkdir -p -- "$test_root"
export DBSPHERE_GO_CONFIG_DIR="$test_root/config"
export DBSPHERE_NO_PRINT_PASSWORD=true
mkdir -p -- "$DBSPHERE_GO_CONFIG_DIR"
chmod 0700 "$DBSPHERE_GO_CONFIG_DIR"
env_file="$DBSPHERE_GO_CONFIG_DIR/dbsphere-go.env"
cookie_file="$test_root/cookies"
base_url='http://127.0.0.1:18081'
external_container="dbsphere-go-external-pg18-$$"
trap 'docker rm -f "$external_container" >/dev/null 2>&1 || true' EXIT

bash "$repo_dir/scripts/build-go-offline-bundle.sh" "$test_root/bundle"
archives=("$test_root"/bundle/DbSphere-go-offline-*.run)
test "${#archives[@]}" -eq 1
archive="${archives[0]}"
pg_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
admin_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
master_key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
umask 077
printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_DATABASE_URL=postgres://postgres:%s@host.docker.internal:15432/postgres\nDBSPHERE_WEB_PORT=18081\nDBSPHERE_COOKIE_SECURE=false\n' \
  "$admin_password" "$master_key" "$pg_password" > "$env_file"
chmod 0600 "$env_file"
config_hash="$(sha256sum "$env_file" | cut -d ' ' -f 1)"

docker run -d --pull never --name "$external_container" -e "POSTGRES_PASSWORD=$pg_password" \
  -p 15432:5432 postgres:18-bookworm >/dev/null
for attempt in {1..40}; do
  if docker exec "$external_container" pg_isready -h 127.0.0.1 -U postgres -d postgres >/dev/null 2>&1; then break; fi
  sleep 2
done
docker exec "$external_container" pg_isready -h 127.0.0.1 -U postgres -d postgres >/dev/null
version="$(docker exec "$external_container" psql -U postgres -d postgres -Atqc 'SHOW server_version_num')"
test "$version" -ge 180000 && test "$version" -lt 190000

wait_for_status() {
  local expected="$1" actual
  for attempt in {1..40}; do
    actual="$(curl --max-time 3 --silent --output /dev/null --write-out '%{http_code}' "$base_url/readyz" || true)"
    if [[ "$actual" == "$expected" ]]; then return 0; fi
    sleep 2
  done
  echo "Expected readiness HTTP $expected; last response was $actual" >&2
  return 1
}
login() {
  local response
  response="$(jq -nc --arg password "$admin_password" '{password:$password}' | \
    curl --fail --silent --show-error -c "$cookie_file" -H 'Content-Type: application/json' \
      --data-binary @- "$base_url/api/login")"
  csrf="$(jq -er .csrf <<< "$response")"
}
check_state() {
  wait_for_status 200
  login
  curl --fail --silent --show-error -b "$cookie_file" "$base_url/api/assets/$asset_id" |
    jq -e --arg id "$asset_id" '.asset.id==$id' >/dev/null
  curl --fail --silent --show-error -b "$cookie_file" "$base_url/api/events/$event_id" |
    jq -e --arg id "$event_id" '.event.id==$id and (.event.evidence|length)>0' >/dev/null
  curl --fail --silent --show-error -b "$cookie_file" \
    "$base_url/api/assets/$asset_id/samples/$sample_id" |
    jq -e --arg id "$sample_id" '.sample.id==$id' >/dev/null
  test "$(sha256sum "$env_file" | cut -d ' ' -f 1)" = "$config_hash"
}

bash "$archive"
test -z "$(docker ps -q --filter label=com.docker.compose.project=dbsphere-go-dev \
  --filter label=com.docker.compose.service=store)"
wait_for_status 200
login
asset_id="$(jq -nc '{name:"External PG18 smoke",host:"127.0.0.1",port:1,
  database:"postgres",username:"monitor",password:"fixture-secret",sslMode:"disable",runtime:"linux"}' |
  curl --fail --silent --show-error -b "$cookie_file" -H "X-CSRF-Token: $csrf" \
    -H 'Content-Type: application/json' --data-binary @- "$base_url/api/assets" | jq -er .asset.id)"
sample_id="$(curl --fail --silent --show-error -b "$cookie_file" -H "X-CSRF-Token: $csrf" \
  -X POST "$base_url/api/assets/$asset_id/check" |
  jq -er '.result | select(.status=="error") | .sampleId')"
event_id="$(curl --fail --silent --show-error -b "$cookie_file" "$base_url/api/events" |
  jq -er --arg id "$asset_id" 'first(.events[] | select(.assetId==$id) | .id)')"
check_state

docker stop "$external_container" >/dev/null
wait_for_status 503
docker start "$external_container" >/dev/null
check_state
bash "$archive"
check_state
echo 'Go offline external PG18 install, live storage outage, recovery and repeat install preserved one asset, sample and event ID.'
