#!/usr/bin/env bash
set -euo pipefail
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"
build=true
ai_split=true
observability=false
redis_split=false
search_indexer=false
search_backends=false
api_replicas=
aisvc_replicas=
parser_replicas=
usage() {
  cat <<'USAGE'
Usage: ./deploy/up.sh [--no-build] [--distributed-workers] [--ai-split] [--redis-split] [--search-indexer] [--search-backends] [--observability] [--api-replicas N] [--aisvc-replicas N] [--parser-replicas N]
Core default: async-control, upload/object/media/gc workers; independent business/control/AI migrations. AI uses explicit roles by default.
--ai-split is a compatibility alias; combined AI execution has retired.
--distributed-workers is retained as an alias for the default explicit roles.
The script stops old business/async writers before migration and hands off retained AI work without clearing queues or data.
USAGE
}
while (($#)); do
  case "$1" in
    --no-build) build=false ;;
    --distributed-workers) ;;
    --ai-split) ai_split=true ;;
    --redis-split) redis_split=true ;;
    --search-indexer) search_indexer=true ;;
    --search-backends) search_backends=true ;;
    --observability) observability=true ;;
    --api-replicas|--aisvc-replicas|--parser-replicas)
      (($#>=2)) || { usage >&2; exit 2; }
      [[ "$2" =~ ^[1-9][0-9]*$ ]] || { echo 'Replica count must be positive.' >&2; exit 2; }
      case "$1" in --api-replicas) api_replicas=$2 ;; --aisvc-replicas) aisvc_replicas=$2 ;; --parser-replicas) parser_replicas=$2 ;; esac
      shift ;;
    --api-replicas=*) api_replicas=${1#*=} ;;
    --aisvc-replicas=*) aisvc_replicas=${1#*=} ;;
    --parser-replicas=*) parser_replicas=${1#*=} ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; exit 2 ;;
  esac
  shift
done
for replicas in "$api_replicas" "$aisvc_replicas" "$parser_replicas"; do
  [[ -z "$replicas" || "$replicas" =~ ^[1-9][0-9]*$ ]] || { echo 'Replica count must be positive.' >&2; exit 2; }
done
source "$deploy_dir/common.sh"
yunpan_assert_compose_owner "$deploy_dir"
"$deploy_dir/init-env.sh"
# Existing values, endpoints and secrets remain in the user's single source.
set -a
source "$env_file"
set +a
if [[ ${ASYNC_PROTOCOL_VERSION:-2} == 3 ]]; then
  if [[ ${ASYNC_BROKER_PREFIX:-bx.async.v2} == bx.async.v[12] || ${ASYNC_LEGACY_BROKER_PREFIX:-bx.async.v1} == bx.async.v1 || ${ASYNC_BROKER_PREFIX:-} == "${ASYNC_LEGACY_BROKER_PREFIX:-}" ]]; then
    echo 'v3 requires a new ASYNC_BROKER_PREFIX and the actual v2 ASYNC_LEGACY_BROKER_PREFIX; finish v1 handover using v2 first.' >&2; exit 1
  fi
fi
if [[ ${AI_ENABLED:-true} == true && ${AI_EMBEDDING_DIMENSION:-1024} != 1024 ]]; then
  echo 'AI_EMBEDDING_DIMENSION must match vector(1024).' >&2; exit 1
fi
profiles=(--profile app)
services=(postgres async-postgres redis rabbitmq minio minio-init migrate migrate-control async-reader-init api web async-control upload-worker object-worker media-worker gc-worker)
targets=(async-handover migrate api async-control upload-worker object-worker media-worker gc-worker web)
if [[ ${AI_ENABLED:-true} == true ]]; then
  targets+=(core-internal ai-query ai-content-worker ai-embedding-worker ai-summary-worker ai-text-projector ai-vector-projector ai-migrate)
  if [[ ${ASYNC_PROTOCOL_VERSION:-2} == 3 ]]; then targets+=(ai-direct-admin); fi
  profiles+=(--profile ai-split); services+=(ai-postgres migrate-ai ai-role-init core-internal ai-query ai-content-worker ai-embedding-worker ai-summary-worker)
  if [[ "$search_indexer" == true || ${SEARCH_INDEX_ENABLED:-false} == true || "$search_backends" == true ]]; then
    if [[ -n ${SEARCH_ES_ENDPOINT:-} || "$search_backends" == true ]]; then services+=(ai-text-projector); fi
    if [[ -n ${SEARCH_QDRANT_ENDPOINT:-} || "$search_backends" == true ]]; then services+=(ai-vector-projector); fi
  fi
  if [[ ${PARSER_REMOTE_ENABLED:-true} == true ]]; then profiles+=(--profile parser); services+=(parser-svc); targets+=(parser-svc); fi
fi
if [[ "$redis_split" == true ]]; then
  profiles+=(--profile redis-split); services+=(redis-coordination redis-cache)
  export REDIS_COORDINATION_ADDR=redis-coordination:6379 REDIS_CACHE_ADDR=redis-cache:6379
fi
if [[ "$search_indexer" == true || ${SEARCH_INDEX_ENABLED:-false} == true || "$search_backends" == true ]]; then
  export SEARCH_INDEX_ENABLED=true
fi
if [[ "$search_backends" == true ]]; then
  profiles+=(--profile search-external); services+=(elasticsearch qdrant)
  export SEARCH_ES_ENDPOINT=${SEARCH_ES_ENDPOINT:-http://elasticsearch:9200} SEARCH_QDRANT_ENDPOINT=${SEARCH_QDRANT_ENDPOINT:-http://qdrant:6333} SEARCH_WRITE_BACKENDS=es_qdrant
fi
if [[ "$observability" == true ]]; then profiles+=(--profile observability); services+=(prometheus grafana otel-collector); fi
compose=(docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml")
"${compose[@]}" "${profiles[@]}" config --quiet
if [[ "$build" == true ]]; then "$deploy_dir/build.sh" "${targets[@]}"; fi
# Fence the old execution authority before either migration/import or new
# producers can run. Containers/queues/volumes are preserved for inspection.
"${compose[@]}" --profile app --profile distributed --profile legacy-aliases --profile ai --profile ai-split --profile search --profile parser stop api worker outbox-relay maintenance-worker object-worker media-worker upload-worker aisvc ai-query ai-indexer search-indexer parser-svc async-control gc-worker core-internal ai-content-worker ai-embedding-worker ai-summary-worker ai-text-projector ai-vector-projector
# Removed services may still exist as preserved containers from an old Compose
# definition. Stop exactly this project's retired writers by their labels.
coord_project=$("${compose[@]}" config --format json | python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])')
for coord_role in committer ai-committer; do
  mapfile -t coord_containers < <(docker ps -q --filter "label=com.docker.compose.project=$coord_project" --filter "label=com.docker.compose.service=$coord_role")
  if ((${#coord_containers[@]})); then docker stop "${coord_containers[@]}"; fi
done
export ASYNC_LEGACY_WRITERS_STOPPED=true
export AI_LEGACY_WRITERS_STOPPED=true
# Complete the cutover before launching any new business writer. API itself
# keeps no dependency on control PG/MQ; this ordering belongs to deployment.
COMPOSE_PARALLEL_LIMIT=${COMPOSE_PARALLEL_LIMIT:-1} "${compose[@]}" "${profiles[@]}" up -d --no-build migrate migrate-control async-reader-init
yunpan_wait_one_shots compose migrate migrate-control async-reader-init
# This is the v1 coordination handover, not a repeat of async-import or AI archive.
# Existing .env values remain untouched. Retired v1 default needs an explicit
# new-protocol launch value for this invocation.
if [[ ${ASYNC_BROKER_PREFIX:-bx.async.v1} == bx.async.v1 ]]; then export ASYNC_BROKER_PREFIX=bx.async.v2; fi
if [[ ${AI_ENABLED:-true} == true ]]; then
  "${compose[@]}" "${profiles[@]}" up -d --no-build migrate-ai ai-role-init
  yunpan_wait_one_shots compose migrate-ai ai-role-init
fi
# Handover reads retained broker messages before any application role starts.
"${compose[@]}" "${profiles[@]}" up -d --no-build --wait --wait-timeout 240 rabbitmq
"${compose[@]}" "${profiles[@]}" --profile async-admin run --rm --no-deps async-handover transfer
if [[ ${ASYNC_PROTOCOL_VERSION:-2} == 3 && ${AI_ENABLED:-true} == true ]]; then
  "${compose[@]}" "${profiles[@]}" --profile async-admin run --rm --no-deps ai-direct-admin transfer
  "${compose[@]}" "${profiles[@]}" --profile async-admin run --rm --no-deps ai-direct-admin activate
fi
"${compose[@]}" "${profiles[@]}" --profile async-admin run --rm --no-deps async-handover activate
scale=()
[[ -z "$api_replicas" ]] || scale+=(--scale "api=$api_replicas")
if [[ -n "$aisvc_replicas" ]]; then
  scale+=(--scale "ai-query=$aisvc_replicas")
fi
[[ -z "$parser_replicas" ]] || scale+=(--scale "parser-svc=$parser_replicas")
COMPOSE_PARALLEL_LIMIT=${COMPOSE_PARALLEL_LIMIT:-1} "${compose[@]}" "${profiles[@]}" up -d --no-build --wait --wait-timeout 240 "${scale[@]}" "${services[@]}"
if [[ ${AI_ENABLED:-true} == true ]]; then echo "AI historical rebuild is resumable; inspect ./deploy/ai-upgrade.sh verify, then archive when complete."; fi
echo "BX YunPan is ready: http://${APP_BIND_IP:-127.0.0.1}:${WEB_PORT:-3000}"
