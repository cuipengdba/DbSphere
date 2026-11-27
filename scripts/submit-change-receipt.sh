#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 4 || -z ${DBSPHERE_API_ORIGIN:-} || -z ${DBSPHERE_OPS_TOKEN:-} ]]; then
  printf '%s\n' 'Usage: submit-change-receipt.sh CHANGE_ID EXTERNAL_ID RECEIPT_ID STATE' \
    'Set DBSPHERE_API_ORIGIN to the HTTPS console origin and provide DBSPHERE_OPS_TOKEN.' >&2
  exit 2
fi
if [[ ! $DBSPHERE_API_ORIGIN =~ ^https://[[:alnum:].-]+(:[0-9]+)?$ || ! $DBSPHERE_OPS_TOKEN =~ ^[[:graph:]]{24,256}$ ]]; then
  printf '%s\n' 'Invalid HTTPS origin or operations token.' >&2
  exit 2
fi

payload=$(jq -cn --arg changeId "$1" --arg externalId "$2" --arg receiptId "$3" --arg state "$4" \
  '{changeId:$changeId,externalId:$externalId,receiptId:$receiptId,state:$state}')
# Supply the token via a pipe so it does not appear in curl's argument list.
curl --silent --show-error --fail-with-body --connect-timeout 5 --max-time 20 \
  --header @<(printf 'Authorization: Bearer %s\n' "$DBSPHERE_OPS_TOKEN") \
  --header 'Content-Type: application/json' --data-binary "$payload" \
  "$DBSPHERE_API_ORIGIN/api/v1/change-receipts"
