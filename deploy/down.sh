#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"
remove_volumes=false

[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

if [[ ${1:-} == "--volumes" ]]; then
  remove_volumes=true
elif (($#)); then
  echo "Usage: ./deploy/down.sh [--volumes]" >&2
  exit 2
fi

args=(--env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile observability down --remove-orphans)
if [[ "$remove_volumes" == true ]]; then
  args+=(--volumes)
fi

docker compose "${args[@]}"
