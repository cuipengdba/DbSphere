#!/usr/bin/env bash
set -euo pipefail

root="${1:?Expected the downloaded test artifacts directory}"
out="${2:?Expected the release manifest directory}"
version="$(jq -er .version package.json)"
[[ "$version" =~ ^3\.[0-9]+\.0$ ]]
tag="v$version"
test "${GITHUB_REF:?}" = "refs/heads/release/$tag"
draft="$(gh api "repos/${GITHUB_REPOSITORY:?}/releases?per_page=10" |
  jq -er --arg tag "$tag" --arg sha "${GITHUB_SHA:?}" \
    '.[] | select(.draft and .tag_name==$tag and .target_commitish==$sha) | .id')"
mkdir -p -- "$out"

verify_group() {
  local group="$1" name checksum file digest bytes report
  shift
  report="$out/$group.ndjson"
  : > "$report"
  for name in "$@"; do
    file="$(find "$root/$group" -type f -name "$name" -print)"
    test -f "$file"
    checksum="$(awk -v name="$name" 'NF==2 && $2==name { print $1 }' "$file.sha256")"
    [[ "$checksum" =~ ^[a-f0-9]{64}$ ]]
    (cd "$(dirname -- "$file")" && sha256sum --strict --check "$name.sha256")
    for file in "$file" "$file.sha256"; do
      digest="$(sha256sum "$file" | cut -d ' ' -f 1)"
      bytes="$(stat -c %s -- "$file")"
      jq -nc --arg name "$(basename -- "$file")" --arg digest "$digest" --argjson bytes "$bytes" \
        '{name:$name,sha256:$digest,bytes:$bytes}' >> "$report"
      upload+=("$file")
    done
  done
  jq -s . "$report" > "$out/$group.json"
}

upload=()
verify_group 'dbsphere-go-tested-packages' \
  "DbSphere-go-$version-linux-amd64" "DbSphere-go-licenses-$version.tar.gz" \
  "DbSphere-go-offline-$version-linux-amd64.run" "DbSphere-go-systemd-$version-linux-amd64.run"
verify_group 'dbsphere-go-arm64-tested-packages' \
  "DbSphere-go-$version-linux-arm64" "DbSphere-go-offline-$version-linux-arm64.run" \
  "DbSphere-go-systemd-$version-linux-arm64.run"
verify_group 'dbsphere-observability-amd64-tested-packages' \
  "DbSphere-go-observability-$version-linux-amd64.run" 'Grafana-OSS-13.3.1-source.tar.gz'
verify_group 'dbsphere-observability-arm64-tested-packages' "DbSphere-go-observability-$version-linux-arm64.run"
test "${#upload[@]}" -eq 20

gh release upload "$tag" "${upload[@]}" --repo "$GITHUB_REPOSITORY" --clobber
gh api "repos/$GITHUB_REPOSITORY/releases/$draft/assets?per_page=100" |
  jq -e --slurpfile expected <(jq -s add "$out"/*.json) '
    length == 20 and
    all(.[]; . as $asset | any($expected[0][]; .name==$asset.name and .bytes==$asset.size and
      ("sha256:"+.sha256)==$asset.digest and $asset.state=="uploaded"))' >/dev/null
