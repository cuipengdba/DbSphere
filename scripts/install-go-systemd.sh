#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo 'Usage: bash DbSphere-go-systemd-*.run --sample-config | --install-pgbackrest /absolute/install-directory, or sudo bash DbSphere-go-systemd-*.run [--check] [/absolute/dbsphere.env]' >&2
  exit 1
}
payload="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
(cd -- "$payload" && sha256sum --check --strict --quiet SHA256SUMS)
if [[ $# -eq 2 && "$1" == --install-pgbackrest ]]; then
  exec bash "$payload/pgbackrest.run" "$2"
fi
if [[ $# -eq 1 && "$1" == --sample-config ]]; then
  cat -- "$payload/dbsphere.env.example"
  exit 0
fi
check_only=false
if [[ "${1:-}" == --check ]]; then check_only=true; shift; fi
if [[ $# -gt 1 || ( $# -eq 1 && "$1" != /* ) ]]; then usage; fi
if [[ $EUID -ne 0 || ! -d /run/systemd/system ]]; then
  echo 'Run as root on a Linux host with systemd as the system manager.' >&2
  exit 1
fi

case "$(uname -m):$(cat -- "$payload/architecture")" in
  x86_64:amd64|aarch64:arm64|arm64:arm64) ;;
  *) echo 'The native package architecture does not match this host.' >&2; exit 1 ;;
esac
if [[ -f "$payload/pgbackrest.run" ]]; then bash "$payload/pgbackrest.run" --check "$payload"; fi

unit_name=dbsphere-native.service
unit="/etc/systemd/system/$unit_name"
config_dir=/etc/dbsphere-native
bin_dir=/opt/dbsphere-native
config="$config_dir/dbsphere.env"
binary="$bin_dir/dbsphere"
source_config="${1:-$config}"
if [[ ! -f "$source_config" ]]; then
  echo 'Supply an absolute environment file for an existing PostgreSQL 18 store.' >&2
  exit 1
fi

umask 077
exec 9>/run/lock/dbsphere-native.install.lock
if ! flock -n 9; then echo 'Another DbSphere native installation is running.' >&2; exit 1; fi
if [[ "$check_only" == false && -f "$config" ]] && ! cmp -s -- "$source_config" "$config"; then
  echo "The managed environment already exists at $config. Preserve its master key and verify intentional edits with --check." >&2
  exit 1
fi
install -d -m 0700 -- "$config_dir"
install -d -m 0755 -- "$bin_dir"
app_stage="$(mktemp -d "$bin_dir/.stage.XXXXXX")"
config_stage="$(mktemp -d "$config_dir/.stage.XXXXXX")"
unit_stage="$(mktemp -d /etc/systemd/system/.dbsphere.XXXXXX)"
cleanup() {
  rm -f -- "$app_stage/dbsphere" "$app_stage/previous" "$config_stage/dbsphere.env" "$unit_stage/unit" "$unit_stage/previous"
  rmdir -- "$app_stage" "$config_stage" "$unit_stage"
}
trap cleanup EXIT
chmod 0755 -- "$app_stage"
install -m 0755 -- "$payload/dbsphere" "$app_stage/dbsphere"
install -m 0600 -- "$source_config" "$config_stage/dbsphere.env"

systemd-run --quiet --wait --pipe --collect --property=DynamicUser=yes \
  "--property=EnvironmentFile=$config_stage/dbsphere.env" -- "$app_stage/dbsphere" check-store
if [[ "$check_only" == true ]]; then exit 0; fi

install -m 0644 -- "$payload/dbsphere-native.service" "$unit_stage/unit"
if [[ -f "$binary" ]]; then cp -p -- "$binary" "$app_stage/previous"; fi
if [[ -f "$unit" ]]; then cp -p -- "$unit" "$unit_stage/previous"; fi
had_config=false
was_active=false
was_enabled=false
if [[ -f "$config" ]]; then had_config=true; fi
if systemctl is-active --quiet "$unit_name"; then was_active=true; fi
if systemctl is-enabled --quiet "$unit_name"; then was_enabled=true; fi

activate() {
  if [[ "$had_config" == false ]]; then mv -f -- "$config_stage/dbsphere.env" "$config" || return; fi
  mv -f -- "$app_stage/dbsphere" "$binary" &&
    mv -f -- "$unit_stage/unit" "$unit" &&
    systemctl daemon-reload &&
    systemctl restart "$unit_name" &&
    systemctl is-active --quiet "$unit_name" &&
    systemctl enable "$unit_name"
}

restore_file() {
  if [[ -f "$1" ]]; then mv -f -- "$1" "$2"; else rm -f -- "$2"; fi
}
if ! activate; then
  echo 'Native startup failed; restoring the previous installation files. See journalctl -u dbsphere-native.service.' >&2
  systemctl stop "$unit_name" || true
  if [[ "$was_enabled" == false ]]; then systemctl disable "$unit_name" || true; fi
  restore_file "$app_stage/previous" "$binary"
  restore_file "$unit_stage/previous" "$unit"
  if [[ "$had_config" == false ]]; then rm -f -- "$config"; fi
  systemctl daemon-reload
  if [[ "$was_active" == true ]]; then systemctl start "$unit_name"; fi
  exit 1
fi
if [[ -f "$payload/pgbackrest.run" ]]; then bash "$payload/pgbackrest.run" "$bin_dir/tools/pgbackrest"; fi
echo "DbSphere native service is active and enabled. Configuration: $config"
