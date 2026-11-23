#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || "$1" != /* || "$2" != /* ]]; then
  echo 'Usage: bash scripts/go-offline-airgap-smoke.sh /absolute/bundle.run /absolute/test-root' >&2
  exit 1
fi
archive="$(realpath -- "$1")"
root="$(realpath -m -- "$2")"
if [[ ! -f "$archive" || ! -f "$archive.sha256" || "$root" == / ]]; then
  echo 'Bundle or isolated test directory is invalid.' >&2
  exit 1
fi
for tool in docker curl jq openssl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "Missing test tool: $tool" >&2; exit 1; }
done
docker compose version >/dev/null
mkdir -p -- "$root/config" "$root/stage"
umask 077
printf 'DBSPHERE_ADMIN_PASSWORD=\nDBSPHERE_STORE_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_WEB_PORT=18081\nDBSPHERE_COOKIE_SECURE=false\n' \
  "$(openssl rand -hex 24)" "$(openssl rand -base64 32 | tr -d '\n')" \
  > "$root/config/dbsphere-go.env"

# The harness is prepared while online; only the installer runs with no network.
docker build --quiet -t dbsphere/offline-airgap-harness:ci - <<'DOCKERFILE'
FROM docker:27-cli@sha256:851f91d241214e7c6db86513b270d58776379aacc5eb9c4a87e5b47115e3065c
RUN apk add --no-cache bash coreutils
DOCKERFILE

version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' package.json | head -n 1)"
docker image rm "dbsphere/offline-go-app:$version" postgres:18-bookworm >/dev/null
if docker image inspect "dbsphere/offline-go-app:$version" >/dev/null 2>&1; then
  echo 'Bundled application image was not removed before the airgap test.' >&2
  exit 1
fi

project=dbsphere-go-airgap-ci
docker run --rm --network none --name dbsphere-go-airgap-installer \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$(dirname -- "$archive"):$(dirname -- "$archive"):ro" \
  -v "$root:$root" \
  -e "TMPDIR=$root/stage" \
  -e "DBSPHERE_GO_CONFIG_DIR=$root/config" \
  -e "COMPOSE_PROJECT_NAME=$project" \
  -e DBSPHERE_NO_PRINT_PASSWORD=true \
  --entrypoint bash dbsphere/offline-airgap-harness:ci "$archive"

curl --fail --silent --show-error http://127.0.0.1:18081/readyz |
  jq -e '.storage == "postgresql18"' >/dev/null
base='http://127.0.0.1:18081'
initial='{"username":"admin","password":"123456"}'
curl --fail --silent "$base/api/session" | jq -e '.setupRequired and .setupLocal and (.authenticated | not)' >/dev/null
test "$(curl --silent -o /dev/null -w '%{http_code}' "$base/api/assets")" = 401
test "$(curl --silent -o /dev/null -w '%{http_code}' -H 'Host: ops.example.invalid' \
  -H 'Content-Type: application/json' --data "$initial" "$base/api/login")" = 403
curl --fail --silent --show-error -c "$root/setup-cookie" -H 'Content-Type: application/json' --data "$initial" "$base/api/login" |
  jq -e '.setupRequired and (.authenticated | not)' >/dev/null
test "$(curl --silent -b "$root/setup-cookie" -o /dev/null -w '%{http_code}' "$base/api/assets")" = 401
new_password="$(openssl rand -hex 24)"
jq -nc --arg password "$new_password" '{username:"admin",password:"123456",newPassword:$password}' |
  curl --fail --silent --show-error -c "$root/admin-cookie" -H 'Content-Type: application/json' --data-binary @- "$base/api/login" |
  jq -e '.authenticated and .role=="admin"' >/dev/null
test "$(curl --silent -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' --data "$initial" "$base/api/login")" = 401
app_id="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=app)"
test -n "$app_id"
docker restart "$app_id" >/dev/null
for attempt in {1..30}; do
  if curl --fail --silent "$base/readyz" >/dev/null; then break; fi
  sleep 1
done
jq -nc --arg password "$new_password" '{username:"admin",password:$password}' |
  curl --fail --silent --show-error -H 'Content-Type: application/json' --data-binary @- "$base/api/login" |
  jq -e '.authenticated and .role=="admin"' >/dev/null
echo 'Local first-use password change, session restriction, and restart persistence passed.'
docker image inspect "dbsphere/offline-go-app:$version" postgres:18-bookworm >/dev/null
mapfile -t containers < <(docker ps -aq --filter "label=com.docker.compose.project=$project")
if (( ${#containers[@]} > 0 )); then docker rm -f "${containers[@]}" >/dev/null; fi
mapfile -t volumes < <(docker volume ls -q --filter "label=com.docker.compose.project=$project")
if (( ${#volumes[@]} > 0 )); then docker volume rm "${volumes[@]}" >/dev/null; fi
echo 'Go/PG18 offline bundle installed from local archives with a network-disabled installer.'
