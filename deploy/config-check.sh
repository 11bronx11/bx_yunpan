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
PARSER_REMOTE_ENABLED="${PARSER_REMOTE_ENABLED:-true}"
if [[ "$PARSER_REMOTE_ENABLED" != "true" && "$PARSER_REMOTE_ENABLED" != "false" ]]; then
  echo "PARSER_REMOTE_ENABLED must be true or false." >&2
  exit 1
fi
PARSER_GRPC_ADDR="${PARSER_GRPC_ADDR:-:8084}"
PARSER_GRPC_TARGET="${PARSER_GRPC_TARGET:-parser-svc:8084}"
PARSER_ETCD_SERVICE_PREFIX="${PARSER_ETCD_SERVICE_PREFIX:-/services/document-parser}"
PARSER_ADVERTISE_ADDR="${PARSER_ADVERTISE_ADDR:-}"
PARSER_CALL_TIMEOUT="${PARSER_CALL_TIMEOUT:-90s}"
PARSER_MAX_CONCURRENT="${PARSER_MAX_CONCURRENT:-2}"

OUTBOX_V2_EVENTS_ENABLED="${OUTBOX_V2_EVENTS_ENABLED:-false}"
if [[ "$OUTBOX_V2_EVENTS_ENABLED" != "true" && "$OUTBOX_V2_EVENTS_ENABLED" != "false" ]]; then
  echo "OUTBOX_V2_EVENTS_ENABLED must be true or false." >&2
  exit 1
fi

# These two values are derived by x-app-environment in compose.yaml.
S3_ACCESS_KEY="${S3_ACCESS_KEY:-${MINIO_ROOT_USER:-yunpan}}"
S3_SECRET_KEY="${S3_SECRET_KEY:-${MINIO_ROOT_PASSWORD:-yunpan-dev-secret}}"
POSTGRES_PRIMARY_DSN="${POSTGRES_PRIMARY_DSN:-${POSTGRES_DSN:-postgres://${POSTGRES_USER:-yunpan}:${POSTGRES_PASSWORD:-yunpan}@postgres:5432/${POSTGRES_DB:-yunpan}?sslmode=disable}}"
POSTGRES_DSN="${POSTGRES_DSN:-$POSTGRES_PRIMARY_DSN}"
REDIS_COORDINATION_ADDR="${REDIS_COORDINATION_ADDR:-redis:6379}"
REDIS_COORDINATION_PASSWORD="${REDIS_COORDINATION_PASSWORD:-${REDIS_PASSWORD:-}}"
REDIS_COORDINATION_DB="${REDIS_COORDINATION_DB:-${REDIS_DB:-0}}"
REDIS_CACHE_ADDR="${REDIS_CACHE_ADDR:-redis:6379}"
REDIS_CACHE_PASSWORD="${REDIS_CACHE_PASSWORD:-${REDIS_PASSWORD:-}}"
REDIS_CACHE_DB="${REDIS_CACHE_DB:-${REDIS_DB:-0}}"
RABBITMQ_URL="${RABBITMQ_URL:-amqp://${RABBITMQ_USER:-yunpan}:${RABBITMQ_PASSWORD:-yunpan-dev-secret}@rabbitmq:5672/}"
RABBITMQ_EXCHANGE="${RABBITMQ_EXCHANGE:-yunpan.tasks}"
RABBITMQ_PREFETCH="${RABBITMQ_PREFETCH:-16}"
RABBITMQ_PUBLISH_TIMEOUT="${RABBITMQ_PUBLISH_TIMEOUT:-5s}"
CACHE_ENABLED="${CACHE_ENABLED:-true}"
CACHE_TTL="${CACHE_TTL:-5m}"
CACHE_L1_MAX_ITEMS="${CACHE_L1_MAX_ITEMS:-512}"
AI_SERVICE_ROLE="${AI_SERVICE_ROLE:-combined}"
SEARCH_INDEX_ENABLED="${SEARCH_INDEX_ENABLED:-false}"
SEARCH_WRITE_BACKENDS="${SEARCH_WRITE_BACKENDS:-postgres}"
SEARCH_READ_BACKEND="${SEARCH_READ_BACKEND:-postgres}"
SEARCH_SHADOW_SAMPLE_RATE="${SEARCH_SHADOW_SAMPLE_RATE:-0}"
SEARCH_INDEX_EPOCH="${SEARCH_INDEX_EPOCH:-v1}"
SEARCH_ES_ENDPOINT="${SEARCH_ES_ENDPOINT:-}"
SEARCH_QDRANT_ENDPOINT="${SEARCH_QDRANT_ENDPOINT:-}"
SEARCH_EXTERNAL_TIMEOUT="${SEARCH_EXTERNAL_TIMEOUT:-2s}"
SEARCH_EXTERNAL_WRITE_TIMEOUT="${SEARCH_EXTERNAL_WRITE_TIMEOUT:-10s}"
SEARCH_INDEX_BATCH_SIZE="${SEARCH_INDEX_BATCH_SIZE:-100}"
SEARCH_VECTOR_OVERSAMPLE="${SEARCH_VECTOR_OVERSAMPLE:-4}"
SEARCH_EXTERNAL_FALLBACK="${SEARCH_EXTERNAL_FALLBACK:-true}"

keys=(
  APP_ENV LOG_LEVEL AI_ENABLED
  HTTP_MAX_BODY_BYTES HTTP_MAX_HEADER_BYTES HTTP_SHUTDOWN_TIMEOUT HTTP_PROBE_TIMEOUT
  HTTP_READ_HEADER_TIMEOUT HTTP_IDLE_TIMEOUT
  POSTGRES_PRIMARY_DSN POSTGRES_DSN
  POSTGRES_MAX_OPEN_CONNS POSTGRES_MAX_IDLE_CONNS POSTGRES_CONN_MAX_LIFETIME POSTGRES_CONN_MAX_IDLE_TIME
  REDIS_PASSWORD REDIS_DB
  REDIS_COORDINATION_ADDR REDIS_COORDINATION_PASSWORD REDIS_COORDINATION_DB
  REDIS_CACHE_ADDR REDIS_CACHE_PASSWORD REDIS_CACHE_DB
  RABBITMQ_URL RABBITMQ_EXCHANGE RABBITMQ_PREFETCH RABBITMQ_PUBLISH_TIMEOUT
  CACHE_ENABLED CACHE_TTL CACHE_L1_MAX_ITEMS
  S3_PUBLIC_ENDPOINT S3_PUBLIC_PATH_PREFIX S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET S3_REGION
  S3_SECURE S3_PUBLIC_SECURE S3_READ_URL_TTL
  AUTH_SIGNING_SEED AUTH_COOKIE_SECURE USER_DEFAULT_QUOTA_BYTES AUTH_ISSUER
  AUTH_ACCESS_TTL AUTH_REFRESH_TTL AUTH_COOKIE_DOMAIN SHARE_SECRET SHARE_ACCESS_TTL
  UPLOAD_SESSION_TTL UPLOAD_PART_URL_TTL UPLOAD_CLEANUP_INTERVAL UPLOAD_CLEANUP_BATCH
  OUTBOX_POLL_INTERVAL OUTBOX_POLL_MAX_INTERVAL OUTBOX_SCAN_TIMEOUT OUTBOX_NOTIFY_ENABLED OUTBOX_BATCH_SIZE OUTBOX_TASK_MAX_RETRY OUTBOX_TASK_TIMEOUT OUTBOX_V2_EVENTS_ENABLED OBJECT_GC_DELAY
  WORKER_CONCURRENCY WORKER_METRICS_ADDR WORKER_QUEUE_AI WORKER_QUEUE_MEDIA WORKER_QUEUE_OBJECT WORKER_QUEUE_MAINTENANCE
  AI_PROVIDER DASHSCOPE_API_KEY AI_BASE_URL AI_CHAT_MODEL AI_EMBEDDING_MODEL AI_VISION_MODEL
  AI_RERANK_ENABLED AI_RERANK_BASE_URL AI_RERANK_MODEL AI_RERANK_CANDIDATES AI_RERANK_TOP_K AI_RERANK_TIMEOUT
  AI_EMBEDDING_DIMENSION AI_MAX_OBJECT_MIB AI_REQUEST_TIMEOUT AI_RATE_LIMIT_ENABLED
  AI_RATE_LIMIT_SEARCH_PER_MINUTE AI_RATE_LIMIT_ASK_PER_MINUTE AI_RATE_LIMIT_REPROCESS_PER_MINUTE
  AISVC_GRPC_ADDR AISVC_GRPC_TARGET AI_SERVICE_ROLE AISVC_CALL_TIMEOUT
  AISVC_SEARCH_MAX_ATTEMPTS AISVC_REPROCESS_MAX_ATTEMPTS
  AISVC_RETRY_BASE_BACKOFF AISVC_RETRY_MAX_BACKOFF
  AISVC_BREAKER_WINDOW AISVC_BREAKER_MIN_REQUESTS AISVC_BREAKER_FAILURE_RATE
  AISVC_BREAKER_OPEN_TIMEOUT AISVC_BREAKER_HALF_OPEN_PROBES
  PARSER_REMOTE_ENABLED PARSER_GRPC_ADDR PARSER_GRPC_TARGET PARSER_ETCD_SERVICE_PREFIX
  PARSER_ADVERTISE_ADDR PARSER_CALL_TIMEOUT PARSER_MAX_CONCURRENT
  OTEL_TRACES_ENABLED OTEL_EXPORTER_OTLP_ENDPOINT OTEL_TRACES_SAMPLER_ARG
  ETCD_ENABLED ETCD_ENDPOINTS ETCD_USERNAME ETCD_PASSWORD ETCD_DIAL_TIMEOUT
  ETCD_SERVICE_PREFIX ETCD_LEASE_TTL AISVC_ADVERTISE_ADDR
  OUTBOX_LEADER_LOCK_ENABLED OUTBOX_LEADER_LOCK_KEY OUTBOX_LEADER_LOCK_TTL
  SEARCH_INDEX_ENABLED SEARCH_WRITE_BACKENDS SEARCH_READ_BACKEND SEARCH_SHADOW_SAMPLE_RATE SEARCH_INDEX_EPOCH
  SEARCH_ES_ENDPOINT SEARCH_ES_USERNAME SEARCH_ES_PASSWORD SEARCH_QDRANT_ENDPOINT SEARCH_QDRANT_API_KEY
  SEARCH_EXTERNAL_TIMEOUT SEARCH_EXTERNAL_WRITE_TIMEOUT SEARCH_INDEX_BATCH_SIZE SEARCH_VECTOR_OVERSAMPLE SEARCH_EXTERNAL_FALLBACK
)

profiles=(--profile app --profile distributed --profile ai --profile ai-split --profile parser --profile redis-split --profile search-external)

if [[ "$SEARCH_READ_BACKEND" == "es_qdrant" ]]; then
  [[ -n "$SEARCH_ES_ENDPOINT" && -n "$SEARCH_QDRANT_ENDPOINT" ]] || {
    echo "SEARCH_ES_ENDPOINT and SEARCH_QDRANT_ENDPOINT are required for SEARCH_READ_BACKEND=es_qdrant." >&2
    exit 1
  }
  [[ ",${SEARCH_WRITE_BACKENDS}," == *,es_qdrant,* ]] || {
    echo "SEARCH_WRITE_BACKENDS must include es_qdrant for SEARCH_READ_BACKEND=es_qdrant." >&2
    exit 1
  }
fi
services=(api)

search_container_ids=()
mapfile -t search_container_ids < <(docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile search ps -q search-indexer)
if ((${#search_container_ids[@]} > 0)); then
  services+=(search-indexer)
  profiles+=(--profile search)
  SEARCH_INDEX_ENABLED=true
fi
search_external_mode=false
search_external_ids=()
mapfile -t search_external_ids < <(docker compose --env-file "$env_file" -f "$compose_file" --profile search-external ps -q elasticsearch)
if ((${#search_external_ids[@]} > 0)); then
  search_external_mode=true
fi
redis_split_mode=false
redis_split_container_ids=()
mapfile -t redis_split_container_ids < <(docker compose --env-file "$env_file" -f "$compose_file" --profile redis-split ps -q redis-coordination)
if ((${#redis_split_container_ids[@]} > 0)); then
  redis_split_mode=true
fi
distributed_container_ids=()
mapfile -t distributed_container_ids < <(docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile distributed ps -q outbox-relay)
if ((${#distributed_container_ids[@]} > 0)); then
  services+=(outbox-relay object-worker media-worker maintenance-worker)
else
  services+=(worker)
fi
ai_split_mode=false
ai_query_container_ids=()
mapfile -t ai_query_container_ids < <(docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile distributed --profile ai --profile ai-split ps -q ai-query)
if ((${#ai_query_container_ids[@]} > 0)); then
	ai_split_mode=true
	services+=(ai-query ai-indexer)
fi
if [[ "$AI_ENABLED" == "true" && "$ai_split_mode" == false ]]; then
  profiles+=(--profile ai)
  services+=(aisvc)
fi
if [[ "$AI_ENABLED" == "true" && "$PARSER_REMOTE_ENABLED" == "true" ]]; then
  services+=(parser-svc)
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
      if [[ "$redis_split_mode" == true ]]; then
        case "$key" in
          REDIS_COORDINATION_ADDR) expected="redis-coordination:6379" ;;
          REDIS_CACHE_ADDR) expected="redis-cache:6379" ;;
        esac
      fi
      if [[ "$key" == "AISVC_GRPC_TARGET" && "$ai_split_mode" == true ]]; then
        expected="ai-query:8082"
      elif [[ "$key" == "AI_SERVICE_ROLE" && "$ai_split_mode" == true ]]; then
        case "$service" in
          ai-query) expected=query ;;
          ai-indexer) expected=indexer ;;
        esac
      elif [[ "$search_external_mode" == true ]]; then
        case "$key" in
          SEARCH_WRITE_BACKENDS) expected=es_qdrant ;;
          SEARCH_ES_ENDPOINT) expected="${SEARCH_ES_ENDPOINT:-http://elasticsearch:9200}" ;;
          SEARCH_QDRANT_ENDPOINT) expected="${SEARCH_QDRANT_ENDPOINT:-http://qdrant:6333}" ;;
        esac
      fi
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
