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
if [[ ! "$archive_name" =~ ^DbSphere-go-offline-(.+)-linux-(amd64|arm64)\.run$ ]]; then
  echo 'Unexpected Go offline installer filename.' >&2
  exit 1
fi
version="${BASH_REMATCH[1]}"
arch="${BASH_REMATCH[2]}"
previous_archive=''
previous_name=''
previous_version=''
if [[ $# -ge 2 ]]; then
  previous_archive="$(realpath -- "$2")"
  previous_name="$(basename -- "$previous_archive")"
  previous_version="$(sed -n "s/^DbSphere-go-offline-\(.*\)-linux-$arch\.run$/\1/p" <<< "$previous_name")"
  if [[ ! -f "$previous_archive" || ! -f "$previous_archive.sha256" || -z "$previous_version" || "$previous_version" == "$version" ]]; then
    echo 'A distinct previous Go offline installer and adjacent SHA-256 file are required.' >&2
    exit 1
  fi
fi

image='dbsphere/airgap-dind:local'
ha_test="${DBSPHERE_HA_SMOKE:-false}"
evidence_test="${DBSPHERE_EVIDENCE_SMOKE:-false}"
container="dbsphere-airgap-dind-$$"
log="$(mktemp "${TMPDIR:-/tmp}/dbsphere-airgap.XXXXXX")"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -f -- "$log"
}
trap cleanup EXIT
docker build -q --build-arg "WITH_HA_TEST=$ha_test" -t "$image" -f "$repo_dir/test/fixtures/Dockerfile.airgap-dind" \
  "$repo_dir/test/fixtures" >/dev/null
mounts=(-v "$(dirname -- "$archive"):/bundle:ro")
if [[ -n "$previous_archive" ]]; then mounts+=(-v "$(dirname -- "$previous_archive"):/previous:ro"); fi
if [[ "$ha_test" == true ]]; then mounts+=(-v "$repo_dir/test/fixtures/go-ha-drill.py:/ha-smoke.py:ro"); fi
docker run -d --privileged --network none --name "$container" \
  -e DOCKER_TLS_CERTDIR= -e DBSPHERE_NO_PRINT_PASSWORD=true \
  "${mounts[@]}" "$image" >/dev/null
if [[ "$(docker inspect --format '{{.HostConfig.NetworkMode}}' "$container")" != none ]]; then
  echo 'Nested Docker host unexpectedly has an external network.' >&2
  exit 1
fi
case "$arch:$(docker exec "$container" uname -m)" in
  amd64:x86_64|arm64:aarch64|arm64:arm64) ;;
  *) echo "Nested Docker architecture does not match $arch." >&2; exit 1 ;;
esac
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

admin_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
install() {
  local installer="$1"
  if ! docker exec -e "DBSPHERE_ADMIN_PASSWORD=$admin_password" "$container" bash "$installer" >"$log" 2>&1; then
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
check_administrator() {
  local payload result
  printf -v payload '{"username":"admin","password":"%s"}' "$admin_password"
  result="$(docker exec "$container" wget -qO- --header='Content-Type: application/json' \
    --post-data "$payload" http://127.0.0.1:8081/api/login)"
  if [[ "$result" != *'"authenticated":true'* || "$result" != *'"role":"admin"'* ]]; then
    echo 'Nested installation did not preserve the preconfigured administrator login.' >&2
    exit 1
  fi
}
check_evidence() {
  local count
  count="$(docker exec "$container" docker exec dbsphere-go-dev-store-1 \
    psql -U dbsphere -d dbsphere -Atqc \
    "SELECT count(*) FROM events e JOIN evidence v ON e.id=v.event_id JOIN samples s ON s.id::text=v.payload_json->>'sampleId'
       WHERE e.id='9b7d8499-9a82-4e9b-95a6-54ac2af1ceb1' AND s.asset_id=e.asset_id
       AND s.metrics_json->>'fixture'='offline-upgrade-original';")"
  test "$count" = 1
}
if [[ -n "$previous_archive" ]]; then
  install "/previous/$previous_name"
  check_health "$previous_version"
else
  install "/bundle/$archive_name"
  check_health "$version"
fi
check_administrator
pg_version="$(docker exec "$container" docker exec dbsphere-go-dev-store-1 \
  psql -U dbsphere -d dbsphere -Atqc "SELECT current_setting('server_version_num')")"
if [[ "$pg_version" != 18* ]]; then
  echo "Nested monitoring store is not PostgreSQL 18: $pg_version" >&2
  exit 1
fi
docker exec "$container" docker exec dbsphere-go-dev-store-1 \
  psql -U dbsphere -d dbsphere -v ON_ERROR_STOP=1 -Atqc \
  "CREATE TABLE dbsphere_airgap_probe (value text PRIMARY KEY); INSERT INTO dbsphere_airgap_probe VALUES ('persisted-before-reinstall');" >/dev/null
if [[ "$evidence_test" == true ]]; then
  docker exec -i "$container" docker exec -i dbsphere-go-dev-store-1 \
    psql -U dbsphere -d dbsphere -v ON_ERROR_STOP=1 -q <<'SQL'
INSERT INTO assets(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,created_at)
VALUES ('3953d046-27f8-4af0-b3e9-42f662986945','Offline evidence fixture','store',5432,'dbsphere','dbsphere','','disable','managed',now());
INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
VALUES ('367697f5-447b-4738-9cdf-76117de21b04','3953d046-27f8-4af0-b3e9-42f662986945',now(),'ok','{"fixture":"offline-upgrade-original"}');
INSERT INTO event_keys(id,asset_id,started_at)
VALUES ('9b7d8499-9a82-4e9b-95a6-54ac2af1ceb1','3953d046-27f8-4af0-b3e9-42f662986945',now());
INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at,event_group)
SELECT id,asset_id,'collection_failure','Offline evidence fixture','resolved',started_at,started_at,'availability'
FROM event_keys WHERE id='9b7d8499-9a82-4e9b-95a6-54ac2af1ceb1';
INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
VALUES (gen_random_uuid(),'9b7d8499-9a82-4e9b-95a6-54ac2af1ceb1','collection_recovered',now(),
        '{"sampleId":"367697f5-447b-4738-9cdf-76117de21b04"}');
SQL
  check_evidence
fi
install "/bundle/$archive_name"
check_health "$version"
check_administrator
if [[ "$evidence_test" == true ]]; then check_evidence; fi
marker="$(docker exec "$container" docker exec dbsphere-go-dev-store-1 \
  psql -U dbsphere -d dbsphere -Atqc 'SELECT value FROM dbsphere_airgap_probe')"
if [[ "$marker" != persisted-before-reinstall ]]; then
  echo 'PostgreSQL 18 data did not survive repeated offline installation.' >&2
  exit 1
fi
if [[ "$ha_test" == true ]]; then
  docker exec "$container" python3 /ha-smoke.py "/bundle/$archive_name"
  check_health "$version"
fi
if [[ -n "$previous_archive" ]]; then
  install "/previous/$previous_name"
  check_health "$previous_version"
  check_administrator
  if [[ "$evidence_test" == true ]]; then check_evidence; fi
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
