#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -ne 1 || "$1" != /* ]]; then
  echo 'Usage: bash scripts/offline-upgrade-rollback-smoke.sh /absolute/test/directory' >&2
  exit 1
fi
if [[ "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]]; then
  echo 'This drill requires Linux x86_64.' >&2
  exit 1
fi
test_root="$(realpath -m -- "$1")"
mkdir -p -- "$test_root"
old_source="$test_root/v05-source"
old_output="$test_root/v05-bundle"
new_output="$test_root/current-bundle"
export DBSPHERE_CONFIG_DIR="$test_root/config"
export DBSPHERE_NO_PRINT_PASSWORD=true
mkdir -p -- "$old_source/scripts" "$old_source/deploy/compose"
git -C "$repo_dir" archive v0.5.0 | tar -x -C "$old_source"
cp -- "$repo_dir/scripts/build-offline-bundle.sh" \
  "$repo_dir/scripts/install-offline.sh" "$repo_dir/scripts/offline-self-extract.sh" "$old_source/scripts/"
cp -- "$repo_dir/deploy/compose/compose.offline.yaml" "$old_source/deploy/compose/"
bash "$old_source/scripts/build-offline-bundle.sh" "$old_output"
bash "$repo_dir/scripts/build-offline-bundle.sh" "$new_output"
old_archive="$old_output/DbSphere-offline-0.5.0-linux-amd64.run"
new_version="$(node -p "require('$repo_dir/package.json').version")"
new_archive="$new_output/DbSphere-offline-$new_version-linux-amd64.run"
test -s "$old_archive"
test -s "$new_archive"
docker image rm "dbsphere/offline-app:0.5.0" "dbsphere/offline-app:$new_version" postgres:18-bookworm
if docker image inspect "dbsphere/offline-app:0.5.0" >/dev/null 2>&1; then exit 1; fi

bash "$old_archive"
curl --fail --silent http://127.0.0.1:8080/readyz >/dev/null
asset_id="$(node "$repo_dir/scripts/offline-upgrade-rollback-api.mjs" create)"
bash "$new_archive"
curl --fail --silent http://127.0.0.1:8080/readyz >/dev/null
node "$repo_dir/scripts/offline-upgrade-rollback-api.mjs" check "$asset_id"
bash "$old_archive"
curl --fail --silent http://127.0.0.1:8080/readyz >/dev/null
node "$repo_dir/scripts/offline-upgrade-rollback-api.mjs" check "$asset_id"
echo 'Offline V0.5 → current → V0.5 application drill passed with one persistent PG18 asset.'
