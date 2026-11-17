#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || "$1" != /* ]]; then
  echo 'Usage: scripts/backup-store.sh /absolute/backup/output/directory' >&2
  exit 1
fi

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
env_file="$repo_dir/deploy/compose/.env"
compose_file="$repo_dir/deploy/compose/compose.yaml"
if [[ ! -f "$env_file" ]]; then
  echo 'Run scripts/one-click.sh first.' >&2
  exit 1
fi
if grep -q '^DBSPHERE_DATABASE_URL=' "$env_file"; then
  echo 'This helper backs up the bundled PG18 store. Use PostgreSQL 18 pg_dump for an external store.' >&2
  exit 1
fi

umask 077
mkdir -p -- "$1"
output_dir="$(cd -- "$1" && pwd -P)"
backup_id="dbsphere-$(date -u +%Y%m%dT%H%M%SZ)-$$"
archive="$output_dir/$backup_id.dump"
partial="$output_dir/.$backup_id.partial"
manifest="$output_dir/monitor-store-backup.json"
manifest_partial="$output_dir/.monitor-store-backup.partial"
cleanup() { rm -f -- "$partial" "$manifest_partial"; }
trap cleanup EXIT

restore_point="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
docker compose --env-file "$env_file" -f "$compose_file" exec -T store \
  pg_dump -U dbsphere -d dbsphere -Fc > "$partial"
docker compose --env-file "$env_file" -f "$compose_file" exec -T store \
  pg_restore --list < "$partial" >/dev/null
mv -- "$partial" "$archive"
completed="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '{"status":"success","backupId":"%s","completedAt":"%s","restorePointAt":"%s"}\n' \
  "$backup_id" "$completed" "$restore_point" > "$manifest_partial"
mv -- "$manifest_partial" "$manifest"
chmod 0600 "$archive" "$manifest"
echo "PG18 monitoring store backup: $archive"
echo "Backup manifest: $manifest"
