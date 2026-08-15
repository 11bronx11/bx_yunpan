#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"

[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile observability ps

web_port=$(awk -F= '$1 == "WEB_PORT" {print $2}' "$env_file" | tail -1)
api_port=$(awk -F= '$1 == "API_PORT" {print $2}' "$env_file" | tail -1)
web_port=${web_port:-3000}
api_port=${api_port:-8081}

echo
curl -fsS "http://127.0.0.1:${api_port}/health/ready" && echo
curl -fsS -o /dev/null -w "web: HTTP %{http_code}\n" "http://127.0.0.1:${web_port}/healthz"
