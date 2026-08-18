#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"
compose_file="$deploy_dir/compose.yaml"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }

# shellcheck disable=SC1091
source "$deploy_dir/common.sh"
yunpan_assert_compose_owner "$deploy_dir"

[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

# Load the Compose/business values directly. Do not source local-env.sh here:
# that file intentionally rewrites Docker-only endpoints for host-side runs.
# shellcheck disable=SC1090
set -a
source "$env_file"
set +a

AI_ENABLED="${AI_ENABLED:-true}"
if [[ "$AI_ENABLED" != "true" && "$AI_ENABLED" != "false" ]]; then
  echo "AI_ENABLED must be true or false." >&2
  exit 1
fi

# These two values are derived by x-app-environment in compose.yaml.
S3_ACCESS_KEY="${S3_ACCESS_KEY:-${MINIO_ROOT_USER:-yunpan}}"
S3_SECRET_KEY="${S3_SECRET_KEY:-${MINIO_ROOT_PASSWORD:-yunpan-dev-secret}}"

keys=(
  APP_ENV LOG_LEVEL AI_ENABLED
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
  AISVC_GRPC_ADDR AISVC_GRPC_TARGET AISVC_CALL_TIMEOUT
  AISVC_SEARCH_MAX_ATTEMPTS AISVC_REPROCESS_MAX_ATTEMPTS
  AISVC_RETRY_BASE_BACKOFF AISVC_RETRY_MAX_BACKOFF
  AISVC_BREAKER_WINDOW AISVC_BREAKER_MIN_REQUESTS AISVC_BREAKER_FAILURE_RATE
  AISVC_BREAKER_OPEN_TIMEOUT AISVC_BREAKER_HALF_OPEN_PROBES
  OTEL_TRACES_ENABLED OTEL_EXPORTER_OTLP_ENDPOINT OTEL_TRACES_SAMPLER_ARG
  ETCD_ENABLED ETCD_ENDPOINTS ETCD_USERNAME ETCD_PASSWORD ETCD_DIAL_TIMEOUT
  ETCD_SERVICE_PREFIX ETCD_LEASE_TTL AISVC_ADVERTISE_ADDR
  OUTBOX_LEADER_LOCK_ENABLED OUTBOX_LEADER_LOCK_KEY OUTBOX_LEADER_LOCK_TTL
)

profiles=(--profile app)
services=(api worker)
if [[ "$AI_ENABLED" == "true" ]]; then
  profiles+=(--profile ai)
  services+=(aisvc)
fi
compose=(docker compose --env-file "$env_file" -f "$compose_file" "${profiles[@]}")
mismatches=0

for service in "${services[@]}"; do
  mapfile -t container_ids < <("${compose[@]}" ps -q "$service")
  if ((${#container_ids[@]} == 0)); then
    echo "$service: container is not running" >&2
    mismatches=$((mismatches + 1))
    continue
  fi
  for container_id in "${container_ids[@]}"; do
    container_name=$(docker inspect --format '{{.Name}}' "$container_id")
    container_name=${container_name#/}
    container_env=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$container_id")
    for key in "${keys[@]}"; do
      expected=${!key-}
      actual=$(awk -F= -v key="$key" '$1 == key {sub(/^[^=]*=/, ""); print; exit}' <<<"$container_env")
      if [[ "$actual" != "$expected" ]]; then
        echo "$container_name: $key differs from deploy/.env" >&2
        mismatches=$((mismatches + 1))
      fi
    done
  done
done

if ((mismatches > 0)); then
  echo "Configuration drift detected; run ./deploy/up.sh --no-build." >&2
  exit 1
fi

echo "Docker application configuration matches deploy/.env (AI_ENABLED=$AI_ENABLED)."
