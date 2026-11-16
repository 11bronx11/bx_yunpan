#!/usr/bin/env bash

yunpan_assert_compose_owner() {
  local deploy_dir=$1
  local project_name=${COMPOSE_PROJECT_NAME:-bx-yunpan}
  local owners owner

  owners=$(docker ps -a \
    --filter "label=com.docker.compose.project=${project_name}" \
    --format '{{.Label "com.docker.compose.project.working_dir"}}' \
    | sed '/^$/d' \
    | sort -u)

  [[ -z "$owners" ]] && return 0

  while IFS= read -r owner; do
    [[ "$owner" == "$deploy_dir" ]] && continue
    echo "Compose project ${project_name} belongs to another checkout: ${owner}" >&2
    echo "Refusing to mix that stack with ${deploy_dir}/.env." >&2
    echo "Run the deployment script from the owning checkout, or stop that stack first." >&2
    return 1
  done <<<"$owners"
}

# Compose wait omits already-exited containers in some v2 releases. A fast
# migration can finish before that command lists it, so inspect all service
# containers and wait on their IDs instead.
yunpan_wait_one_shots() {
  local -n compose_command=$1
  shift
  local service container_id exit_code
  local -a container_ids
  for service in "$@"; do
    mapfile -t container_ids < <("${compose_command[@]}" ps --all --quiet "$service")
    if ((${#container_ids[@]} == 0)); then
      echo "$service: no container found for one-shot completion check" >&2
      return 1
    fi
    for container_id in "${container_ids[@]}"; do
      exit_code=$(docker wait "$container_id") || return 1
      if [[ "$exit_code" != 0 ]]; then
        echo "$service: one-shot container exited with code $exit_code" >&2
        return 1
      fi
    done
  done
}
