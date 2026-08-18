#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)
env_file="$deploy_dir/.env"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

# shellcheck disable=SC1091
source "$deploy_dir/common.sh"
yunpan_assert_compose_owner "$deploy_dir"

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

image_tag=${APP_IMAGE_TAG:-$(env_value APP_IMAGE_TAG)}
image_tag=${image_tag:-local}
docker_registry=${DOCKER_REGISTRY:-$(env_value DOCKER_REGISTRY)}
docker_registry=${docker_registry:-docker.io}
go_version=${GO_VERSION:-$(env_value GO_VERSION)}
go_version=${go_version:-1.25.13}
go_proxy=${GO_PROXY:-$(env_value GO_PROXY)}
go_proxy=${go_proxy:-https://proxy.golang.org,direct}
node_version=${NODE_VERSION:-$(env_value NODE_VERSION)}
node_version=${node_version:-22.23.0}
go_build_parallelism=${GO_BUILD_PARALLELISM:-$(env_value GO_BUILD_PARALLELISM)}
go_build_parallelism=${go_build_parallelism:-1}
go_build_max_procs=${GO_BUILD_MAX_PROCS:-$(env_value GO_BUILD_MAX_PROCS)}
go_build_max_procs=${go_build_max_procs:-2}

memory_limit=${BUILD_MEMORY_LIMIT:-$(env_value BUILD_MEMORY_LIMIT)}
memory_limit=${memory_limit:-1536m}
memory_swap_limit=${BUILD_MEMORY_SWAP_LIMIT:-$(env_value BUILD_MEMORY_SWAP_LIMIT)}
memory_swap_limit=${memory_swap_limit:-2048m}
cpu_period=${BUILD_CPU_PERIOD:-$(env_value BUILD_CPU_PERIOD)}
cpu_period=${cpu_period:-100000}
cpu_quota=${BUILD_CPU_QUOTA:-$(env_value BUILD_CPU_QUOTA)}
cpu_quota=${cpu_quota:-100000}
min_available_mb=${BUILD_MIN_AVAILABLE_MB:-$(env_value BUILD_MIN_AVAILABLE_MB)}
min_available_mb=${min_available_mb:-1536}

for positive_integer in "$go_build_parallelism" "$go_build_max_procs" "$cpu_period" "$cpu_quota" "$min_available_mb"; do
  if [[ ! "$positive_integer" =~ ^[1-9][0-9]*$ ]]; then
    echo "Build parallelism, CPU limits, and BUILD_MIN_AVAILABLE_MB must be positive integers." >&2
    exit 1
  fi
done

targets=("$@")
if ((${#targets[@]} == 0)); then
  targets=(migrate api worker aisvc web)
fi
for target in "${targets[@]}"; do
  case "$target" in
    migrate|api|worker|aisvc|web) ;;
    *) echo "Unsupported build target: $target" >&2; exit 2 ;;
  esac
done

build_limits=(
  --memory "$memory_limit"
  --memory-swap "$memory_swap_limit"
  --cpu-period "$cpu_period"
  --cpu-quota "$cpu_quota"
)

for target in "${targets[@]}"; do
  available_mb=$(awk '/MemAvailable:/ {printf "%d", $2 / 1024}' /proc/meminfo)
  if ((available_mb < min_available_mb)); then
    echo "Refusing to build $target: ${available_mb}MiB available, ${min_available_mb}MiB required." >&2
    exit 1
  fi

  echo "Building $target with one image at a time (memory=$memory_limit, memory+swap=$memory_swap_limit, cpu=1 core)..."
  if [[ "$target" == web ]]; then
    docker build "${build_limits[@]}" \
      --build-arg "DOCKER_REGISTRY=$docker_registry" \
      --build-arg "NODE_VERSION=$node_version" \
      --tag "bx-yunpan-web:$image_tag" \
      "$root_dir/web"
  else
    docker build "${build_limits[@]}" \
      --build-arg "DOCKER_REGISTRY=$docker_registry" \
      --build-arg "GO_VERSION=$go_version" \
      --build-arg "GO_PROXY=$go_proxy" \
      --build-arg "GO_BUILD_PARALLELISM=$go_build_parallelism" \
      --build-arg "GO_BUILD_MAX_PROCS=$go_build_max_procs" \
      --build-arg "TARGET=$target" \
      --tag "bx-yunpan-$target:$image_tag" \
      "$root_dir/backend"
  fi
done
