#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -lt 1 || $# -gt 2 || "$1" != /* || "${2:-bundled}" != bundled && "${2:-bundled}" != external ||
      "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]]; then
  echo 'Usage on Linux x86_64: bash scripts/go-offline-upgrade-rollback-smoke.sh /absolute/test/directory [external]' >&2
  exit 1
fi
mode="${2:-bundled}"
for command in git docker curl jq; do command -v "$command" >/dev/null 2>&1 || { echo "Missing $command" >&2; exit 1; }; done
docker compose version >/dev/null

test_root="$(realpath -m -- "$1")"
old_source="$test_root/previous-go"
old_output="$test_root/previous-bundle"
new_output="$test_root/current-bundle"
export DBSPHERE_GO_CONFIG_DIR="$test_root/config"
export DBSPHERE_NO_PRINT_PASSWORD=true
mkdir -p -- "$old_source" "$old_output" "$new_output" "$DBSPHERE_GO_CONFIG_DIR"

# An existing Go development baseline; give it a distinct image tag while
# leaving the application code and PG18 schema from that commit untouched.
git -C "$repo_dir" archive 585608d | tar -x -C "$old_source"
sed -i 's/"version": "0.6.0-dev"/"version": "0.6.0-dev.prev"/' "$old_source/package.json"
grep -q '"version": "0.6.0-dev.prev"' "$old_source/package.json"
bash "$old_source/scripts/build-go-offline-bundle.sh" "$old_output"
bash "$repo_dir/scripts/build-go-offline-bundle.sh" "$new_output"
old_archive="$old_output/DbSphere-go-offline-0.6.0-dev.prev-linux-amd64.run"
new_version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$repo_dir/package.json" | head -n 1)"
new_archive="$new_output/DbSphere-go-offline-$new_version-linux-amd64.run"
test -s "$old_archive" && test -s "$new_archive"
if [[ "$mode" == external ]]; then
  docker image rm 'dbsphere/offline-go-app:0.6.0-dev.prev' "dbsphere/offline-go-app:$new_version"
else
  docker image rm 'dbsphere/offline-go-app:0.6.0-dev.prev' "dbsphere/offline-go-app:$new_version" postgres:18-bookworm
fi
if docker image inspect 'dbsphere/offline-go-app:0.6.0-dev.prev' >/dev/null 2>&1; then exit 1; fi

if [[ "$mode" == external ]]; then
  external_container="dbsphere-go-external-upgrade-pg18-$$"
  trap 'docker rm -f "$external_container" >/dev/null 2>&1 || true' EXIT
  pg_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  admin_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  master_key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  umask 077
  printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_DATABASE_URL=postgres://postgres:%s@host.docker.internal:15433/postgres\nDBSPHERE_WEB_PORT=8081\nDBSPHERE_COOKIE_SECURE=false\n' \
    "$admin_password" "$master_key" "$pg_password" > "$DBSPHERE_GO_CONFIG_DIR/dbsphere-go.env"
  chmod 0600 "$DBSPHERE_GO_CONFIG_DIR/dbsphere-go.env"
  docker run -d --pull never --name "$external_container" -e "POSTGRES_PASSWORD=$pg_password" \
    -p 15433:5432 postgres:18-bookworm >/dev/null
  for attempt in {1..40}; do
    if docker exec "$external_container" pg_isready -h 127.0.0.1 -U postgres -d postgres >/dev/null 2>&1; then break; fi
    sleep 2
  done
  docker exec "$external_container" pg_isready -h 127.0.0.1 -U postgres -d postgres >/dev/null
  pg_version="$(docker exec "$external_container" psql -U postgres -d postgres -Atqc 'SHOW server_version_num')"
  test "$pg_version" -ge 180000 && test "$pg_version" -lt 190000
fi

base_url='http://127.0.0.1:8081'
cookie_file="$test_root/cookies"
login() {
  local password response
  password="$(sed -n 's/^DBSPHERE_ADMIN_PASSWORD=//p' "$DBSPHERE_GO_CONFIG_DIR/dbsphere-go.env" | head -n 1)"
  test -n "$password"
  response="$(jq -nc --arg password "$password" '{password:$password}' | \
    curl --fail --silent --show-error -c "$cookie_file" -H 'Content-Type: application/json' \
      --data-binary @- "$base_url/api/login")"
  csrf="$(jq -er .csrf <<<"$response")"
}
check_state() {
  local expected_image="$1" image asset event sample app_container
  curl --fail --silent --show-error "$base_url/readyz" | jq -e '.storage=="postgresql18"' >/dev/null
  app_container="$(docker ps -q \
    --filter label=com.docker.compose.project=dbsphere-go-dev \
    --filter label=com.docker.compose.service=app)"
  image="$(docker inspect -f '{{.Config.Image}}' "$app_container")"
  test "$image" = "$expected_image"
  if [[ "$mode" == external ]]; then
    test -z "$(docker ps -q --filter label=com.docker.compose.project=dbsphere-go-dev \
      --filter label=com.docker.compose.service=store)"
    docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$app_container" |
      grep -Fq 'DBSPHERE_DATABASE_URL=postgres://postgres:'
  fi
  login
  asset="$(curl --fail --silent --show-error -b "$cookie_file" "$base_url/api/assets/$asset_id")"
  jq -e --arg id "$asset_id" '.asset.id==$id' <<<"$asset" >/dev/null
  event="$(curl --fail --silent --show-error -b "$cookie_file" "$base_url/api/events/$event_id")"
  jq -e --arg id "$event_id" '.event.id==$id' <<<"$event" >/dev/null
  sample="$(curl --fail --silent --show-error -b "$cookie_file" "$base_url/api/assets/$asset_id/samples/$sample_id")"
  jq -e --arg id "$sample_id" '.sample.id==$id' <<<"$sample" >/dev/null
  test "$(sha256sum "$DBSPHERE_GO_CONFIG_DIR/dbsphere-go.env" | cut -d' ' -f1)" = "$config_hash"
}

bash "$old_archive"
login
asset_id="$(jq -nc '{name:"Go offline persistence",host:"127.0.0.1",port:1,
  database:"postgres",username:"monitor",password:"fixture-secret",sslMode:"disable",runtime:"linux"}' |
  curl --fail --silent --show-error -b "$cookie_file" -H "X-CSRF-Token: $csrf" \
    -H 'Content-Type: application/json' --data-binary @- "$base_url/api/assets" | jq -er .asset.id)"
sample_id="$(curl --fail --silent --show-error -b "$cookie_file" -H "X-CSRF-Token: $csrf" \
  -X POST "$base_url/api/assets/$asset_id/check" | jq -er '.result | select(.status=="error") | .sampleId')"
event_id="$(curl --fail --silent --show-error -b "$cookie_file" "$base_url/api/events" |
  jq -er --arg id "$asset_id" 'first(.events[] | select(.assetId==$id) | .id)')"
config_hash="$(sha256sum "$DBSPHERE_GO_CONFIG_DIR/dbsphere-go.env" | cut -d' ' -f1)"
check_state 'dbsphere/offline-go-app:0.6.0-dev.prev'

bash "$new_archive"
check_state "dbsphere/offline-go-app:$new_version"
if [[ "$mode" == external ]]; then
  [[ "$sample_id" =~ ^[0-9a-f-]{36}$ ]]
  ref_count="$(docker exec "$external_container" psql -U postgres -d postgres -Atqc \
    "SELECT count(*) FROM evidence_sample_refs WHERE sample_id='$sample_id'")"
  test "$ref_count" -ge 1
fi

bash "$old_archive"
check_state 'dbsphere/offline-go-app:0.6.0-dev.prev'
echo "Go offline $mode previous → current → previous application drill preserved one PG18 asset, sample and event ID."
