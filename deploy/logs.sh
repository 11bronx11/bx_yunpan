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

if (($#)); then
  services=("$@")
else
  services=(api worker outbox-relay object-worker media-worker maintenance-worker web migrate search-indexer elasticsearch qdrant)
  if [[ "$ai_enabled" == true ]]; then
    services+=(aisvc ai-query ai-indexer etcd)
    parser_remote_enabled=$(awk -F= '$1 == "PARSER_REMOTE_ENABLED" {print $2}' "$env_file" | tail -1)
    if [[ "${parser_remote_enabled:-true}" == true ]]; then
      services+=(parser-svc)
    fi
  fi
  services+=(redis-coordination redis-cache)
fi

docker compose --env-file "$env_file" -f "$deploy_dir/compose.yaml" --profile app --profile ai --profile ai-split --profile parser --profile distributed --profile redis-split --profile observability --profile search --profile search-external logs --tail=200 -f "${services[@]}"
