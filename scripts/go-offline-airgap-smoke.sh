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
printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_STORE_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_WEB_PORT=18081\nDBSPHERE_COOKIE_SECURE=false\n' \
  "$(openssl rand -hex 24)" "$(openssl rand -hex 24)" "$(openssl rand -base64 32 | tr -d '\n')" \
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
docker image inspect "dbsphere/offline-go-app:$version" postgres:18-bookworm >/dev/null
mapfile -t containers < <(docker ps -aq --filter "label=com.docker.compose.project=$project")
if (( ${#containers[@]} > 0 )); then docker rm -f "${containers[@]}" >/dev/null; fi
mapfile -t volumes < <(docker volume ls -q --filter "label=com.docker.compose.project=$project")
if (( ${#volumes[@]} > 0 )); then docker volume rm "${volumes[@]}" >/dev/null; fi
echo 'Go/PG18 offline bundle installed from local archives with a network-disabled installer.'
