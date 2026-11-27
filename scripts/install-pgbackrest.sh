#!/usr/bin/env bash
set -euo pipefail

payload="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
check=false
if [[ "${1:-}" == --check ]]; then check=true; shift; fi
if [[ $# -ne 1 || "$1" != /* ]]; then
  echo 'Usage: bash DbSphere-pgbackrest-*.run [--check] /absolute/install-directory' >&2
  exit 1
fi

[[ $(uname -s) == Linux ]]
case "$(uname -m):$(cat -- "$payload/architecture")" in
  x86_64:amd64|aarch64:arm64|arm64:arm64) ;;
  *) echo 'The pgBackRest package architecture does not match this host.' >&2; exit 1 ;;
esac
(cd -- "$payload" && sha256sum --check --quiet --strict SHA256SUMS)
expected="$(cat -- "$payload/pgbackrest-version")"
[[ "$expected" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ $("$payload/pgbackrest" version) == "pgBackRest $expected" ]]
if [[ "$check" == true ]]; then
  printf 'pgBackRest %s\n' "$expected"
  exit 0
fi

destination="$1"
install -d -m 0755 -- "$destination"
staged_binary="$(mktemp -- "$destination/.pgbackrest.XXXXXX")"
staged_notices="$(mktemp -- "$destination/.pgbackrest-notices.XXXXXX")"
trap 'rm -f -- "$staged_binary" "$staged_notices"' EXIT
install -m 0755 -- "$payload/pgbackrest" "$staged_binary"
install -m 0644 -- "$payload/licenses.tar.gz" "$staged_notices"
mv -fT -- "$staged_notices" "$destination/pgbackrest-licenses.tar.gz"
mv -fT -- "$staged_binary" "$destination/pgbackrest"
printf 'pgBackRest %s installed in %s\n' "$expected" "$destination"
