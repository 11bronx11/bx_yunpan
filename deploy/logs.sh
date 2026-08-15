#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"

[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

if (($#)); then
  services=("$@")
else
  services=(api worker web migrate)
fi

docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app logs --tail=200 -f "${services[@]}"
