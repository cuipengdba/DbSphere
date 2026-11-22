#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || "$1" != /* ]]; then
  echo 'Usage: bash backup-go-store.sh /absolute/backup/output/directory' >&2
  exit 1
fi
if ! command -v docker >/dev/null 2>&1; then
  echo 'Docker is required for the bundled PostgreSQL 18 backup.' >&2
  exit 1
fi

mapfile -t containers < <(docker ps -q \
  --filter label=com.docker.compose.project=dbsphere-go-dev \
  --filter label=com.docker.compose.service=store)
if [[ ${#containers[@]} -ne 1 ]]; then
  echo 'Expected one running DbSphere Go bundled PostgreSQL 18 container. For external PG18, use PostgreSQL 18 pg_dump.' >&2
  exit 1
fi

umask 077
mkdir -p -- "$1"
output_dir="$(cd -- "$1" && pwd -P)"
backup_id="dbsphere-go-$(date -u +%Y%m%dT%H%M%SZ)-$$"
archive="$output_dir/$backup_id.dump"
manifest="$output_dir/$backup_id.json"
partial="$output_dir/.$backup_id.partial"
manifest_partial="$output_dir/.$backup_id.json.partial"
cleanup() { rm -f -- "$partial" "$manifest_partial"; }
trap cleanup EXIT

started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
docker exec -i "${containers[0]}" pg_dump -U dbsphere -d dbsphere -Fc > "$partial"
docker exec -i "${containers[0]}" pg_restore --list < "$partial" >/dev/null
test -s "$partial"
digest="$(sha256sum "$partial" | cut -d ' ' -f 1)"
completed="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '{"status":"success","backupId":"%s","completedAt":"%s","restorePointAt":"%s","archive":"%s","sha256":"%s"}\n' \
  "$backup_id" "$completed" "$started" "$(basename -- "$archive")" "$digest" > "$manifest_partial"
mv -- "$partial" "$archive"
mv -- "$manifest_partial" "$manifest"
echo "PostgreSQL 18 backup: $archive"
echo "SHA-256 and manifest: $manifest"
