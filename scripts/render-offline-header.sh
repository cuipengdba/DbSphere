#!/usr/bin/env bash
set -euo pipefail

src="${1:?Pass the offline header template path}"
case "${DBSPHERE_SIGNED_RELEASE:-optional}" in
  required|optional) ;;
  *) echo 'Invalid signature mode.' >&2; exit 2 ;;
esac
sed "s/@DBSPHERE_SIGNED_RELEASE@/${DBSPHERE_SIGNED_RELEASE:-optional}/g" "$src"
