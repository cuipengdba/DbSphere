#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -ne 2 || "$1" != /* || "$2" != /* ]]; then
  echo 'Usage: bash scripts/build-go-observability-bundle.sh /absolute/standard.run /absolute/output/directory' >&2
  exit 1
fi
archive="$(realpath -- "$1")"
out="$(realpath -m -- "$2")"
mkdir -p -- "$out"
stage="$(mktemp -d "$out/.dbsphere-observability.XXXXXX")"
cleanup() {
  if [[ "$(realpath -m -- "$stage")" == "$out"/.dbsphere-observability.* ]]; then rm -rf -- "$stage"; fi
}
trap cleanup EXIT

(cd -- "$(dirname -- "$archive")" && sha256sum --check --strict "$(basename -- "$archive").sha256")
payload="$(awk '/^__DBSPHERE_PAYLOAD__$/ { print NR + 1; exit }' "$archive")"
test -n "$payload"
tail -n +"$payload" "$archive" | tar -xz -C "$stage"
(cd -- "$stage" && sha256sum --check --quiet SHA256SUMS)
source "$stage/bundle.env"
version="$DBSPHERE_GO_OFFLINE_VERSION"
arch="$DBSPHERE_GO_OFFLINE_ARCH"
case "$(uname -m):$arch" in x86_64:amd64|aarch64:arm64|arm64:arm64) ;; *) exit 1 ;; esac
test "$(basename -- "$archive")" = "DbSphere-go-offline-$version-linux-$arch.run"

prom_version=3.13.5
prom_source="prom/prometheus:v$prom_version@sha256:aafa9d694e6a883c02db516d9f6e5b84e14195f49a2a6456a4cca815edda72b9"
prom_image="dbsphere/offline-prometheus:$prom_version"
docker pull --platform "linux/$arch" "$prom_source"
docker image tag "$prom_source" "$prom_image"
images=("$prom_image")
mkdir -p -- "$stage/observability" "$stage/licenses/prometheus"
cp -- "$repo_dir/deploy/compose/compose.go.observability.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/observability/"*.yml "$stage/observability/"
for notice in LICENSE NOTICE; do
  curl -fsSL --retry 4 "https://raw.githubusercontent.com/prometheus/prometheus/v$prom_version/$notice" \
    -o "$stage/licenses/prometheus/$notice"
done
printf 'Prometheus v%s\nContainer %s\nSource https://github.com/prometheus/prometheus/tree/v%s\nLicense Apache-2.0\n' \
  "$prom_version" "$prom_source" "$prom_version" > "$stage/observability/COMPONENTS.txt"
printf 'DBSPHERE_OFFLINE_PROM_IMAGE=%s\n' "$prom_image" >> "$stage/bundle.env"
if [[ -d "$repo_dir/deploy/observability/grafana" ]]; then
  grafana_version=13.3.1
  grafana_source="grafana/grafana:$grafana_version@sha256:0615ec4fbd4072004b7d86b46ded63013a74b7d6eb1119273dd89e8fa25c13cf"
  grafana_image="dbsphere/offline-grafana:$grafana_version"
  docker pull --platform "linux/$arch" "$grafana_source"
  docker image tag "$grafana_source" "$grafana_image"
  images+=("$grafana_image")
  cp -- "$repo_dir/deploy/compose/compose.go.grafana.yaml" "$stage/deploy/compose/"
  grafana_stage="$stage/observability/grafana"
  mkdir -p -- "$grafana_stage/provisioning" "$stage/licenses/grafana"
  cp -R -- "$repo_dir/deploy/observability/grafana/"{datasources,dashboards} "$grafana_stage/provisioning/"
  cp -R -- "$repo_dir/deploy/observability/grafana/panels" "$grafana_stage/"
  for notice in LICENSE LICENSING.md; do
    curl -fsSL --retry 4 "https://raw.githubusercontent.com/grafana/grafana/v$grafana_version/$notice" \
      -o "$stage/licenses/grafana/$notice"
  done
  source_name="Grafana-OSS-$grafana_version-source.tar.gz"
  printf '\nGrafana OSS v%s\nContainer %s\nLicense AGPL-3.0\nCorresponding release source %s\n' \
    "$grafana_version" "$grafana_source" \
    "https://github.com/cuipengdba/DbSphere/releases/download/v$version/$source_name" >> "$stage/observability/COMPONENTS.txt"
  printf 'DBSPHERE_OFFLINE_GRAFANA_IMAGE=%s\n' "$grafana_image" >> "$stage/bundle.env"
  if [[ "$arch" == amd64 ]]; then
    curl -fsSL --retry 4 "https://codeload.github.com/grafana/grafana/tar.gz/refs/tags/v$grafana_version" \
      -o "$out/$source_name"
    tar -tzf "$out/$source_name" > "$stage/source-list.tmp"
    grep -Fxq "grafana-$grafana_version/LICENSE" "$stage/source-list.tmp"
    rm -- "$stage/source-list.tmp"
    (cd -- "$out" && sha256sum "$source_name" > "$source_name.sha256")
  fi
fi
docker image save --platform "linux/$arch" "${images[@]}" | gzip -n > "$stage/observability-images.tar.gz"
(cd -- "$stage" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
enhanced="$out/DbSphere-go-observability-$version-linux-$arch.run"
cat "$repo_dir/scripts/offline-self-extract.sh" > "$enhanced"
tar -C "$stage" -czf - . >> "$enhanced"
chmod 0755 "$enhanced"
(cd -- "$out" && sha256sum "$(basename -- "$enhanced")" > "$(basename -- "$enhanced").sha256")
echo "DbSphere observability offline bundle: $enhanced"
