#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }

# shellcheck disable=SC1091
source "$deploy_dir/common.sh"
yunpan_assert_compose_owner "$deploy_dir"

[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

ai_enabled=$(awk -F= '$1 == "AI_ENABLED" {print $2}' "$env_file" | tail -1)
ai_enabled=${ai_enabled:-true}
if [[ "$ai_enabled" != "true" && "$ai_enabled" != "false" ]]; then
  echo "AI_ENABLED must be true or false." >&2
  exit 1
fi

docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile ai --profile observability ps

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

# aisvc 只暴露 gRPC；逐个检查所有副本，不能让一个健康副本掩盖其他故障。
compose=(docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile ai)
mapfile -t aisvc_ids < <("${compose[@]}" ps -q aisvc)
aisvc_failures=0
if [[ "$ai_enabled" == true ]]; then
  if ((${#aisvc_ids[@]} == 0)); then
    echo "aisvc: no running containers" >&2
    aisvc_failures=1
  fi
  for container_id in "${aisvc_ids[@]}"; do
    container_name=$(docker inspect --format '{{.Name}}' "$container_id")
    container_name=${container_name#/}
    if docker exec "$container_id" /app/service healthcheck >/dev/null 2>&1; then
      echo "$container_name -> grpc.health.v1: SERVING"
    else
      echo "$container_name -> grpc.health.v1: FAILED" >&2
      aisvc_failures=$((aisvc_failures + 1))
    fi
  done
elif ((${#aisvc_ids[@]} > 0)); then
  echo "aisvc: disabled but running containers still exist" >&2
  aisvc_failures=1
else
  echo "aisvc: disabled"
fi

curl -fsS -o /dev/null -w "web: HTTP %{http_code}\n" "http://${probe_ip}:${web_port}/healthz"
curl -fsS -o /dev/null -w "web -> api: HTTP %{http_code}\n" "http://${probe_ip}:${web_port}/readyz"
curl -fsS -o /dev/null -w "web -> storage: HTTP %{http_code}\n" "http://${probe_ip}:${web_port}/storage-healthz"
"$deploy_dir/config-check.sh"

if ((aisvc_failures > 0)); then
  exit 1
fi
