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
ubuntu_version=${UBUNTU_VERSION:-$(env_value UBUNTU_VERSION)}
ubuntu_version=${ubuntu_version:-24.04}
ubuntu_mirror=${UBUNTU_MIRROR:-$(env_value UBUNTU_MIRROR)}
ubuntu_mirror=${ubuntu_mirror:-http://mirrors.aliyun.com/ubuntu-ports/}
parser_python_index=${PARSER_PYTHON_INDEX:-$(env_value PARSER_PYTHON_INDEX)}
parser_python_index=${parser_python_index:-https://mirrors.aliyun.com/pypi/simple}
go_proxy=${GO_PROXY:-$(env_value GO_PROXY)}
go_proxy=${go_proxy:-https://proxy.golang.org,direct}
parser_go_proxy=${PARSER_GO_PROXY:-$(env_value PARSER_GO_PROXY)}
parser_go_proxy=${parser_go_proxy:-https://goproxy.cn,direct}
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
min_available_disk_mb=${BUILD_MIN_AVAILABLE_DISK_MB:-$(env_value BUILD_MIN_AVAILABLE_DISK_MB)}
min_available_disk_mb=${min_available_disk_mb:-2048}

for positive_integer in "$go_build_parallelism" "$go_build_max_procs" "$cpu_period" "$cpu_quota" "$min_available_mb" "$min_available_disk_mb"; do
  if [[ ! "$positive_integer" =~ ^[1-9][0-9]*$ ]]; then
    echo "Build parallelism, CPU limits, and BUILD_MIN_AVAILABLE_MB must be positive integers." >&2
    exit 1
  fi
done

targets=("$@")
if ((${#targets[@]} == 0)); then
	targets=(migrate api worker aisvc parser-svc web)
fi
for target in "${targets[@]}"; do
  case "$target" in
		migrate|migrate-asynq|api|worker|aisvc|parser-svc|outbox-relay|object-worker|media-worker|maintenance-worker|search-indexer|web) ;;
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

  available_disk_mb=$(df -Pm "$root_dir" | awk 'NR == 2 {print $4}')
  if ((available_disk_mb < min_available_disk_mb)); then
    echo "Refusing to build $target: ${available_disk_mb}MiB disk available on the project filesystem, ${min_available_disk_mb}MiB required." >&2
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
    dockerfile="$root_dir/backend/Dockerfile"
    target_build_args=()
    if [[ "$target" == parser-svc ]]; then
      dockerfile="$root_dir/backend/Dockerfile.parser"
      target_build_args+=(--build-arg "UBUNTU_VERSION=$ubuntu_version")
      target_build_args+=(--build-arg "UBUNTU_MIRROR=$ubuntu_mirror")
      target_build_args+=(--build-arg "GO_PROXY=$parser_go_proxy")
      target_build_args+=(--build-arg "PYTHON_PACKAGE_INDEX=$parser_python_index")
    fi
    docker build "${build_limits[@]}" \
      --build-arg "DOCKER_REGISTRY=$docker_registry" \
      --build-arg "GO_VERSION=$go_version" \
      --build-arg "GO_PROXY=$go_proxy" \
      --build-arg "GO_BUILD_PARALLELISM=$go_build_parallelism" \
      --build-arg "GO_BUILD_MAX_PROCS=$go_build_max_procs" \
      --build-arg "TARGET=$target" \
      "${target_build_args[@]}" \
      --tag "bx-yunpan-$target:$image_tag" \
      -f "$dockerfile" \
      "$root_dir/backend"
  fi
done
