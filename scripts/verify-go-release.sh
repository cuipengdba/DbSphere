#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 3 ]]; then
  echo 'Usage: verify-go-release.sh TRUSTED_PUBLIC_KEY RELEASE_DIRECTORY INSTALLER_NAME' >&2
  exit 2
fi

key="$(realpath -- "$1")"
directory="$(realpath -- "$2")"
name="$3"
if [[ ! "$name" =~ ^DbSphere-go-(offline|observability|systemd)-[0-9]+\.[0-9]+\.[0-9]+-linux-(amd64|arm64)\.run$ ]]; then
  echo 'Invalid release installer filename.' >&2
  exit 2
fi

cd -- "$directory"
openssl pkeyutl -verify -pubin -inkey "$key" -rawin -in SHA256SUMS -sigfile SHA256SUMS.sig >/dev/null
digest="$(awk -v name="$name" 'NF==2 && $2==name {print $1}' SHA256SUMS)"
if [[ ! "$digest" =~ ^[0-9a-f]{64}$ ]]; then
  echo 'Installer is absent from the signed release manifest.' >&2
  exit 1
fi
printf '%s  %s\n' "$digest" "$name" | sha256sum --check --strict
