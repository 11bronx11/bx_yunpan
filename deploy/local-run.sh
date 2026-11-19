#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)

case "${1:-}" in
  api|worker|aisvc|core-internal|ai-query|ai-content-worker|ai-embedding-worker|ai-summary-worker|ai-text-projector|ai-vector-projector|ai-migrate|parser-svc|async-control|gc-worker|upload-worker|object-worker|media-worker|search-indexer|async-import|async-admin|async-handover|reader-init|ai-reader-init|migrate) target=$1; shift ;;
  *)
    echo "Usage: ./deploy/local-run.sh {api|async-control|upload-worker|object-worker|media-worker|gc-worker|core-internal|ai-query|ai-content-worker|ai-embedding-worker|ai-summary-worker|ai-text-projector|ai-vector-projector|ai-migrate|parser-svc|async-import|async-admin|async-handover|reader-init|ai-reader-init|migrate} [args...]" >&2
    exit 2
    ;;
esac

# shellcheck disable=SC1091
task_metrics_addr=${WORKER_METRICS_ADDR:-}
source "$deploy_dir/local-env.sh"
if [[ ${ASYNC_BROKER_PREFIX:-bx.async.v1} == bx.async.v1 ]]; then export ASYNC_BROKER_PREFIX=bx.async.v2; fi
if [[ "$target" == ai-reader-init ]]; then
 exec docker compose --env-file "$deploy_dir/.env" -f "$deploy_dir/compose.yaml" --profile app --profile ai-split run --rm ai-role-init
fi
if [[ "$target" == reader-init ]]; then
  exec docker compose --env-file "$deploy_dir/.env" -f "$deploy_dir/compose.yaml" --profile app run --rm --no-deps async-reader-init
fi
if [[ -n "$task_metrics_addr" ]]; then
  export WORKER_METRICS_ADDR=$task_metrics_addr
elif [[ ${WORKER_METRICS_ADDR:-:8083} == :8083 ]]; then
  case "$target" in
    async-control|worker) metrics_port=18083 ;;
    upload-worker) metrics_port=18085 ;;
    object-worker) metrics_port=18086 ;;
    media-worker) metrics_port=18087 ;;
    gc-worker) metrics_port=18088 ;;
    aisvc) metrics_port=18089 ;;
    parser-svc) metrics_port=18090 ;;
    search-indexer) metrics_port=18091 ;;
    core-internal) metrics_port=18092 ;;
    ai-query) metrics_port=18093 ;;
    ai-content-worker) metrics_port=18095 ;;
    ai-embedding-worker) metrics_port=18096 ;;
    ai-summary-worker) metrics_port=18097 ;;
    ai-text-projector) metrics_port=18098 ;;
    ai-vector-projector) metrics_port=18099 ;;
    *) metrics_port=18100 ;;
  esac
  export WORKER_METRICS_ADDR="127.0.0.1:$metrics_port"
fi
cd "$root_dir/backend"
exec go run "./cmd/$target" "$@"
