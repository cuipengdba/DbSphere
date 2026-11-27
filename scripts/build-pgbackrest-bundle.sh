#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 || "$1" != /* || "$2" != /* || "$4" != /* || ! "$3" =~ ^(amd64|arm64)$ ]]; then
  echo 'Usage: bash scripts/build-pgbackrest-bundle.sh /absolute/static-binary /absolute/licenses.tar.gz amd64|arm64 /absolute/output' >&2
  exit 1
fi
repository="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
version=2.60.1
binary="$1"
licenses="$2"
arch="$3"
out="$(realpath -m -- "$4")"
case "$arch" in amd64) machine='Advanced Micro Devices X86-64' ;; arm64) machine='AArch64' ;; esac
header="$(LC_ALL=C readelf -hW -- "$binary")"
program="$(LC_ALL=C readelf -lW -- "$binary")"
dynamic="$(LC_ALL=C readelf -dW -- "$binary")"
if [[ "$header" != *"$machine"* || "$program" == *INTERP* || "$dynamic" == *NEEDED* ]]; then
  echo 'Expected a statically linked pgBackRest executable for the specified architecture.' >&2
  exit 1
fi
test -s "$licenses"
mkdir -p -- "$out"
stage="$(mktemp -d "$out/.pgbackrest-stage.XXXXXX")"
archive="$(mktemp "$out/.pgbackrest-package.XXXXXX")"
trap 'rm -f -- "$stage/pgbackrest" "$stage/pgbackrest-version" "$stage/architecture" "$stage/install.sh" "$stage/licenses.tar.gz" "$stage/SHA256SUMS" "$archive"; rmdir -- "$stage"' EXIT
install -m 0755 -- "$binary" "$stage/pgbackrest"
install -m 0755 -- "$repository/scripts/install-pgbackrest.sh" "$stage/install.sh"
install -m 0644 -- "$licenses" "$stage/licenses.tar.gz"
printf '%s\n' "$arch" > "$stage/architecture"
printf '%s\n' "$version" > "$stage/pgbackrest-version"
(cd -- "$stage" && sha256sum pgbackrest install.sh licenses.tar.gz architecture pgbackrest-version > SHA256SUMS)
name="DbSphere-pgbackrest-$version-linux-$arch.run"
cat -- "$repository/scripts/offline-self-extract.sh" > "$archive"
tar -C "$stage" -czf - . >> "$archive"
chmod 0755 -- "$archive"
mv -fT -- "$archive" "$out/$name"
(cd -- "$out" && sha256sum "$name" > "$name.sha256")
