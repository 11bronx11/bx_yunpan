#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }

if [[ ! -f "$env_file" ]]; then
  "$deploy_dir/init-env.sh"
fi

docker compose \
  --env-file "$env_file" \
  -f "$deploy_dir/compose.yaml" \
  up -d --wait --wait-timeout 120 postgres redis minio

docker compose \
  --env-file "$env_file" \
  -f "$deploy_dir/compose.yaml" \
  run --rm --no-deps minio-init

echo "BX YunPan infrastructure is ready for host-side development."
