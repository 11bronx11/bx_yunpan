#!/usr/bin/env bash
set -euo pipefail

required=(POSTGRES_PRIMARY_DSN SEARCH_INDEX_EPOCH SEARCH_ES_ENDPOINT SEARCH_QDRANT_ENDPOINT)
missing=0
for key in "${required[@]}"; do
  if [[ -z "${!key:-}" ]]; then
    echo "missing staging search variable: $key" >&2
    missing=$((missing + 1))
  fi
done
if ((missing > 0)); then
  echo "staging search gate: blocked until ES/Qdrant and writer endpoints are configured" >&2
  exit 2
fi

echo "staging search gate: prerequisites present"
echo "next: snapshot -> watermark drain -> checksum -> shadow -> flag switch -> fallback -> switch-back"
