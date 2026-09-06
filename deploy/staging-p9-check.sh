#!/usr/bin/env bash
set -euo pipefail

# Non-destructive P9 preflight. It validates that a named staging target has
# stable endpoints, backup/restore metadata, and an owner before any cutover.
# It never connects to, promotes, fences, copies, or deletes data.

required=(
  P9_ENVIRONMENT P9_CHANGE_WINDOW P9_ROLLBACK_OWNER
  POSTGRES_PRIMARY_DSN POSTGRES_REPLICA_DSNS WAL_ARCHIVE_TARGET BACKUP_BUCKET
  REDIS_BROKER_ENDPOINT REDIS_COORDINATION_ENDPOINT REDIS_CACHE_ENDPOINT
  MINIO_SOURCE_ENDPOINT MINIO_TARGET_ENDPOINT MINIO_CHECKSUM_MANIFEST
  SEARCH_ES_ENDPOINT SEARCH_QDRANT_ENDPOINT KAFKA_BROKERS
  GATEWAY_PUBLIC_ENDPOINT TLS_CERTIFICATE_REF
  ACCOUNT_DB_DSN CONTENT_DB_DSN STORAGE_DB_DSN
)

missing=0
for key in "${required[@]}"; do
  if [[ -z "${!key:-}" ]]; then
    echo "missing P9 staging variable: $key" >&2
    missing=$((missing + 1))
  fi
done
if ((missing > 0)); then
  echo "P9 staging gate: blocked until dedicated endpoints, backup target, and rollback owner are configured" >&2
  exit 2
fi

if [[ "${P9_ENVIRONMENT}" == "production" ]]; then
  echo "P9 staging gate: P9_ENVIRONMENT must identify a non-production rehearsal target" >&2
  exit 2
fi

for endpoint in POSTGRES_PRIMARY_DSN REDIS_BROKER_ENDPOINT REDIS_COORDINATION_ENDPOINT REDIS_CACHE_ENDPOINT MINIO_SOURCE_ENDPOINT MINIO_TARGET_ENDPOINT SEARCH_ES_ENDPOINT SEARCH_QDRANT_ENDPOINT GATEWAY_PUBLIC_ENDPOINT; do
  value=${!endpoint}
  if [[ "$value" != *:* ]]; then
    echo "invalid P9 endpoint (expected host:port or URL): $endpoint" >&2
    exit 2
  fi
done

echo "P9 staging gate: prerequisites present for ${P9_ENVIRONMENT}"
echo "next: backup/restore -> checksum inventory -> failover/fencing -> canary -> rollback rehearsal"
