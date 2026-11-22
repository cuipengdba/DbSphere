#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || "$1" != /* || "$2" != /* || ! -f "$1" ]]; then
  echo 'Usage: bash scripts/build-go-systemd-bundle.sh /absolute/static-binary /absolute/output' >&2
  exit 1
fi
repo="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
binary="$(basename -- "$1")"
if [[ ! "$binary" =~ ^DbSphere-go-([0-9]+\.[0-9]+\.[0-9]+)-linux-(amd64|arm64)$ ]]; then
  echo 'The static binary filename must include the released version and Linux architecture.' >&2
  exit 1
fi
version="${BASH_REMATCH[1]}"
arch="${BASH_REMATCH[2]}"
out="$2"
mkdir -p -- "$out"
stage="$(mktemp -d "$out/.systemd.XXXXXX")"
payload="$(mktemp "$out/.systemd-payload.XXXXXX")"
trap 'rm -f -- "$stage/dbsphere" "$stage/architecture" "$stage/SHA256SUMS" "$stage/dbsphere-native.service" "$stage/dbsphere.env.example" "$stage/install.sh" "$payload"; rmdir -- "$stage"' EXIT
install -m 0755 -- "$1" "$stage/dbsphere"
cp -- "$repo/scripts/install-go-systemd.sh" "$stage/install.sh"
cp -- "$repo/deploy/systemd/dbsphere-native.service" "$repo/deploy/systemd/dbsphere.env.example" "$stage/"
printf '%s\n' "$arch" > "$stage/architecture"
(cd -- "$stage" && sha256sum dbsphere architecture install.sh dbsphere-native.service dbsphere.env.example > SHA256SUMS)
archive="$out/DbSphere-go-systemd-$version-linux-$arch.run"
tar -C "$stage" -czf "$payload" .
cat "$repo/scripts/offline-self-extract.sh" "$payload" > "$archive"
chmod 0755 -- "$archive"
(cd -- "$out" && sha256sum "$(basename -- "$archive")" > "$(basename -- "$archive").sha256")
