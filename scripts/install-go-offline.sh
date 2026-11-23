#!/usr/bin/env bash
set -euo pipefail

rotate_admin=false
disable_observability=false
if [[ $# -eq 1 && "$1" == --rotate-admin-password ]]; then
  rotate_admin=true
elif [[ $# -eq 1 && "$1" == --disable-observability ]]; then
  disable_observability=true
elif [[ $# -ne 0 && ! ( $# -eq 2 && "$1" == --backup && "$2" == /* ) ]]; then
  echo 'Usage: bash install.sh [--backup /absolute/output/directory | --rotate-admin-password | --disable-observability]' >&2
  exit 1
fi
bundle_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) arch='' ;;
esac
if [[ "$(uname -s)" != Linux || -z "$arch" ]] ||
   ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  echo 'Linux x86_64 or arm64, Docker Engine and Compose v2 are required.' >&2
  exit 1
fi
if [[ ! -f "$bundle_dir/SHA256SUMS" || ! -f "$bundle_dir/bundle.env" ||
      ! -f "$bundle_dir/images.tar.gz" ]]; then
  echo 'Go offline bundle is incomplete.' >&2
  exit 1
fi
(cd -- "$bundle_dir" && sha256sum --check --quiet SHA256SUMS) || {
  echo 'Go offline bundle checksum verification failed.' >&2
  exit 1
}
unset DBSPHERE_OFFLINE_PROM_IMAGE DBSPHERE_OFFLINE_GRAFANA_IMAGE
source "$bundle_dir/bundle.env"
if [[ "${DBSPHERE_GO_OFFLINE_ARCH:-amd64}" != "$arch" ]]; then
  echo "Offline bundle targets ${DBSPHERE_GO_OFFLINE_ARCH:-amd64}; this Linux host is $arch." >&2
  exit 1
fi
if [[ ! "${DBSPHERE_GO_OFFLINE_VERSION:-}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?$ ||
  "${DBSPHERE_GO_OFFLINE_APP_IMAGE:-}" != "dbsphere/offline-go-app:$DBSPHERE_GO_OFFLINE_VERSION" ||
  ! "${DBSPHERE_GO_OFFLINE_APP_IMAGE_ID:-}" =~ ^sha256:[0-9a-f]{64}$ ||
  ! "${DBSPHERE_GO_OFFLINE_PG_IMAGE_ID:-}" =~ ^sha256:[0-9a-f]{64}$ ||
  ! "${DBSPHERE_GO_OFFLINE_PG_SOURCE:-}" =~ ^postgres:18\.[0-9]+-bookworm@sha256:[0-9a-f]{64}$ ]]; then
  echo 'Go offline bundle manifest is invalid.' >&2
  exit 1
fi
if [[ $# -eq 2 ]]; then
  bash "$bundle_dir/backup.sh" "$2"
  exit 0
fi
if [[ "$disable_observability" == true && -z "${DBSPHERE_OFFLINE_PROM_IMAGE:-}" ]]; then
  echo 'Use the enhanced offline bundle to disable the installed monitoring components.' >&2
  exit 1
fi
export DBSPHERE_GO_OFFLINE_APP_IMAGE
export DBSPHERE_PG18_IMAGE=postgres:18-bookworm
export COMPOSE_PROFILES=

config_dir="${DBSPHERE_GO_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/dbsphere-go}"
if [[ "$config_dir" != /* ]]; then echo 'DBSPHERE_GO_CONFIG_DIR must be absolute.' >&2; exit 1; fi
mkdir -p -- "$config_dir"
chmod 0700 "$config_dir"
env_file="$config_dir/dbsphere-go.env"
if [[ "$rotate_admin" == true && ! -f "$env_file" ]]; then
  echo 'Administrator password rotation requires an existing Go installation.' >&2
  exit 1
fi
created_config=false
if [[ ! -e "$env_file" ]]; then
  umask 077
  created_config=true
  store_password="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  master_key="$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
  printf 'DBSPHERE_ADMIN_PASSWORD=%s\nDBSPHERE_STORE_PASSWORD=%s\nDBSPHERE_MASTER_KEY=%s\nDBSPHERE_WEB_PORT=8081\nDBSPHERE_COOKIE_SECURE=false\n' \
    "${DBSPHERE_ADMIN_PASSWORD:-}" "$store_password" "$master_key" > "$env_file"
  if [[ "${DBSPHERE_GO_HA:-false}" == true ]]; then printf 'DBSPHERE_GO_HA=true\n' >> "$env_file"; fi
  unset store_password master_key
fi
chmod 0600 "$env_file"
base="$bundle_dir/deploy/compose/compose.go.yaml"
if grep -q '^DBSPHERE_DATABASE_URL=' "$env_file"; then
  base="$bundle_dir/deploy/compose/compose.go.external.yaml"
fi
compose_args=(-f "$base")
if grep -q '^DBSPHERE_KUBECONFIG_HOST_PATH=' "$env_file"; then
  kubeconfig_path="$(sed -n 's/^DBSPHERE_KUBECONFIG_HOST_PATH=//p' "$env_file" | tail -n 1)"
  if [[ "$kubeconfig_path" != /* || ! -f "$kubeconfig_path" ]]; then
    echo 'DBSPHERE_KUBECONFIG_HOST_PATH must be an existing absolute file path.' >&2
    exit 1
  fi
  compose_args+=(-f "$bundle_dir/deploy/compose/compose.go.k8s.yaml")
fi
if grep -q '^DBSPHERE_BACKUP_INFO_HOST_PATH=' "$env_file"; then
  backup_info_path="$(sed -n 's/^DBSPHERE_BACKUP_INFO_HOST_PATH=//p' "$env_file" | tail -n 1)"
  if [[ "$backup_info_path" != /* || ! -d "$backup_info_path" ]]; then
    echo 'DBSPHERE_BACKUP_INFO_HOST_PATH must be an existing absolute directory.' >&2
    exit 1
  fi
  compose_args+=(-f "$bundle_dir/deploy/compose/compose.backup.yaml")
fi
compose_args+=(-f "$bundle_dir/deploy/compose/compose.go.offline.yaml")
ha_mode="${DBSPHERE_GO_HA:-$(sed -n 's/^DBSPHERE_GO_HA=//p' "$env_file" | tail -n 1)}"
restart_apps=(app)
case "$ha_mode" in
  true)
    compose_args+=(-f "$bundle_dir/deploy/compose/compose.go.ha.yaml" --profile ha)
    if [[ -n "${backup_info_path:-}" ]]; then
      compose_args+=(-f "$bundle_dir/deploy/compose/compose.go.replica-backup.yaml")
    fi
    restart_apps+=(replica)
    ;;
  ''|false) ;;
  *) echo 'DBSPHERE_GO_HA must be true or false.' >&2; exit 1 ;;
esac
loaded_images="$(docker image load --input "$bundle_dir/images.tar.gz")"
if ! grep -Fqx "Loaded image: $DBSPHERE_GO_OFFLINE_APP_IMAGE" <<< "$loaded_images" ||
   ! grep -Fqx 'Loaded image: postgres:18-bookworm' <<< "$loaded_images"; then
  echo 'The verified offline archive did not load both expected image tags.' >&2
  exit 1
fi
docker image inspect "$DBSPHERE_GO_OFFLINE_APP_IMAGE" postgres:18-bookworm >/dev/null
if [[ "$(docker image inspect --format '{{.Id}}' "$DBSPHERE_GO_OFFLINE_APP_IMAGE")" != "$DBSPHERE_GO_OFFLINE_APP_IMAGE_ID" ||
      "$(docker image inspect --format '{{.Id}}' postgres:18-bookworm)" != "$DBSPHERE_GO_OFFLINE_PG_IMAGE_ID" ]]; then
  # Docker Engine versions can normalize image manifests on load, changing
  # local image IDs. The verified archive and the two load results remain the
  # source of identity for this installation.
  echo 'Docker normalized image IDs; verified archive and loaded tags match the bundle.' >&2
fi
if [[ -n "${DBSPHERE_OFFLINE_PROM_IMAGE:-}" ]]; then
  if [[ ! "$DBSPHERE_OFFLINE_PROM_IMAGE" =~ ^dbsphere/offline-prometheus:[0-9]+\.[0-9]+\.[0-9]+$ ||
        ! -f "$bundle_dir/observability-images.tar.gz" ]]; then
    echo 'Observability bundle manifest is invalid.' >&2
    exit 1
  fi
  export DBSPHERE_OFFLINE_PROM_IMAGE
  export DBSPHERE_OBSERVABILITY_DIR="$config_dir/observability"
  install -d -m 0700 -- "$DBSPHERE_OBSERVABILITY_DIR"
  install -m 0644 -- "$bundle_dir/observability/"prometheus*.yml "$DBSPHERE_OBSERVABILITY_DIR/"
  install -m 0644 -- "$bundle_dir/observability/COMPONENTS.txt" "$DBSPHERE_OBSERVABILITY_DIR/"
  cp -R -- "$bundle_dir/licenses/prometheus" "$DBSPHERE_OBSERVABILITY_DIR/"
  observability_args=("${compose_args[@]}" -f "$bundle_dir/deploy/compose/compose.go.observability.yaml")
  observability_services=(prometheus)
  if [[ -n "${DBSPHERE_OFFLINE_GRAFANA_IMAGE:-}" ]]; then
    if [[ ! "$DBSPHERE_OFFLINE_GRAFANA_IMAGE" =~ ^dbsphere/offline-grafana:[0-9]+\.[0-9]+\.[0-9]+$ ||
          ! -f "$bundle_dir/deploy/compose/compose.go.grafana.yaml" ]]; then
      echo 'Grafana offline bundle manifest is invalid.' >&2
      exit 1
    fi
    export DBSPHERE_OFFLINE_GRAFANA_IMAGE
    install -d -m 0755 -- "$DBSPHERE_OBSERVABILITY_DIR/grafana"
    cp -R -- "$bundle_dir/observability/grafana/"{provisioning,panels} "$DBSPHERE_OBSERVABILITY_DIR/grafana/"
    find "$DBSPHERE_OBSERVABILITY_DIR/grafana/"{provisioning,panels} -type d -exec chmod 0755 {} +
    find "$DBSPHERE_OBSERVABILITY_DIR/grafana/"{provisioning,panels} -type f -exec chmod 0644 {} +
    cp -R -- "$bundle_dir/licenses/grafana/" "$DBSPHERE_OBSERVABILITY_DIR/"
    grafana_password="$DBSPHERE_OBSERVABILITY_DIR/grafana/admin-password"
    if [[ ! -s "$grafana_password" ]]; then
      head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$grafana_password"
      chmod 0644 "$grafana_password"
    fi
    observability_args+=(-f "$bundle_dir/deploy/compose/compose.go.grafana.yaml")
    observability_services+=(grafana)
  fi
  if [[ "$disable_observability" == true ]]; then
    docker compose --env-file "$env_file" "${observability_args[@]}" stop "${observability_services[@]}"
  else
    metrics_token="${DBSPHERE_METRICS_TOKEN:-$(sed -n 's/^DBSPHERE_METRICS_TOKEN=//p' "$env_file" | tail -n 1)}"
    if [[ -z "$metrics_token" && -s "$DBSPHERE_OBSERVABILITY_DIR/metrics-token" ]]; then
      metrics_token="$(cat -- "$DBSPHERE_OBSERVABILITY_DIR/metrics-token")"
    fi
    if [[ -z "$metrics_token" ]]; then
      metrics_token="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    fi
    printf '%s\n' "$metrics_token" > "$DBSPHERE_OBSERVABILITY_DIR/metrics-token"
    chmod 0644 "$DBSPHERE_OBSERVABILITY_DIR/metrics-token"
    export DBSPHERE_METRICS_TOKEN="$metrics_token"
    unset metrics_token
    loaded_observability="$(docker image load --input "$bundle_dir/observability-images.tar.gz")"
    grep -Fqx "Loaded image: $DBSPHERE_OFFLINE_PROM_IMAGE" <<< "$loaded_observability" || {
      echo 'The verified observability archive did not load its expected Prometheus image.' >&2
      exit 1
    }
    if [[ -n "${DBSPHERE_OFFLINE_GRAFANA_IMAGE:-}" ]] &&
       ! grep -Fqx "Loaded image: $DBSPHERE_OFFLINE_GRAFANA_IMAGE" <<< "$loaded_observability"; then
      echo 'The verified observability archive did not load its expected Grafana image.' >&2
      exit 1
    fi
    compose_args=("${observability_args[@]}")
  fi
fi
docker compose --env-file "$env_file" "${compose_args[@]}" config -q
if [[ "$ha_mode" != true ]]; then
  docker compose --env-file "$env_file" "${compose_args[@]}" --profile ha stop gateway replica
fi
if [[ "$rotate_admin" == true ]]; then
  docker compose --env-file "$env_file" "${compose_args[@]}" \
    run --rm --no-deps --pull never app rotate-admin-password
  docker compose --env-file "$env_file" "${compose_args[@]}" \
    up -d --no-build --pull never --no-deps --force-recreate "${restart_apps[@]}"
fi
docker compose --env-file "$env_file" "${compose_args[@]}" \
  up -d --no-build --pull never --wait --wait-timeout 180
if [[ -f "$bundle_dir/deploy/k8s/dbsphere-container-agent.yaml" ]]; then
  install -d -m 0755 -- "$config_dir/kubernetes"
  install -m 0644 -- "$bundle_dir/deploy/k8s/dbsphere-container-agent.yaml" "$config_dir/kubernetes/"
fi
web_port="$(sed -n 's/^DBSPHERE_WEB_PORT=//p' "$env_file" | tail -n 1)"
echo "DbSphere Go is ready at http://127.0.0.1:${web_port:-8081}"
if [[ "$created_config" == true && -z "${DBSPHERE_ADMIN_PASSWORD:-}" ]]; then
  echo 'Open the loopback address to set the admin password on first use.'
fi
