#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)
compose_file="$deploy_dir/compose.yaml"
env_file="$deploy_dir/.env"
build=true
observability=false
aisvc_replicas=
api_replicas=
distributed_workers=false
ai_split=false
redis_split=false
search_indexer=false

usage() {
  cat <<'EOF'
Usage: ./deploy/up.sh [--no-build] [--observability] [--distributed-workers] [--ai-split] [--redis-split] [--search-indexer] [--api-replicas N] [--aisvc-replicas N]

  --no-build           Start existing images without building them.
  --observability      Also start Prometheus, Grafana, and OpenTelemetry Collector.
  --distributed-workers Start independent relay/object/media/maintenance roles.
  --ai-split           Start separate ai-query and ai-indexer roles.
  --redis-split        Use separate Broker, Coordination, and Cache Redis services.
  --search-indexer     Start the opt-in PostgreSQL search projection consumer.
  --api-replicas N      Scale core API only when explicitly requested.
  --aisvc-replicas N   Override AISVC_REPLICAS for this run.
EOF
}

while (($#)); do
  case "$1" in
    --no-build) build=false ;;
    --observability) observability=true ;;
    --distributed-workers) distributed_workers=true ;;
    --ai-split) ai_split=true ;;
    --redis-split) redis_split=true ;;
    --search-indexer) search_indexer=true ;;
    --api-replicas)
      (($# >= 2)) || { echo "--api-replicas requires a value" >&2; exit 2; }
      api_replicas=$2
      shift
      ;;
    --api-replicas=*) api_replicas=${1#*=} ;;
    --aisvc-replicas)
      (($# >= 2)) || { echo "--aisvc-replicas requires a value" >&2; exit 2; }
      aisvc_replicas=$2
      shift
      ;;
    --aisvc-replicas=*) aisvc_replicas=${1#*=} ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }

# shellcheck disable=SC1091
source "$deploy_dir/common.sh"
yunpan_assert_compose_owner "$deploy_dir"

if [[ ! -f "$env_file" ]]; then
  "$deploy_dir/init-env.sh"
fi

env_value() {
  local key=$1
  awk -F= -v key="$key" '
    $1 == key {
      sub(/^[^=]*=/, "")
      value = $0
    }
    END {
      sub(/\r$/, "", value)
      if (value ~ /^".*"$/ || value ~ /^\047.*\047$/) {
        value = substr(value, 2, length(value) - 2)
      }
      print value
    }
  ' "$env_file"
}

ai_enabled=$(env_value AI_ENABLED)
ai_enabled=${ai_enabled:-true}
if [[ "$ai_enabled" != "true" && "$ai_enabled" != "false" ]]; then
  echo "AI_ENABLED must be true or false." >&2
  exit 1
fi

outbox_v2_events_enabled=$(env_value OUTBOX_V2_EVENTS_ENABLED)
outbox_v2_events_enabled=${outbox_v2_events_enabled:-false}
if [[ "$outbox_v2_events_enabled" != "true" && "$outbox_v2_events_enabled" != "false" ]]; then
  echo "OUTBOX_V2_EVENTS_ENABLED must be true or false." >&2
  exit 1
fi

if [[ "$ai_enabled" == true ]]; then
  ai_provider=$(env_value AI_PROVIDER)
  ai_provider=${ai_provider:-fake}
  case "$ai_provider" in
    fake) ;;
    dashscope)
      [[ -n "$(env_value DASHSCOPE_API_KEY)" ]] || {
        echo "DASHSCOPE_API_KEY is required when AI_PROVIDER=dashscope." >&2
        exit 1
      }
      ;;
    *)
      echo "AI_PROVIDER must be fake or dashscope when AI is enabled." >&2
      exit 1
      ;;
  esac

  ai_dimension=$(env_value AI_EMBEDDING_DIMENSION)
  ai_dimension=${ai_dimension:-1024}
  if [[ "$ai_dimension" != "1024" ]]; then
    echo "AI_EMBEDDING_DIMENSION must be 1024 to match the pgvector schema." >&2
    exit 1
  fi

  rerank_enabled=$(env_value AI_RERANK_ENABLED)
  rerank_enabled=${rerank_enabled:-false}
  if [[ "$rerank_enabled" != "true" && "$rerank_enabled" != "false" ]]; then
    echo "AI_RERANK_ENABLED must be true or false." >&2
    exit 1
  fi
  if [[ "$rerank_enabled" == true ]]; then
    rerank_model=$(env_value AI_RERANK_MODEL)
    rerank_model=${rerank_model:-qwen3-rerank}
    rerank_base_url=$(env_value AI_RERANK_BASE_URL)
    rerank_candidates=$(env_value AI_RERANK_CANDIDATES)
    rerank_candidates=${rerank_candidates:-30}
    rerank_top_k=$(env_value AI_RERANK_TOP_K)
    rerank_top_k=${rerank_top_k:-8}
    if [[ -z "$rerank_model" ]]; then
      echo "AI_RERANK_MODEL is required when AI_RERANK_ENABLED=true." >&2
      exit 1
    fi
    if [[ "$ai_provider" == dashscope && -z "$rerank_base_url" ]]; then
      echo "AI_RERANK_BASE_URL is required for DashScope reranking." >&2
      exit 1
    fi
    if [[ ! "$rerank_candidates" =~ ^[1-9][0-9]*$ || ! "$rerank_top_k" =~ ^[1-9][0-9]*$ || "$rerank_top_k" -gt "$rerank_candidates" ]]; then
      echo "AI_RERANK_CANDIDATES and AI_RERANK_TOP_K must be positive, with TOP_K <= CANDIDATES." >&2
      exit 1
    fi
  fi
fi

if [[ "$ai_split" == true && "$ai_enabled" != true ]]; then
  echo "--ai-split requires AI_ENABLED=true." >&2
  exit 1
fi

s3_public_path_prefix=$(env_value S3_PUBLIC_PATH_PREFIX)
s3_public_path_prefix=${s3_public_path_prefix:-/storage}
if [[ "$s3_public_path_prefix" != "/storage" ]]; then
  echo "S3_PUBLIC_PATH_PREFIX must be /storage for the bundled Web proxy." >&2
  exit 1
fi

s3_secure=$(env_value S3_SECURE)
s3_secure=${s3_secure:-false}
if [[ "$s3_secure" != "false" ]]; then
  echo "S3_SECURE must be false for the bundled MinIO service." >&2
  exit 1
fi

aisvc_replicas=${aisvc_replicas:-$(env_value AISVC_REPLICAS)}
if [[ "$ai_enabled" == true ]]; then
  aisvc_replicas=${aisvc_replicas:-1}
  if [[ ! "$aisvc_replicas" =~ ^[1-9][0-9]*$ ]]; then
    echo "AISVC_REPLICAS must be a positive integer when AI is enabled." >&2
    exit 1
  fi
elif [[ -n "$aisvc_replicas" && "$aisvc_replicas" != "0" ]]; then
  echo "AISVC_REPLICAS must be 0 when AI_ENABLED=false." >&2
  exit 1
else
  aisvc_replicas=0
fi

if [[ -n "$api_replicas" && ! "$api_replicas" =~ ^[1-9][0-9]*$ ]]; then
  echo "API_REPLICAS must be a positive integer when specified." >&2
  exit 1
fi

go_build_parallelism=$(env_value GO_BUILD_PARALLELISM)
go_build_parallelism=${go_build_parallelism:-1}
go_build_max_procs=$(env_value GO_BUILD_MAX_PROCS)
go_build_max_procs=${go_build_max_procs:-2}
compose_parallel_limit=$(env_value COMPOSE_PARALLEL_LIMIT)
compose_parallel_limit=${compose_parallel_limit:-1}
for build_limit in "$go_build_parallelism" "$go_build_max_procs" "$compose_parallel_limit"; do
  if [[ ! "$build_limit" =~ ^[1-9][0-9]*$ ]]; then
    echo "GO_BUILD_PARALLELISM, GO_BUILD_MAX_PROCS, and COMPOSE_PARALLEL_LIMIT must be positive integers." >&2
    exit 1
  fi
done

if [[ "$build" == true ]]; then
  min_free_gb=${YUNPAN_MIN_FREE_GB:-$(env_value YUNPAN_MIN_FREE_GB)}
  min_free_gb=${min_free_gb:-6}
  if [[ ! "$min_free_gb" =~ ^[1-9][0-9]*$ ]]; then
    echo "YUNPAN_MIN_FREE_GB must be a positive integer." >&2
    exit 1
  fi
  available_kb=$(df -Pk "$root_dir" | awk 'NR == 2 {print $4}')
  required_kb=$((min_free_gb * 1024 * 1024))
  if ((available_kb < required_kb)); then
    available_gb=$(awk -v kb="$available_kb" 'BEGIN {printf "%.1f", kb / 1024 / 1024}')
    echo "Refusing to build: ${available_gb}GB free, ${min_free_gb}GB required." >&2
    echo "Free disk space or use --no-build when application images already exist." >&2
    exit 1
  fi
  build_targets=(migrate api web)
  if [[ "$distributed_workers" == true ]]; then
    build_targets+=(outbox-relay object-worker media-worker maintenance-worker)
  else
    build_targets+=(worker)
  fi
  if [[ "$ai_enabled" == true ]]; then
    build_targets+=(aisvc)
  fi
  if [[ "$search_indexer" == true ]]; then
    build_targets+=(search-indexer)
  fi
  "$deploy_dir/build.sh" "${build_targets[@]}"
fi

profiles=(--profile app)
if [[ "$ai_enabled" == true && "$ai_split" == false ]]; then
  profiles+=(--profile ai)
fi
if [[ "$ai_enabled" == true && "$ai_split" == true ]]; then
  profiles+=(--profile ai-split)
  # The API remains configured through the stable env file by default. The
  # split profile is an explicit run-time override and is not persisted.
  export AISVC_GRPC_TARGET=ai-query:8082
fi
if [[ "$observability" == true ]]; then
  profiles+=(--profile observability)
fi
if [[ "$distributed_workers" == true ]]; then
  profiles+=(--profile distributed)
fi
if [[ "$redis_split" == true ]]; then
  profiles+=(--profile redis-split)
  export REDIS_BROKER_ADDR=redis-broker:6379
  export REDIS_COORDINATION_ADDR=redis-coordination:6379
  export REDIS_CACHE_ADDR=redis-cache:6379
fi
if [[ "$search_indexer" == true ]]; then
  profiles+=(--profile search)
  # The explicit role opt-in also enables search deliveries for this process
  # lifetime; the value is not persisted into the user's env file.
  export SEARCH_INDEX_ENABLED=true
fi

args=(--env-file "$env_file" -f "$compose_file" "${profiles[@]}" up -d --remove-orphans --wait --wait-timeout 240)
scale_args=()
if [[ "$ai_enabled" == true ]]; then
  if [[ "$ai_split" == true ]]; then
    scale_args+=(--scale "ai-query=$aisvc_replicas" --scale "ai-indexer=1")
  else
    scale_args+=(--scale "aisvc=$aisvc_replicas")
  fi
fi
if [[ -n "$api_replicas" ]]; then
  scale_args+=(--scale "api=$api_replicas")
fi
args+=("${scale_args[@]}")
if [[ "$distributed_workers" == true ]]; then
  distributed_services=(migrate api web outbox-relay object-worker media-worker maintenance-worker)
  if [[ "$ai_enabled" == true ]]; then
    if [[ "$ai_split" == true ]]; then
      distributed_services+=(ai-query ai-indexer)
    else
      distributed_services+=(aisvc)
    fi
  fi
  if [[ "$redis_split" == true ]]; then
    distributed_services+=(redis-broker redis-coordination redis-cache)
  fi
  if [[ "$search_indexer" == true ]]; then
    distributed_services+=(search-indexer)
  fi
  args+=("${distributed_services[@]}")
fi

max_attempts=${YUNPAN_UP_ATTEMPTS:-$(env_value YUNPAN_UP_ATTEMPTS)}
max_attempts=${max_attempts:-3}
if [[ ! "$max_attempts" =~ ^[1-9][0-9]*$ ]]; then
  echo "YUNPAN_UP_ATTEMPTS must be a positive integer." >&2
  exit 1
fi
started=false
for ((attempt = 1; attempt <= max_attempts; attempt++)); do
  if COMPOSE_PARALLEL_LIMIT="$compose_parallel_limit" docker compose "${args[@]}"; then
    started=true
    break
  fi
  if ((attempt < max_attempts)); then
    delay=$((attempt * 3))
    echo "Compose startup attempt ${attempt}/${max_attempts} failed; retrying in ${delay}s." >&2
    sleep "$delay"
  fi
done
if [[ "$started" != true ]]; then
  echo "Compose startup failed after ${max_attempts} attempts." >&2
  exit 1
fi

if [[ "$distributed_workers" == true ]]; then
  # Compose does not remove a service that is still part of the selected
  # project but omitted from the explicit service list. Stop the legacy
  # combined worker so it cannot consume queues owned by independent roles.
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile distributed stop worker >/dev/null
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile distributed rm -f worker >/dev/null
else
  # Switching back to the compatibility worker must also remove the
  # independent consumers, otherwise both generations would process a queue.
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile distributed stop outbox-relay object-worker media-worker maintenance-worker >/dev/null
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile distributed rm -f outbox-relay object-worker media-worker maintenance-worker >/dev/null
fi

if [[ "$ai_split" == true ]]; then
  # ai-query/ai-indexer replace the combined aisvc process in split mode.
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile ai --profile ai-split stop aisvc >/dev/null
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile ai --profile ai-split rm -f aisvc >/dev/null
else
  # The combined aisvc is the rollback path when split mode is disabled.
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile ai --profile ai-split stop ai-query ai-indexer >/dev/null
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile ai --profile ai-split rm -f ai-query ai-indexer >/dev/null
fi

if [[ "$ai_enabled" == false ]]; then
  # Profile-disabled services are not orphans, so remove old AI containers
  # explicitly when switching an existing deployment to core-only mode.
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile ai --profile ai-split stop aisvc ai-query ai-indexer etcd
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile ai --profile ai-split rm -f aisvc ai-query ai-indexer etcd
fi

if [[ "$redis_split" == false ]]; then
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile redis-split stop redis-broker redis-coordination redis-cache >/dev/null
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile redis-split rm -f redis-broker redis-coordination redis-cache >/dev/null
fi

if [[ "$search_indexer" == false ]]; then
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile search stop search-indexer >/dev/null
  docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile search rm -f search-indexer >/dev/null
fi

web_port=$(awk -F= '$1 == "WEB_PORT" {print $2}' "$env_file" | tail -1)
web_port=${web_port:-3000}
bind_ip=$(awk -F= '$1 == "APP_BIND_IP" {print $2}' "$env_file" | tail -1)
bind_ip=${bind_ip:-127.0.0.1}
echo "BX YunPan is ready: http://${bind_ip}:${web_port}"
