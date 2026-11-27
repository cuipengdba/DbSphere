#!/usr/bin/env bash
set -euo pipefail

# Preview builders can omit the tool. The official release supplies both inputs.
if [[ -z "${DBSPHERE_PGBACKREST_PACKAGES:-}" ]]; then exit 0; fi
stage="$1"
arch="$2"
case "$arch" in amd64|arm64) ;; *) exit 1 ;; esac
name="DbSphere-pgbackrest-2.60.1-linux-$arch.run"
packages="$(realpath -- "$DBSPHERE_PGBACKREST_PACKAGES")"
read -r digest member < "$packages/$name.sha256"
[[ "$digest" =~ ^[0-9a-f]{64}$ && "$member" == "$name" ]]
(cd -- "$packages" && sha256sum --check --strict --quiet "$name.sha256")
install -m 0755 -- "$packages/$name" "$stage/pgbackrest.run"
(cd -- "$stage" && sha256sum pgbackrest.run > pgbackrest.run.sha256)
