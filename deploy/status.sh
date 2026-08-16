#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"

[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile observability ps

web_port=$(awk -F= '$1 == "WEB_PORT" {print $2}' "$env_file" | tail -1)
api_port=$(awk -F= '$1 == "API_PORT" {print $2}' "$env_file" | tail -1)
bind_ip=$(awk -F= '$1 == "APP_BIND_IP" {print $2}' "$env_file" | tail -1)
web_port=${web_port:-3000}
api_port=${api_port:-8081}
bind_ip=${bind_ip:-127.0.0.1}

echo
probe_ip=$bind_ip
[[ "$probe_ip" == "0.0.0.0" ]] && probe_ip=127.0.0.1
curl -fsS "http://${probe_ip}:${api_port}/health/ready" && echo
curl -fsS -o /dev/null -w "web: HTTP %{http_code}\n" "http://${probe_ip}:${web_port}/healthz"
"$deploy_dir/config-check.sh"
