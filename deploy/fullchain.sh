#!/usr/bin/env bash
set -euo pipefail
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)
cd "$root_dir"
project=${FULLCHAIN_PROJECT:-bx-fullchain-20261106}
env_file=${FULLCHAIN_ENV_FILE:-$deploy_dir/.env.fullchain-20261106}
[[ "$project" == bx-fullchain-* ]] || { echo 'A dedicated bx-fullchain-* project is required.' >&2; exit 2; }
[[ -f "$env_file" ]] || { echo "Missing private test env: $env_file" >&2; exit 2; }
compose=(docker compose --project-name "$project" --env-file "$env_file"
  -f "$deploy_dir/compose.yaml" -f "$deploy_dir/compose.staging.yaml" -f "$deploy_dir/compose.fullchain.yaml"
  --profile app --profile distributed --profile ai-split --profile parser
  --profile redis-split --profile search --profile search-external --profile observability)
case "${1:-ps}" in
  up) "${compose[@]}" up -d --no-build --scale worker=0 --scale api=2 --scale outbox-relay=2 --scale maintenance-worker=2 --scale upload-worker=2 ;;
  ps) "${compose[@]}" ps -a ;;
  stop) "${compose[@]}" stop ;;
  core|sharing|ai|retrieval|controls|faults|fallback|audit) FULLCHAIN_PROJECT="$project" node "$deploy_dir/fullchain-test.mjs" "$1" ;;
  *) echo 'Usage: bash deploy/fullchain.sh [up|ps|stop|core|sharing|ai|retrieval|controls|faults|fallback|audit]' >&2; exit 2 ;;
esac
