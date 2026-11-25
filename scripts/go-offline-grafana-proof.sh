#!/usr/bin/env bash
set -Eeuo pipefail
trap 'printf "Grafana acceptance failed at line %s.\n" "$LINENO" >&2' ERR

if [[ $# -ne 6 ]]; then
  echo 'Usage: go-offline-grafana-proof.sh project origin cookie csrf asset viewer-password' >&2
  exit 1
fi
project="$1"
base="$2"
cookie="$3"
csrf="$4"
asset="$5"
viewer="$6"
scratch="$(dirname -- "$cookie")"
grafana="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=grafana)"
store="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=store)"
test -n "$grafana"
test -n "$store"
test -z "$(docker port "$grafana")"
test "$(docker network inspect --format '{{.Internal}}' "${project}_grafana-private")" = true
api() {
  curl -fsS -b "$cookie" -H "X-CSRF-Token: $csrf" "$@"
}
test "$(curl -sS -H 'X-WEBAUTH-USER: admin' -o /dev/null -w '%{http_code}' "$base/grafana/api/user")" = 401
api "$base/api/live/integrations" | jq -e '.grafana and .prometheus=="ready"' >/dev/null
jq -nc --arg password "$viewer" '{username:"viewer",password:$password}' |
  curl -fsS -c "$scratch/viewer.cookie" -H 'Content-Type: application/json' --data-binary @- "$base/api/login" |
  jq -e '.authenticated and .role=="viewer"' >/dev/null
test "$(curl -sS -b "$scratch/viewer.cookie" -o /dev/null -w '%{http_code}' "$base/grafana/api/user")" = 403
api -H 'X-WEBAUTH-USER: admin' "$base/grafana/api/user" |
  jq -e '.login=="dbsphere-local-admin" and (.isGrafanaAdmin|not)' >/dev/null
api -D "$scratch/frame.headers" -o "$scratch/frame.html" -H 'Sec-Fetch-Dest: iframe' \
  "$base/grafana/d/dbsphere-overview?kiosk"
tr -d '\r' < "$scratch/frame.headers" | grep -Fxi 'X-Frame-Options: SAMEORIGIN' >/dev/null
grep -Fqi "frame-ancestors 'self'" "$scratch/frame.headers"
grep -q 'grafanaBootData' "$scratch/frame.html"
if grep -Eqi '^(Location:.*login|Set-Cookie:)' "$scratch/frame.headers"; then exit 1; fi
echo 'Grafana private network and DbSphere access roles passed.'

for topic in overview postgres waits replication infrastructure mysql dm8 kingbase redis etcd; do
  panels=8
  if [[ "$topic" == waits ]]; then panels=10; fi
  if [[ "$topic" == infrastructure ]]; then panels=12; fi
  api "$base/grafana/api/dashboards/uid/dbsphere-$topic" |
    jq -e --arg uid "dbsphere-$topic" --argjson panels "$panels" '.dashboard.uid==$uid and (.dashboard.panels|length)==$panels and
      (.dashboard.links|any(.url? // ""|startswith("/grafana/dbsphere-return?view=event")))' >/dev/null
done
echo 'Grafana dashboard provisioning passed for all ten topics.'
for topic in io cache volumes rca; do
  panels=4
  if [[ "$topic" == io ]]; then panels=6; fi
  if [[ "$topic" == volumes ]]; then panels=5; fi
  api "$base/grafana/api/dashboards/uid/dbsphere-container-$topic" |
    jq -e --arg uid "dbsphere-container-$topic" --argjson panels "$panels" '.dashboard.uid==$uid and (.dashboard.panels|length)==$panels and
      (.dashboard.links|any(.url? // ""|startswith("/grafana/dbsphere-return?view=storage")))' >/dev/null
done
api "$base/grafana/api/dashboards/uid/dbsphere-container-volumes" |
  jq -c --arg asset "$asset" '{from:"now-5m",to:"now",queries:([.dashboard.panels[].targets[].expr]
    | to_entries | map({refId:("V"+(.key|tostring)),range:true,intervalMs:15000,maxDataPoints:100,
       datasource:{type:"prometheus",uid:"dbsphere-prometheus"},
       expr:(.value | gsub("\\$\\{asset:raw\\}";$asset) | gsub("\\$\\{[a-z_]+:raw\\}";".*"))}))}' |
  api -H 'Content-Type: application/json' --data-binary @- "$base/grafana/api/ds/query" |
  jq -e '.results | length>0 and all(.[]; .error == null and (.status // 200) == 200)' >/dev/null
echo 'Grafana container storage specialties passed for all four topics.'
query="$(
  jq -nc --arg asset "$asset" '{from:"now-5m",to:"now",queries:[{refId:"A",range:true,intervalMs:15000,maxDataPoints:100,
    datasource:{type:"prometheus",uid:"dbsphere-prometheus"},expr:("dbsphere_asset_connections{asset_id=\""+$asset+"\"}")}]}'
)"
found_grafana_samples=false
for _ in {1..20}; do
  # Grafana proxy authentication and the first Prometheus range step can settle
  # after the readiness checks and the successful instant scrape query.
  if printf '%s' "$query" |
    api -H 'Content-Type: application/json' --data-binary @- "$base/grafana/api/ds/query" |
    jq -e '(.results.A.frames // []) | any((.data.values[1] // []) | any(type=="number"))' >/dev/null; then
    found_grafana_samples=true
    break
  fi
  sleep 2
done
test "$found_grafana_samples" = true
echo 'Grafana queried a real asset sample through Prometheus.'

psql() {
  docker exec "$store" psql -X -U dbsphere -d dbsphere -At -v ON_ERROR_STOP=1 "$@"
}
start_session() {
  docker exec -e "PGAPPNAME=$1" "$store" psql -X -U dbsphere -d dbsphere -At -v ON_ERROR_STOP=1 -c "$2"
}
api -X PATCH -H 'Content-Type: application/json' --data '{"durationSeconds":0}' "$base/api/alert-rules/pg_lock_wait" >/dev/null
start_session dbsphere_grafana_lock_root 'BEGIN; SELECT pg_advisory_xact_lock(83224950); SELECT pg_sleep(70); ROLLBACK' \
  > "$scratch/grafana-lock-root.log" 2>&1 &
holder=$!
release_lock() {
  psql -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name='dbsphere_grafana_lock_root'" >/dev/null || true
}
trap release_lock EXIT
root_pid=''
for _ in {1..35}; do
  root_pid="$(psql -c "SELECT pid FROM pg_locks WHERE locktype='advisory' AND objid=83224950 AND granted LIMIT 1")"
  if [[ -n "$root_pid" ]]; then break; fi
  sleep 0.4
done
test -n "$root_pid"
start_session dbsphere_grafana_lock_waiter 'SELECT pg_advisory_xact_lock(83224950)' > "$scratch/grafana-lock-waiter.log" 2>&1 &
waiter=$!
waiting_pid=''
for _ in {1..35}; do
  waiting_pid="$(psql -c "SELECT pid FROM pg_locks WHERE locktype='advisory' AND objid=83224950 AND NOT granted LIMIT 1")"
  if [[ -n "$waiting_pid" ]]; then break; fi
  sleep 0.4
done
test -n "$waiting_pid"
sample="$(api -X POST "$base/api/assets/$asset/check" | jq -er .result.sampleId)"
event="$(api "$base/api/live/overview" |
  jq -er --arg asset "$asset" '.events|map(select(.assetId==$asset and .status=="open" and (.title|contains("锁"))))[0].id')"
test -n "$event"
api "$base/api/events/$event" |
  jq -e --arg id "$event" --arg root "$root_pid" --arg waiting "$waiting_pid" \
    '.event.id==$id and (.analysis|tostring|(contains($root) and contains($waiting)))' >/dev/null
api "$base/grafana/d/dbsphere-waits?var-event=$event&var-asset=$asset&from=now-15m&to=now" > "$scratch/grafana-waits.html"
grep -Eq 'grafanaBootData|Grafana' "$scratch/grafana-waits.html"
api -H 'Sec-Fetch-Dest: iframe' -G --data-urlencode 'view=event' --data-urlencode "event=$event" \
  --data-urlencode "context=incidents?event=$event&focusPhase=after" "$base/grafana/dbsphere-return" > "$scratch/frame-return.html"
grep -Fq 'id="dbsphere-grafana-return"' "$scratch/frame-return.html"
grep -Fq "href=\"/#incidents?event=$event&amp;focusPhase=after\"" "$scratch/frame-return.html"
redirect="$(api -D - -o /dev/null "$base/grafana/dbsphere-return?view=event&event=$event&asset=$asset&from=2026-01-01T00:00:00Z&to=2026-01-01T00:15:00Z" |
  tr -d '\r' | sed -n 's/^Location: //p')"
test "$redirect" = "/#incidents?asset=$asset&event=$event&from=2026-01-01T00%3A00%3A00Z&to=2026-01-01T00%3A15%3A00Z"
redirect="$(api -D - -o /dev/null --get --data-urlencode "view=event" --data-urlencode "event=$event" \
  --data-urlencode "context=incidents?event=$event&focusPhase=after&return=overview%3Fenvironment%3DProduction" \
  "$base/grafana/dbsphere-return" | tr -d '\r' | sed -n 's/^Location: //p')"
test "$redirect" = "/#incidents?event=$event&focusPhase=after&return=overview%3Fenvironment%3DProduction"
release_lock
wait "$holder" || true
wait "$waiter"
api -X POST "$base/api/assets/$asset/check" >/dev/null
api "$base/api/events/$event" | jq -e --arg id "$event" '.event.id==$id and .event.status=="resolved"' >/dev/null
trap - EXIT
printf 'Grafana with private Prometheus displayed 14 dashboards. Native lock sample %s and recovery shared event %s.\n' "$sample" "$event"
