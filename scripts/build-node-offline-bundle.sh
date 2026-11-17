#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -ne 1 || "$1" != /* ]]; then
  echo 'Usage: bash scripts/build-node-offline-bundle.sh /absolute/output/directory' >&2
  exit 1
fi
if [[ "$(uname -s)" != Linux || "$(uname -m)" != x86_64 ]] || ! command -v docker >/dev/null 2>&1; then
  echo 'Linux x86_64 and Docker are required on the online builder.' >&2
  exit 1
fi
version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$repo_dir/package.json" | head -n 1)"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?$ ]]; then
  echo 'Invalid package version.' >&2
  exit 1
fi
output_dir="$(realpath -m -- "$1")"
mkdir -p -- "$output_dir"
stage="$(mktemp -d "$output_dir/.dbsphere-node.XXXXXX")"
container=''
cleanup() {
  if [[ -n "$container" ]]; then docker rm "$container" >/dev/null 2>&1 || true; fi
  local resolved
  resolved="$(realpath -m -- "$stage")"
  if [[ "$resolved" == "$output_dir"/.dbsphere-node.* && -d "$resolved" ]]; then rm -rf -- "$resolved"; fi
}
trap cleanup EXIT

app_image="dbsphere/offline-app:$version"
docker build --platform linux/amd64 -t "$app_image" "$repo_dir"
container="$(docker create "$app_image")"
mkdir -p -- "$stage/app"
docker cp "$container:/app/." "$stage/app/"
docker rm "$container" >/dev/null
container=''
cp -- "$repo_dir/scripts/run-node-offline.sh" "$stage/run.sh"
chmod 0755 "$stage/run.sh"
printf 'DbSphere %s Linux x86_64; requires Node.js >=24.13 and external PostgreSQL 18.\n' "$version" > "$stage/README.txt"
(cd -- "$stage" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
archive="$output_dir/DbSphere-node-$version-linux-amd64.tar.gz"
tar -C "$stage" -czf "$archive" .
(cd -- "$output_dir" && sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256")
echo "Node.js offline bundle: $archive"
