#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo 'Usage: bash scripts/go-offline-nested-airgap-smoke.sh /absolute/current.run [/absolute/previous.run]' >&2
  exit 1
fi
archive="$(realpath -- "${1:?Pass the absolute Go offline .run path}")"
if [[ ! -f "$archive" || ! -f "$archive.sha256" ]]; then
  echo 'The Go offline installer and adjacent SHA-256 file are required.' >&2
  exit 1
fi
repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
archive_name="$(basename -- "$archive")"
version="$(sed -n 's/^DbSphere-go-offline-\(.*\)-linux-amd64\.run$/\1/p' <<< "$archive_name")"
if [[ -z "$version" ]]; then
  echo 'Unexpected Go offline installer filename.' >&2
  exit 1
fi
previous_archive=''
previous_name=''
previous_version=''
if [[ $# -ge 2 ]]; then
  previous_archive="$(realpath -- "$2")"
  previous_name="$(basename -- "$previous_archive")"
  previous_version="$(sed -n 's/^DbSphere-go-offline-\(.*\)-linux-amd64\.run$/\1/p' <<< "$previous_name")"
  if [[ ! -f "$previous_archive" || ! -f "$previous_archive.sha256" || -z "$previous_version" || "$previous_version" == "$version" ]]; then
    echo 'A distinct previous Go offline installer and adjacent SHA-256 file are required.' >&2
    exit 1
  fi
fi

image='dbsphere/airgap-dind:local'
container="dbsphere-airgap-dind-$$"
log="$(mktemp "${TMPDIR:-/tmp}/dbsphere-airgap.XXXXXX")"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -f -- "$log"
}
trap cleanup EXIT
docker build -q -t "$image" -f "$repo_dir/test/fixtures/Dockerfile.airgap-dind" \
  "$repo_dir/test/fixtures" >/dev/null
mounts=(-v "$(dirname -- "$archive"):/bundle:ro")
if [[ -n "$previous_archive" ]]; then mounts+=(-v "$(dirname -- "$previous_archive"):/previous:ro"); fi
docker run -d --privileged --network none --name "$container" \
  -e DOCKER_TLS_CERTDIR= -e DBSPHERE_NO_PRINT_PASSWORD=true \
  "${mounts[@]}" "$image" >/dev/null
if [[ "$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$container")" != none ]]; then
  echo 'Nested Docker host unexpectedly has an external network.' >&2
  exit 1
fi
ready=false
for _ in $(seq 1 60); do
  if docker exec "$container" docker info >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  docker logs "$container" >&2
  echo 'Nested Docker daemon did not start.' >&2
  exit 1
fi
if docker exec "$container" wget -q -T 3 -O /dev/null https://registry-1.docker.io; then
  echo 'Nested installer unexpectedly reached the external registry.' >&2
  exit 1
fi

install() {
  local installer="$1"
  if ! docker exec "$container" bash "$installer" >"$log" 2>&1; then
    cat "$log" >&2
    return 1
  fi
}
check_health() {
  local expected_version="$1" health version_response
  health="$(docker exec "$container" wget -qO- http://127.0.0.1:8081/readyz)"
  version_response="$(docker exec "$container" wget -qO- http://127.0.0.1:8081/api/version)"
  if [[ "$health" != *'"storage":"postgresql18"'* ||
        "$version_response" != *"\"version\":\"$expected_version\""* ]]; then
    echo "Nested installation failed readiness or version check: $health $version_response" >&2
    exit 1
  fi
}
if [[ -n "$previous_archive" ]]; then
  install "/previous/$previous_name"
  check_health "$previous_version"
else
  install "/bundle/$archive_name"
  check_health "$version"
fi
pg_version="$(docker exec "$container" docker exec dbsphere-go-dev-store-1 \
  psql -U dbsphere -d dbsphere -Atqc "SELECT current_setting('server_version_num')")"
if [[ "$pg_version" != 18* ]]; then
  echo "Nested monitoring store is not PostgreSQL 18: $pg_version" >&2
  exit 1
fi
docker exec "$container" docker exec dbsphere-go-dev-store-1 \
  psql -U dbsphere -d dbsphere -v ON_ERROR_STOP=1 -Atqc \
  "CREATE TABLE dbsphere_airgap_probe (value text PRIMARY KEY); INSERT INTO dbsphere_airgap_probe VALUES ('persisted-before-reinstall');" >/dev/null
install "/bundle/$archive_name"
check_health "$version"
marker="$(docker exec "$container" docker exec dbsphere-go-dev-store-1 \
  psql -U dbsphere -d dbsphere -Atqc 'SELECT value FROM dbsphere_airgap_probe')"
if [[ "$marker" != persisted-before-reinstall ]]; then
  echo 'PostgreSQL 18 data did not survive repeated offline installation.' >&2
  exit 1
fi
if [[ -n "$previous_archive" ]]; then
  install "/previous/$previous_name"
  check_health "$previous_version"
  marker="$(docker exec "$container" docker exec dbsphere-go-dev-store-1 \
    psql -U dbsphere -d dbsphere -Atqc 'SELECT value FROM dbsphere_airgap_probe')"
  if [[ "$marker" != persisted-before-reinstall ]]; then
    echo 'PostgreSQL 18 data did not survive offline rollback.' >&2
    exit 1
  fi
  echo "Nested Docker 27 without external network passed: DbSphere $previous_version → $version → $previous_version, PostgreSQL $pg_version, persistent data."
else
  echo "Nested Docker 27 offline installation passed: DbSphere $version, PostgreSQL $pg_version, persistent data."
fi
