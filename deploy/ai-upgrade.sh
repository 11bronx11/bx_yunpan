#!/usr/bin/env bash
# Retained-data AI cutover. Never purges queues or runs the core importer.
set -euo pipefail
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
phase=${1:-inspect}
shift || true
source "$deploy_dir/common.sh"
yunpan_assert_compose_owner "$deploy_dir"
legacy_stopped=${AI_LEGACY_WRITERS_STOPPED-}
set -a
source "$deploy_dir/.env"
if [[ -n "$legacy_stopped" ]]; then export AI_LEGACY_WRITERS_STOPPED=$legacy_stopped; fi
set +a
run_id=${1:-$(python3 -c 'import os,uuid;print(uuid.uuid5(uuid.NAMESPACE_URL,"bx-yunpan-ai-cutover:"+os.getenv("AI_PROFILE_ID","default")+":"+os.getenv("AI_PROFILE_REVISION","v1")))')}
compose=(docker compose --env-file "$deploy_dir/.env" -f "$deploy_dir/compose.yaml" --profile app --profile ai-split)
tool() { "${compose[@]}" run --rm --no-deps -T ai-migrate "$@"; }
batch() {
  local report
  while true; do
    report=$(tool "$1" --run "$run_id" --batch 100)
    echo "$report"
    if [[ "$1" == verify ]] && [[ $(python3 -c 'import json,sys;r=json.load(sys.stdin);print(str(r.get("has_more",False) and r.get("verified",0)==0).lower())' <<<"$report") == true ]]; then
      echo "AI migration still awaits published text; inspect tasks and rerun verify after they finish." >&2; return 1
    fi
    [[ $(python3 -c 'import json,sys;print(str(json.load(sys.stdin).get("has_more",False)).lower())' <<<"$report") == true ]] || break
  done
}
case "$phase" in
  prepare)
    [[ ${AI_LEGACY_WRITERS_STOPPED:-false} == true ]] || { echo 'Stop old AI/search consumers and set AI_LEGACY_WRITERS_STOPPED=true.' >&2; exit 1; }
    COMPOSE_PARALLEL_LIMIT=1 "${compose[@]}" up -d --no-build migrate migrate-control migrate-ai async-reader-init ai-role-init rabbitmq
    yunpan_wait_one_shots compose migrate migrate-control migrate-ai async-reader-init ai-role-init
    batch rebuild-plan
    batch handoff
    batch enqueue-intents
    echo "AI migration prepared: $run_id; start explicit roles, then run ./deploy/ai-upgrade.sh archive $run_id."
    ;;
  inspect) tool inspect ;;
  verify) tool verify --run "$run_id" --batch 100 ;;
  archive)
    [[ ${AI_LEGACY_WRITERS_STOPPED:-false} == true ]] || { echo 'AI_LEGACY_WRITERS_STOPPED=true required.' >&2; exit 1; }
    batch verify
    tool cutover-check --run "$run_id"
    "${compose[@]}" run --rm --no-deps -T migrate archive up
    ;;
  *) echo 'Usage: ./deploy/ai-upgrade.sh inspect|prepare|verify|archive [RUN_UUID]' >&2; exit 2 ;;
esac
