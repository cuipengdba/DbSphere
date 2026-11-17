#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -ne 1 || "$1" != /* ]]; then
  echo 'Usage: bash scripts/build-offline-bundle.sh /absolute/output/directory' >&2
  exit 1
fi
if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  echo 'Docker Engine and Docker Compose v2 are required on the online builder.' >&2
  exit 1
fi
if [[ "$(uname -m)" != 'x86_64' ]]; then
  echo 'The current offline bundle builder supports Linux x86_64 only.' >&2
  exit 1
fi
version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$repo_dir/package.json" | head -n 1)"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?$ ]]; then
  echo 'package.json must contain a version before building an offline bundle.' >&2
  exit 1
fi
output_dir="$(realpath -m -- "$1")"
mkdir -p -- "$output_dir"
stage="$(mktemp -d "$output_dir/.dbsphere-offline.XXXXXX")"
payload="$(mktemp "$output_dir/.dbsphere-payload.XXXXXX")"
cleanup() {
  local resolved payload_resolved
  resolved="$(realpath -m -- "$stage")"
  payload_resolved="$(realpath -m -- "$payload")"
  if [[ "$resolved" == "$output_dir"/.dbsphere-offline.* && -d "$resolved" ]]; then
    rm -rf -- "$resolved"
  fi
  if [[ "$payload_resolved" == "$output_dir"/.dbsphere-payload.* && -f "$payload_resolved" ]]; then
    rm -f -- "$payload_resolved"
  fi
}
trap cleanup EXIT

app_image="dbsphere/offline-app:$version"
docker build --platform linux/amd64 -t "$app_image" "$repo_dir"
docker pull --platform linux/amd64 postgres:18-bookworm
mkdir -p -- "$stage/deploy/compose"
cp -- "$repo_dir"/deploy/compose/compose*.yaml "$stage/deploy/compose/"
cp -- "$repo_dir/scripts/install-offline.sh" "$stage/install.sh"
chmod 0755 "$stage/install.sh"
printf 'DBSPHERE_OFFLINE_VERSION=%s\nDBSPHERE_OFFLINE_APP_IMAGE=%s\n' \
  "$version" "$app_image" > "$stage/bundle.env"
docker image save --platform linux/amd64 "$app_image" postgres:18-bookworm | gzip -n > "$stage/images.tar.gz"
(cd -- "$stage" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
archive="$output_dir/DbSphere-offline-$version-linux-amd64.run"
tar -C "$stage" -czf "$payload" .
cat "$repo_dir/scripts/offline-self-extract.sh" "$payload" > "$archive"
chmod 0755 "$archive"
(cd -- "$output_dir" && sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256")
echo "Offline one-click bundle: $archive"
echo "SHA-256: $(sha256sum "$archive" | cut -d ' ' -f 1)"
