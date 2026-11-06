#!/usr/bin/env bash
set -euo pipefail
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
project=${FULLCHAIN_PROJECT:-bx-fullchain-20261106}
env_file=${FULLCHAIN_ENV_FILE:-$deploy_dir/.env.fullchain-20261106}
[[ "$project" == bx-fullchain-* ]] || { echo 'Dedicated fullchain project required' >&2; exit 2; }
docker compose --project-name "$project" --env-file "$env_file" \
  -f "$deploy_dir/compose.yaml" -f "$deploy_dir/compose.staging.yaml" \
  -f "$deploy_dir/compose.fullchain.yaml" -f "$deploy_dir/compose.sharing.yaml" \
  --profile app --profile distributed --profile ai-split --profile search \
  up -d --no-build --no-deps --wait --wait-timeout 180 \
  --scale api=2 --scale outbox-relay=2 api outbox-relay search-indexer web
