#!/usr/bin/env bash
set -euo pipefail

archive="${1:?pass the native installer path}"
version="$(jq -er .version package.json)"
base=http://127.0.0.1:18082
store=dbsphere-native-ci-pg18
seed=dbsphere-native-ci-seed
unit=dbsphere-native.service
scratch="$(mktemp -d "${RUNNER_TEMP:-/tmp}/dbsphere-native-check.XXXXXX")"
finish() {
  if [[ "$1" != 0 ]]; then sudo journalctl --unit "$unit" --no-pager -n 25 >&2 || true; fi
  sudo systemctl disable --now "$unit" >/dev/null 2>&1 || true
  sudo rm -f /etc/dbsphere-native/dbsphere.env /etc/systemd/system/dbsphere-native.service /opt/dbsphere-native/dbsphere
  sudo systemctl daemon-reload
  docker rm -f "$seed" "$store" >/dev/null 2>&1 || true
  rm -f -- "$scratch/env" "$scratch/read-only.env" "$scratch/cookies"
  rmdir -- "$scratch"
}
# This drill owns these paths on its isolated hosted Linux test VM.
test ! -e /etc/systemd/system/dbsphere-native.service
test ! -e /etc/dbsphere-native/dbsphere.env
test ! -e /opt/dbsphere-native/dbsphere
trap 'finish "$?"' EXIT
pg_password="$(openssl rand -hex 24)"
admin_password="$(openssl rand -hex 24)"
master_key="$(openssl rand -base64 32)"
umask 077
printf 'DBSPHERE_DATABASE_URL=postgres://postgres:%s@127.0.0.1:15482/postgres?sslmode=disable\nDBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_HOST=127.0.0.1\nDBSPHERE_PORT=18082\n' \
  "$pg_password" "$admin_password" "$master_key" > "$scratch/env"
docker run -d --pull never --name "$store" -p 127.0.0.1:15482:5432 -e "POSTGRES_PASSWORD=$pg_password" postgres:18-bookworm >/dev/null
for _ in {1..40}; do
  if docker exec "$store" pg_isready -h 127.0.0.1 -U postgres >/dev/null; then break; fi
  sleep 1
done
psql_ci() { docker exec "$store" psql -U postgres -Atq -v ON_ERROR_STOP=1 -c "$1"; }

sudo bash "$archive" --check "$scratch/env" | jq -e '.serverVersion >= 180000 and .serverVersion < 190000 and .schemaVersion == 0' >/dev/null
test "$(psql_ci "SELECT to_regclass('public.dbsphere_schema_migrations') IS NULL")" = t
sed 's/sslmode=disable/sslmode=disable\&options=-c%20default_transaction_read_only%3Don/' "$scratch/env" > "$scratch/read-only.env"
if output="$(sudo bash "$archive" --check "$scratch/read-only.env" 2>&1)"; then exit 1; fi
grep -Fq 'store migrations require write access' <<< "$output"
test ! -e /etc/systemd/system/dbsphere-native.service

# Seed a real asset, fault and original sample through the containerized app.
# The database is independent of both application processes.
docker run -d --pull never --name "$seed" --network host --env-file "$scratch/env" "dbsphere/offline-go-app:$version" >/dev/null
for _ in {1..30}; do
  if curl -fs "$base/readyz" >/dev/null; then break; fi
  sleep 1
done
login() {
  csrf="$(jq -nc --arg password "$admin_password" '{password:$password}' |
    curl -fsS -c "$scratch/cookies" -H 'Content-Type: application/json' --data-binary @- "$base/api/login" | jq -er .csrf)"
}
login
asset="$(jq -nc '{name:"Native switch incident",host:"127.0.0.1",port:1,database:"postgres",username:"dbsphere_monitor",password:"fixture-password",sslMode:"disable",runtime:"linux"}' |
  curl -fsS -b "$scratch/cookies" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' --data-binary @- "$base/api/assets" | jq -er .asset.id)"
sample="$(curl -fsS -b "$scratch/cookies" -H "X-CSRF-Token: $csrf" -X POST "$base/api/assets/$asset/check" |
  jq -er '.result | select(.status=="error") | .sampleId')"
event="$(curl -fsS -b "$scratch/cookies" "$base/api/events" | jq -er --arg asset "$asset" 'first(.events[] | select(.assetId==$asset) | .id)')"
docker rm -f "$seed" >/dev/null

sudo bash "$archive" --check "$scratch/env" | jq -e '.schemaVersion > 0' >/dev/null
sudo bash "$archive" "$scratch/env"
systemctl is-enabled --quiet "$unit"
test "$(sudo stat -c '%a' /etc/dbsphere-native/dbsphere.env)" = 600
sudo cmp /etc/dbsphere-native/dbsphere.env "$scratch/env"
verify() {
  curl -fsS "$base/readyz" | jq -e '.storage=="postgresql18"' >/dev/null
  curl -fsS "$base/api/version" | jq -e --arg version "$version" '.version==$version' >/dev/null
  login
  curl -fsS -b "$scratch/cookies" "$base/api/assets/$asset" | jq -e --arg id "$asset" '.asset.id==$id' >/dev/null
  curl -fsS -b "$scratch/cookies" "$base/api/events/$event" | jq -e --arg id "$event" '.event.id==$id' >/dev/null
  curl -fsS -b "$scratch/cookies" "$base/api/assets/$asset/samples/$sample" | jq -e --arg id "$sample" '.sample.id==$id' >/dev/null
}
verify
sudo systemctl restart "$unit"
verify
sudo bash "$archive"
verify
jq -nc --arg arch "$(uname -m)" --arg event "$event" --arg sample "$sample" --arg asset "$asset" \
  --argjson pgVersion "$(psql_ci 'SHOW server_version_num')" '{arch:$arch,pgVersion:$pgVersion,asset:$asset,event:$event,sample:$sample,bootEnabled:true,externalStore:true}'
