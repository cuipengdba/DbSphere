#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != 'Linux' ]]; then
  echo 'DbSphere offline installer runs on Linux only.' >&2
  exit 1
fi
self="$(realpath -- "$0")"
checksum="$self.sha256"
if [[ '@DBSPHERE_SIGNED_RELEASE@' == 'required' || -n "${DBSPHERE_RELEASE_VERIFY_KEY:-}" && "$(basename -- "$self")" == DbSphere-go-*.run ]]; then
  release_dir="$(dirname -- "$self")"
  if [[ ! -r "${DBSPHERE_RELEASE_VERIFY_KEY:-/nonexistent}" || ! -r "$release_dir/SHA256SUMS" || ! -r "$release_dir/SHA256SUMS.sig" ]]; then
    echo 'Signed release requires a trusted public key, SHA256SUMS and SHA256SUMS.sig.' >&2
    exit 1
  fi
  openssl pkeyutl -verify -pubin -inkey "$DBSPHERE_RELEASE_VERIFY_KEY" -rawin \
    -in "$release_dir/SHA256SUMS" -sigfile "$release_dir/SHA256SUMS.sig" >/dev/null || {
    echo 'Release manifest signature verification failed.' >&2
    exit 1
  }
  name="$(basename -- "$self")"
  digest="$(awk -v name="$name" 'NF==2 && $2==name {print $1}' "$release_dir/SHA256SUMS")"
  if [[ ! "$digest" =~ ^[0-9a-f]{64}$ ]]; then
    echo 'Installer is absent from the signed release manifest.' >&2
    exit 1
  fi
  printf '%s  %s\n' "$digest" "$self" | sha256sum --check --status || {
    echo 'Installer did not match the signed release manifest.' >&2
    exit 1
  }
fi
if [[ ! -f "$checksum" ]]; then
  echo 'Offline installer SHA-256 file is missing.' >&2
  exit 1
fi
(cd -- "$(dirname -- "$self")" && sha256sum --check --quiet "$(basename -- "$checksum")") || {
  echo 'Offline installer SHA-256 verification failed.' >&2
  exit 1
}
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
