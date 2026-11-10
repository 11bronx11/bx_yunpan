#!/usr/bin/env bash
set -euo pipefail
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
project=${FULLCHAIN_PROJECT:-bx-fullchain-20261106}
env_file=${FULLCHAIN_ENV_FILE:-$deploy_dir/.env.fullchain-20261106}
[[ "$project" == bx-fullchain-* ]] || { echo 'Dedicated fullchain project required' >&2; exit 2; }
AISVC_GRPC_TARGET=ai-query:8082 COMPOSE_PARALLEL_LIMIT=1 docker compose --project-name "$project" --env-file "$env_file" \
  -f "$deploy_dir/compose.yaml" -f "$deploy_dir/compose.staging.yaml" \
  -f "$deploy_dir/compose.fullchain.yaml" -f "$deploy_dir/compose.sharing.yaml" \
  -f "$deploy_dir/compose.sharing-lifecycle.yaml" \
  --profile app --profile distributed --profile ai --profile ai-split --profile parser \
  --profile redis-split --profile search --profile search-external \
  up -d --no-build --wait --wait-timeout 240 \
  --scale worker=0 --scale aisvc=0 --scale api=2 --scale outbox-relay=2 \
  --scale maintenance-worker=2 --scale upload-worker=2
