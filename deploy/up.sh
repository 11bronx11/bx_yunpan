#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)
compose_file="$deploy_dir/compose.yaml"
env_file="$deploy_dir/.env"
build=true
observability=false

usage() {
  cat <<'EOF'
Usage: ./deploy/up.sh [--no-build] [--observability]

  --no-build        Start existing images without building them.
  --observability   Also start Prometheus, Grafana, and OpenTelemetry Collector.
EOF
}

while (($#)); do
  case "$1" in
    --no-build) build=false ;;
    --observability) observability=true ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
  shift
done

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }

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
    echo "AI_PROVIDER must be fake or dashscope." >&2
    exit 1
    ;;
esac

ai_dimension=$(env_value AI_EMBEDDING_DIMENSION)
ai_dimension=${ai_dimension:-1024}
if [[ "$ai_dimension" != "1024" ]]; then
  echo "AI_EMBEDDING_DIMENSION must be 1024 to match the pgvector schema." >&2
  exit 1
fi

if [[ "$build" == true ]]; then
  min_free_gb=${YUNPAN_MIN_FREE_GB:-4}
  available_kb=$(df -Pk "$root_dir" | awk 'NR == 2 {print $4}')
  required_kb=$((min_free_gb * 1024 * 1024))
  if ((available_kb < required_kb)); then
    available_gb=$(awk -v kb="$available_kb" 'BEGIN {printf "%.1f", kb / 1024 / 1024}')
    echo "Refusing to build: ${available_gb}GB free, ${min_free_gb}GB required." >&2
    echo "Free disk space or use --no-build when application images already exist." >&2
    exit 1
  fi
fi

profiles=(--profile app)
if [[ "$observability" == true ]]; then
  profiles+=(--profile observability)
fi

args=(--env-file "$env_file" -f "$compose_file" "${profiles[@]}" up -d --remove-orphans --wait --wait-timeout 240)
if [[ "$build" == true ]]; then
  args+=(--build)
fi

docker compose "${args[@]}"

web_port=$(awk -F= '$1 == "WEB_PORT" {print $2}' "$env_file" | tail -1)
web_port=${web_port:-3000}
echo "BX YunPan is ready: http://127.0.0.1:${web_port}"
