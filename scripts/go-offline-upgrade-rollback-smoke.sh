#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -ne 1 || "$1" != /* || "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]]; then
  echo 'Usage on Linux x86_64: bash scripts/go-offline-upgrade-rollback-smoke.sh /absolute/test/directory' >&2
  exit 1
fi
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
docker image rm 'dbsphere/offline-go-app:0.6.0-dev.prev' "dbsphere/offline-go-app:$new_version" postgres:18-bookworm
if docker image inspect 'dbsphere/offline-go-app:0.6.0-dev.prev' >/dev/null 2>&1; then exit 1; fi

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
  local expected_image="$1" image asset event sample
  curl --fail --silent --show-error "$base_url/readyz" | jq -e '.storage=="postgresql18"' >/dev/null
  image="$(docker inspect -f '{{.Config.Image}}' "$(docker ps -q \
    --filter label=com.docker.compose.project=dbsphere-go-dev \
    --filter label=com.docker.compose.service=app)")"
  test "$image" = "$expected_image"
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

bash "$old_archive"
check_state 'dbsphere/offline-go-app:0.6.0-dev.prev'
echo 'Go offline previous → current → previous application drill preserved one PG18 asset, sample and event ID.'
