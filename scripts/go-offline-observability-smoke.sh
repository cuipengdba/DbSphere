#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || "$1" != /* || "$2" != /* ]]; then
  echo 'Usage: bash scripts/go-offline-observability-smoke.sh /absolute/bundle.run /absolute/test-directory' >&2
  exit 1
fi
bundle="$(realpath -- "$1")"
root="$(realpath -m -- "$2")"
test "$root" != /
mkdir -p -- "$root/config" "$root/stage"
project="dbsphere-observability-ci-$$"
app_port=18149
base="http://127.0.0.1:$app_port"
admin="$(openssl rand -hex 24)"
store="$(openssl rand -hex 24)"
metrics="$(openssl rand -hex 24)"
viewer="$(openssl rand -hex 24)"
image=dbsphere/offline-prometheus:3.13.5
fixture=busybox:1.37.0
cleanup() {
  local containers volumes
  mapfile -t containers < <(docker ps -aq --filter "label=com.docker.compose.project=$project")
  if (( ${#containers[@]} )); then docker rm -f "${containers[@]}" >/dev/null; fi
  if docker container inspect "$project-exporter" >/dev/null 2>&1; then docker rm -f "$project-exporter" >/dev/null; fi
  mapfile -t volumes < <(docker volume ls -q --filter "label=com.docker.compose.project=$project")
  if (( ${#volumes[@]} )); then docker volume rm "${volumes[@]}" >/dev/null; fi
  docker network rm "${project}_default" >/dev/null 2>&1 || true
}
trap cleanup EXIT

umask 077
printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_STORE_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_WEB_PORT=%s\nDBSPHERE_METRICS_TOKEN=%s\nDBSPHERE_VIEWER_PASSWORD=%s\n' \
  "$admin" "$store" "$(openssl rand -base64 32)" "$app_port" "$metrics" "$viewer" > "$root/config/dbsphere-go.env"
docker build -q -t dbsphere/observability-airgap-harness:ci - <<'DOCKERFILE'
FROM docker:27-cli@sha256:851f91d241214e7c6db86513b270d58776379aacc5eb9c4a87e5b47115e3065c
RUN apk add --no-cache bash coreutils
DOCKERFILE
docker pull "$fixture" >/dev/null
docker image rm "$image" >/dev/null
install() {
  docker run --rm --network none --entrypoint bash \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$(dirname -- "$bundle"):$(dirname -- "$bundle"):ro" -v "$root:$root" \
    -e "TMPDIR=$root/stage" -e "DBSPHERE_GO_CONFIG_DIR=$root/config" -e "COMPOSE_PROJECT_NAME=$project" \
    dbsphere/observability-airgap-harness:ci -c 'umask 077; exec bash "$@"' _ "$bundle" "$@"
}
install
prom="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=prometheus)"
test -n "$prom"
test -z "$(docker port "$prom")"
docker exec "$prom" /bin/promtool check config /etc/prometheus/dbsphere.yml
test "$(curl -sS -o /dev/null -w '%{http_code}' "$base/metrics")" = 401
curl -fSs "$base/readyz" | jq -e '.storage=="postgresql18"' >/dev/null
csrf="$(jq -nc --arg password "$admin" '{username:"admin",password:$password}' |
  curl -fsS -c "$root/session.cookie" -H 'Content-Type: application/json' --data-binary @- "$base/api/login" | jq -er .csrf)"
api() {
  curl -fsS -b "$root/session.cookie" -H "X-CSRF-Token: $csrf" "$@"
}
api "$base/api/live/integrations" | jq -e '.prometheus=="ready"' >/dev/null

cat > "$root/metrics" <<'METRICS'
node_cpu_seconds_total{cpu="0",mode="idle"} 10
node_cpu_seconds_total{cpu="0",mode="system"} 1
node_memory_MemTotal_bytes 1000
node_memory_MemAvailable_bytes 750
node_load1 0.3
METRICS
chmod 0644 "$root/metrics"
docker run -d --name "$project-exporter" --pull never --network "${project}_default" \
  --network-alias observation-exporter -v "$root/metrics:/www/metrics:ro" "$fixture" \
  httpd -f -p 9100 -h /www >/dev/null

asset_config="$(jq -nc --arg password "$store" '{name:"Offline observability",host:"store",port:5432,database:"dbsphere",
    username:"dbsphere",password:$password,sslMode:"disable",runtime:"linux",linuxHost:"observation-exporter",
    linuxExporterPort:9100,clusterName:"offline-ci"}')"
asset="$(printf '%s' "$asset_config" |
  api -H 'Content-Type: application/json' --data-binary @- "$base/api/assets" | jq -er .asset.id)"
api -X POST "$base/api/assets/$asset/check" | jq -e '.result.sampleId!=null' >/dev/null
metric_url() {
  docker exec "$prom" /bin/wget -q -O - "http://127.0.0.1:9090/$1"
}
seen=false
for _ in {1..45}; do
  if metric_url 'api/v1/query?query=dbsphere_asset_connections' |
    jq -e --arg id "$asset" '.data.result|any(.metric.asset_id==$id and .metric.cluster=="offline-ci" and .metric.engine=="postgres")' >/dev/null &&
    metric_url 'api/v1/query?query=node_memory_MemAvailable_bytes' |
    jq -e --arg id "$asset" '.data.result|any(.metric.asset_id==$id and .value[1]=="750")' >/dev/null; then
    seen=true
    break
  fi
  sleep 2
done
test "$seen" = true
metric_url 'api/v1/rules' | jq -e '.data.groups[]|select(.name=="dbsphere.observation")|.rules|length>=5' >/dev/null

if [[ -d deploy/observability/grafana/panels ]]; then
  bash scripts/go-offline-grafana-proof.sh "$project" "$base" "$root/session.cookie" "$csrf" "$asset" "$viewer"
fi

jq '.name="Renamed offline observability"|del(.password)' <<< "$asset_config" |
  api -X PATCH -H 'Content-Type: application/json' --data-binary @- "$base/api/assets/$asset" >/dev/null
seen=false
for _ in {1..30}; do
  if metric_url 'api/v1/targets?state=active' |
    jq -e --arg id "$asset" '.data.activeTargets|any(.labels.asset_id==$id and .labels.asset_name=="Renamed offline observability" and .health=="up")' >/dev/null; then
    seen=true
    break
  fi
  sleep 2
done
test "$seen" = true
jq '.linuxHost=""|.linuxExporterPort=0|del(.password)' <<< "$asset_config" |
  api -X PATCH -H 'Content-Type: application/json' --data-binary @- "$base/api/assets/$asset" >/dev/null
seen=false
for _ in {1..30}; do
  if metric_url 'api/v1/targets?state=active' |
    jq -e --arg id "$asset" '.data.activeTargets|all(.labels.asset_id!=$id)' >/dev/null; then
    seen=true
    break
  fi
  sleep 2
done
test "$seen" = true

docker stop "$prom" >/dev/null
api "$base/api/live/integrations" | jq -e '.prometheus=="unavailable"' >/dev/null
api -X POST "$base/api/assets/$asset/check" | jq -e '.result.sampleId!=null' >/dev/null
api "$base/api/live/overview" | jq -e --arg id "$asset" '.assets|any(.id==$id)' >/dev/null
install --disable-observability
api "$base/api/live/integrations" | jq -e '.prometheus=="disabled"' >/dev/null
if [[ -d deploy/observability/grafana/panels ]]; then
  test "$(curl -sS -b "$root/session.cookie" -o /dev/null -w '%{http_code}' "$base/grafana/api/health")" = 404
fi
api "$base/api/live/overview" | jq -e --arg id "$asset" '.assets|any(.id==$id)' >/dev/null
api "$base/api/live/overview?board=wallboard&asset=$asset" |
  jq -e --arg id "$asset" '(.total>=1) and (.assets[0].id==$id) and (.hostCount>=0)' >/dev/null
api "$base/api/assets/$asset/wallboard-history" |
  jq -e '.points|any(.sampleId!=null and .values.connections>=0)' >/dev/null
curl --fail --silent "$base/wallboard.js" | grep 'DbSphereWallboard' >/dev/null
echo 'Enhanced offline installation verified private Prometheus, current database data, dynamic exporter labels, and native degradation.'
