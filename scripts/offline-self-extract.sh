#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != 'Linux' ]]; then
  echo 'DbSphere offline installer runs on Linux only.' >&2
  exit 1
fi
self="$(realpath -- "$0")"
checksum="$self.sha256"
if [[ -f "$checksum" ]]; then
  (cd -- "$(dirname -- "$self")" && sha256sum --check --quiet "$(basename -- "$checksum")") || {
    echo 'Offline installer SHA-256 verification failed.' >&2
    exit 1
  }
fi
payload_line="$(awk '/^__DBSPHERE_PAYLOAD__$/ { print NR + 1; exit }' "$self")"
if [[ -z "$payload_line" ]]; then
  echo 'Offline installer payload marker is missing.' >&2
  exit 1
fi
temp_root="${TMPDIR:-/tmp}"
stage="$(mktemp -d "$temp_root/dbsphere-offline.XXXXXX")"
cleanup() {
  local resolved
  resolved="$(realpath -m -- "$stage")"
  if [[ "$resolved" == "$(realpath -m -- "$temp_root")"/dbsphere-offline.* && -d "$resolved" ]]; then
    rm -rf -- "$resolved"
  fi
}
trap cleanup EXIT
tail -n +"$payload_line" "$self" | tar -xz -C "$stage"
bash "$stage/install.sh" "$@"
exit 0
__DBSPHERE_PAYLOAD__
