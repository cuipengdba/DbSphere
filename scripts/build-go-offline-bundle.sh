#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -ne 1 || "$1" != /* ]]; then
  echo 'Usage: bash scripts/build-go-offline-bundle.sh /absolute/output/directory' >&2
  exit 1
fi
case "$(uname -m)" in
  x86_64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) arch='' ;;
esac
arch="${DBSPHERE_TARGET_ARCH:-$arch}"
if [[ "$(uname -s)" != Linux || ! "$arch" =~ ^(amd64|arm64)$ ]] ||
   ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  echo 'The builder needs Linux, Docker Engine, Compose v2, and target architecture amd64 or arm64.' >&2
  exit 1
fi
version="$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$repo_dir/package.json" | head -n 1)"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-z0-9.]+)?$ ]]; then
  echo 'A valid package version is required.' >&2
  exit 1
fi
output_dir="$(realpath -m -- "$1")"
mkdir -p -- "$output_dir"
stage="$(mktemp -d "$output_dir/.dbsphere-go-offline.XXXXXX")"
payload="$(mktemp "$output_dir/.dbsphere-go-payload.XXXXXX")"
cleanup() {
  local resolved payload_resolved
  resolved="$(realpath -m -- "$stage")"
  payload_resolved="$(realpath -m -- "$payload")"
  if [[ "$resolved" == "$output_dir"/.dbsphere-go-offline.* && -d "$resolved" ]]; then rm -rf -- "$resolved"; fi
  if [[ "$payload_resolved" == "$output_dir"/.dbsphere-go-payload.* && -f "$payload_resolved" ]]; then rm -f -- "$payload_resolved"; fi
}
trap cleanup EXIT

app_image="dbsphere/offline-go-app:$version"
pg_source='postgres:18.7-bookworm@sha256:ece55f7868da684e400f82f1030a050718adbdd65c23aa500c486f6e2950dc0f'
if ! grep -Fq "DBSPHERE_PG18_IMAGE:-$pg_source" "$repo_dir/deploy/compose/compose.go.yaml"; then
  echo 'Go Compose PostgreSQL image must match the pinned offline source.' >&2
  exit 1
fi
if [[ -n "${DBSPHERE_RELEASE_BINARY:-}" ]]; then
  binary="$(realpath -- "$DBSPHERE_RELEASE_BINARY")"
  if [[ "$(basename -- "$binary")" != "DbSphere-go-$version-linux-$arch" ]]; then
    echo 'The release binary filename must match the package version and architecture.' >&2
    exit 1
  fi
  go version -m "$binary" | grep -Eq "^[[:space:]]*build[[:space:]]+GOARCH=$arch$"
  go version -m "$binary" | grep -Eq '^[[:space:]]*build[[:space:]]+GOOS=linux$'
  root="$stage/rootfs"
  install -d "$root/etc/ssl/certs" "$root/licenses" "$root/var/lib/dbsphere-discovery"
  install -m 0755 -- "$binary" "$root/dbsphere"
  install -m 0644 -- "${DBSPHERE_CA_CERTIFICATES:-/etc/ssl/certs/ca-certificates.crt}" "$root/etc/ssl/certs/ca-certificates.crt"
  cp -- "$repo_dir/LICENSE" "$root/licenses/DBSphere-LICENSE"
  cp -R -- "$repo_dir/third_party/licenses/go" "$root/licenses/go"
  printf '[]\n' > "$root/var/lib/dbsphere-discovery/targets.json"
  docker build --network none --platform "linux/$arch" -t "$app_image" -f - "$root" <<'DOCKERFILE'
FROM scratch
COPY dbsphere /dbsphere
COPY etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY licenses/ /licenses/
COPY --chown=65532:65532 var/lib/dbsphere-discovery/ /var/lib/dbsphere-discovery/
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --retries=12 CMD ["/dbsphere", "healthcheck"]
ENTRYPOINT ["/dbsphere"]
DOCKERFILE
  rm -rf -- "$root"
else
  docker build --platform "linux/$arch" --network "${DBSPHERE_BUILD_NETWORK:-default}" \
    --build-arg TARGETOS=linux --build-arg "TARGETARCH=$arch" --build-arg http_proxy --build-arg https_proxy --build-arg no_proxy \
    -f "$repo_dir/Dockerfile-go" -t "$app_image" "$repo_dir"
fi
# Resolve the pinned index to a platform manifest. Classic Docker image stores
# cannot retain two platform images under the same index digest.
pg_manifest="$(docker buildx imagetools inspect --raw "$pg_source" | jq -er --arg arch "$arch" '
  [.manifests[] | select(.platform.os == "linux" and .platform.architecture == $arch) | .digest]
  | select(length == 1) | .[0]')"
pg_platform="postgres@$pg_manifest"
docker pull --platform "linux/$arch" "$pg_platform"
docker image tag "$pg_platform" postgres:18-bookworm
mkdir -p -- "$stage/deploy/compose"
mkdir -p -- "$stage/deploy/k8s"
cp -- "$repo_dir/deploy/k8s/dbsphere-container-agent.yaml" "$stage/deploy/k8s/"
mkdir -p -- "$stage/licenses"
cp -- "$repo_dir/LICENSE" "$stage/licenses/DBSphere-LICENSE"
cp -R -- "$repo_dir/third_party/licenses/go" "$stage/licenses/go"
cp -- "$repo_dir/deploy/compose/compose.go.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/compose/compose.go.external.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/compose/compose.go.k8s.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/compose/compose.go.openbao.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/compose/compose.go.site.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/compose/compose.backup.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/compose/compose.go.offline.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/compose/compose.go.ha.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/deploy/compose/compose.go.replica-backup.yaml" "$stage/deploy/compose/"
cp -- "$repo_dir/scripts/install-go-offline.sh" "$stage/install.sh"
cp -- "$repo_dir/scripts/backup-go-store.sh" "$stage/backup.sh"
chmod 0755 "$stage/install.sh"
chmod 0755 "$stage/backup.sh"
docker image save --platform "linux/$arch" "$app_image" postgres:18-bookworm | gzip -n > "$stage/images.tar.gz"
# Docker may change an image's reported ID when a multi-platform tag is
# exported as one platform. Reload the exact archive before recording IDs.
docker image load --input "$stage/images.tar.gz" >/dev/null
app_image_id="$(docker image inspect --platform "linux/$arch" --format '{{.Id}}' "$app_image")"
pg_image_id="$(docker image inspect --platform "linux/$arch" --format '{{.Id}}' postgres:18-bookworm)"
if [[ ! "$app_image_id" =~ ^sha256:[0-9a-f]{64}$ || ! "$pg_image_id" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo 'Cannot identify both Linux images in the offline archive.' >&2
  exit 1
fi
printf 'DBSPHERE_GO_OFFLINE_VERSION=%s\nDBSPHERE_GO_OFFLINE_APP_IMAGE=%s\nDBSPHERE_GO_OFFLINE_APP_IMAGE_ID=%s\nDBSPHERE_GO_OFFLINE_PG_IMAGE_ID=%s\nDBSPHERE_GO_OFFLINE_PG_SOURCE=%s\nDBSPHERE_GO_OFFLINE_ARCH=%s\n' \
  "$version" "$app_image" "$app_image_id" "$pg_image_id" "$pg_source" "$arch" > "$stage/bundle.env"
bash "$repo_dir/scripts/stage-pgbackrest-bundle.sh" "$stage" "$arch"
(cd -- "$stage" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
archive="$output_dir/DbSphere-go-offline-$version-linux-$arch.run"
tar -C "$stage" -czf "$payload" .
bash "$repo_dir/scripts/render-offline-header.sh" "$repo_dir/scripts/offline-self-extract.sh" > "$archive"
cat "$payload" >> "$archive"
chmod 0755 "$archive"
(cd -- "$output_dir" && sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256")
echo "Go development offline bundle: $archive"
