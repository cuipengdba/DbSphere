#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || "$1" != /* || "$2" != /* || ! -f "$1" ]]; then
  echo 'Usage: bash scripts/build-go-systemd-bundle.sh /absolute/static-binary /absolute/output' >&2
  exit 1
fi
repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
binary="$(basename -- "$1")"
if [[ ! "$binary" =~ ^DbSphere-go-([0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?)-linux-(amd64|arm64)$ ]]; then
  echo 'The static binary filename must include the package version and Linux architecture.' >&2
  exit 1
fi
version="${BASH_REMATCH[1]}"
arch="${BASH_REMATCH[3]}"
out="$2"
mkdir -p -- "$out"
stage="$(mktemp -d "$out/.systemd.XXXXXX")"
payload="$(mktemp "$out/.systemd-payload.XXXXXX")"
trap 'rm -f -- "$stage/dbsphere" "$stage/architecture" "$stage/SHA256SUMS" "$stage/dbsphere-native.service" "$stage/dbsphere.env.example" "$stage/install.sh" "$stage/licenses.tar.gz" "$stage/pgbackrest.run" "$stage/pgbackrest.run.sha256" "$payload"; rmdir -- "$stage"' EXIT
install -m 0755 -- "$1" "$stage/dbsphere"
cp -- "$repo/scripts/install-go-systemd.sh" "$stage/install.sh"
cp -- "$repo/deploy/systemd/dbsphere-native.service" "$repo/deploy/systemd/dbsphere.env.example" "$stage/"
printf '%s\n' "$arch" > "$stage/architecture"
tar -C "$repo" -czf "$stage/licenses.tar.gz" LICENSE third_party/licenses/go
bash "$repo/scripts/stage-pgbackrest-bundle.sh" "$stage" "$arch"
(cd -- "$stage" && find . -maxdepth 1 -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS)
archive="$out/DbSphere-go-systemd-$version-linux-$arch.run"
tar -C "$stage" -czf "$payload" .
bash "$repo/scripts/render-offline-header.sh" "$repo/scripts/offline-self-extract.sh" > "$archive"
cat "$payload" >> "$archive"
chmod 0755 -- "$archive"
(cd -- "$out" && sha256sum "$(basename -- "$archive")" > "$(basename -- "$archive").sha256")
