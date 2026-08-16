#!/usr/bin/env bash

yunpan_assert_compose_owner() {
  local deploy_dir=$1
  local project_name=bx-yunpan
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
