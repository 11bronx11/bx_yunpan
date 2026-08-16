#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"
compose_file="$deploy_dir/compose.yaml"

[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

# shellcheck disable=SC1091
source "$deploy_dir/local-env.sh"

keys=(
  APP_ENV LOG_LEVEL
  HTTP_MAX_BODY_BYTES HTTP_MAX_HEADER_BYTES HTTP_SHUTDOWN_TIMEOUT HTTP_PROBE_TIMEOUT
  HTTP_READ_HEADER_TIMEOUT HTTP_IDLE_TIMEOUT
  POSTGRES_MAX_OPEN_CONNS POSTGRES_MAX_IDLE_CONNS POSTGRES_CONN_MAX_LIFETIME POSTGRES_CONN_MAX_IDLE_TIME
  REDIS_PASSWORD REDIS_DB
  S3_PUBLIC_ENDPOINT S3_PUBLIC_PATH_PREFIX S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET S3_REGION
  S3_SECURE S3_PUBLIC_SECURE S3_READ_URL_TTL
  AUTH_SIGNING_SEED AUTH_COOKIE_SECURE USER_DEFAULT_QUOTA_BYTES AUTH_ISSUER
  AUTH_ACCESS_TTL AUTH_REFRESH_TTL AUTH_COOKIE_DOMAIN SHARE_SECRET SHARE_ACCESS_TTL
  UPLOAD_SESSION_TTL UPLOAD_PART_URL_TTL UPLOAD_CLEANUP_INTERVAL UPLOAD_CLEANUP_BATCH
  OUTBOX_POLL_INTERVAL OUTBOX_BATCH_SIZE OUTBOX_TASK_MAX_RETRY OUTBOX_TASK_TIMEOUT OBJECT_GC_DELAY
  WORKER_CONCURRENCY WORKER_QUEUE_AI WORKER_QUEUE_MEDIA WORKER_QUEUE_OBJECT WORKER_QUEUE_MAINTENANCE
  AI_PROVIDER DASHSCOPE_API_KEY AI_BASE_URL AI_CHAT_MODEL AI_EMBEDDING_MODEL AI_VISION_MODEL
  AI_EMBEDDING_DIMENSION AI_MAX_OBJECT_MIB AI_REQUEST_TIMEOUT AI_RATE_LIMIT_ENABLED
  AI_RATE_LIMIT_SEARCH_PER_MINUTE AI_RATE_LIMIT_ASK_PER_MINUTE AI_RATE_LIMIT_REPROCESS_PER_MINUTE
)

compose=(docker compose --env-file "$env_file" -f "$compose_file" --profile app)
mismatches=0

for service in api worker; do
  container_id=$("${compose[@]}" ps -q "$service")
  if [[ -z "$container_id" ]]; then
    echo "$service: container is not running" >&2
    mismatches=$((mismatches + 1))
    continue
  fi
  container_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container_id")
  for key in "${keys[@]}"; do
    expected=${!key-}
    actual=$(awk -F= -v key="$key" '$1 == key {sub(/^[^=]*=/, ""); print; exit}' <<<"$container_env")
    if [[ "$actual" != "$expected" ]]; then
      echo "$service: $key differs from deploy/.env" >&2
      mismatches=$((mismatches + 1))
    fi
  done
done

if ((mismatches > 0)); then
  echo "Configuration drift detected; run ./deploy/up.sh --no-build." >&2
  exit 1
fi

echo "Docker API/Worker configuration matches deploy/.env."
