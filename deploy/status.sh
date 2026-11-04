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
parser_remote_enabled=$(awk -F= '$1 == "PARSER_REMOTE_ENABLED" {print $2}' "$env_file" | tail -1)
parser_remote_enabled=${parser_remote_enabled:-true}
if [[ "$parser_remote_enabled" != "true" && "$parser_remote_enabled" != "false" ]]; then
  echo "PARSER_REMOTE_ENABLED must be true or false." >&2
  exit 1
fi

docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile ai --profile ai-split --profile parser --profile distributed --profile redis-split --profile observability --profile search --profile search-external ps

web_port=$(awk -F= '$1 == "WEB_PORT" {print $2}' "$env_file" | tail -1)
bind_ip=$(awk -F= '$1 == "APP_BIND_IP" {print $2}' "$env_file" | tail -1)
web_port=${web_port:-3000}
bind_ip=${bind_ip:-127.0.0.1}

echo
probe_ip=$bind_ip
[[ "$probe_ip" == "0.0.0.0" ]] && probe_ip=127.0.0.1

# API has no fixed host port in the scalable profile. Probe every container so
# one healthy replica cannot hide an unready peer.
compose=(docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile ai --profile ai-split --profile parser --profile distributed --profile redis-split --profile search --profile search-external)
mapfile -t api_ids < <("${compose[@]}" ps -q api)
api_failures=0
if ((${#api_ids[@]} == 0)); then
  echo "api: no running containers" >&2
  api_failures=1
fi
for container_id in "${api_ids[@]}"; do
  container_name=$(docker inspect --format '{{.Name}}' "$container_id")
  container_name=${container_name#/}
  if docker exec "$container_id" wget -q -O - http://127.0.0.1:8081/health/ready >/dev/null; then
    echo "$container_name -> HTTP readiness: OK"
  else
    echo "$container_name -> HTTP readiness: FAILED" >&2
    api_failures=$((api_failures + 1))
  fi
done

# aisvc 只暴露 gRPC；逐个检查所有副本，不能让一个健康副本掩盖其他故障。
# Keep the distributed profile enabled while probing optional role services;
# targeting a profiled service without its profile can make Compose validate an
# incomplete dependency graph.
compose=(docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile ai --profile ai-split --profile parser --profile distributed --profile redis-split)
mapfile -t aisvc_ids < <("${compose[@]}" ps -q aisvc)
mapfile -t ai_query_ids < <("${compose[@]}" ps -q ai-query)
mapfile -t ai_indexer_ids < <("${compose[@]}" ps -q ai-indexer)
mapfile -t parser_ids < <("${compose[@]}" ps -q parser-svc)
aisvc_failures=0
if [[ "$ai_enabled" == true ]]; then
	if ((${#ai_query_ids[@]} > 0)); then
		if ((${#ai_indexer_ids[@]} == 0)); then
			echo "ai-indexer: no running containers" >&2
			aisvc_failures=1
		fi
		for container_id in "${ai_query_ids[@]}"; do
			container_name=$(docker inspect --format '{{.Name}}' "$container_id")
			container_name=${container_name#/}
			if docker exec "$container_id" /app/service healthcheck >/dev/null 2>&1; then
				echo "$container_name -> grpc.health.v1: SERVING"
			else
				echo "$container_name -> grpc.health.v1: FAILED" >&2
				aisvc_failures=$((aisvc_failures + 1))
			fi
		done
		for container_id in "${ai_indexer_ids[@]}"; do
			container_name=$(docker inspect --format '{{.Name}}' "$container_id")
			container_name=${container_name#/}
			echo "$container_name -> ai queue worker: running"
		done
	elif ((${#aisvc_ids[@]} == 0)); then
		echo "aisvc: no running containers" >&2
		aisvc_failures=1
	else
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
	fi
elif ((${#aisvc_ids[@]} > 0)); then
  echo "aisvc: disabled but running containers still exist" >&2
  aisvc_failures=1
else
  echo "aisvc: disabled"
fi

if [[ "$ai_enabled" == true && "$parser_remote_enabled" == true ]]; then
  if ((${#parser_ids[@]} == 0)); then
    echo "parser-svc: no running containers" >&2
    aisvc_failures=$((aisvc_failures + 1))
  fi
  for container_id in "${parser_ids[@]}"; do
    container_name=$(docker inspect --format '{{.Name}}' "$container_id")
    container_name=${container_name#/}
    if docker exec "$container_id" /app/service healthcheck >/dev/null 2>&1; then
      echo "$container_name -> document parser grpc.health.v1: SERVING"
    else
      echo "$container_name -> document parser grpc.health.v1: FAILED" >&2
      aisvc_failures=$((aisvc_failures + 1))
    fi
    if docker exec "$container_id" wget -q -O - http://127.0.0.1:8083/metrics >/dev/null; then
      echo "$container_name -> parser metrics: OK"
    else
      echo "$container_name -> parser metrics: FAILED" >&2
      aisvc_failures=$((aisvc_failures + 1))
    fi
  done
elif ((${#parser_ids[@]} > 0)); then
  echo "parser-svc: running while remote parsing is disabled" >&2
  aisvc_failures=$((aisvc_failures + 1))
fi

curl -fsS -o /dev/null -w "web: HTTP %{http_code}\n" "http://${probe_ip}:${web_port}/healthz"
curl -fsS -o /dev/null -w "web -> api: HTTP %{http_code}\n" "http://${probe_ip}:${web_port}/readyz"
curl -fsS -o /dev/null -w "web -> storage: HTTP %{http_code}\n" "http://${probe_ip}:${web_port}/storage-healthz"
"$deploy_dir/config-check.sh"

# Probe the role metrics endpoints when the distributed profile is active.
for role in outbox-relay object-worker media-worker maintenance-worker; do
  mapfile -t role_ids < <("${compose[@]}" ps -q "$role")
  for container_id in "${role_ids[@]}"; do
    if docker exec "$container_id" wget -q -O - http://127.0.0.1:8083/metrics >/dev/null; then
      echo "$role -> metrics: OK"
    else
      echo "$role -> metrics: FAILED" >&2
      api_failures=$((api_failures + 1))
    fi
  done
done
mapfile -t search_ids < <("${compose[@]}" ps -q search-indexer)
for container_id in "${search_ids[@]}"; do
  if docker exec "$container_id" wget -q -O - http://127.0.0.1:8083/metrics >/dev/null; then
    echo "search-indexer -> metrics: OK"
  else
    echo "search-indexer -> metrics: FAILED" >&2
    api_failures=$((api_failures + 1))
  fi
done

if ((api_failures > 0 || aisvc_failures > 0)); then
  exit 1
fi
