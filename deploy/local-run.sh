#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)

case "${1:-}" in
  api|worker|aisvc|parser-svc|async-control|committer|gc-worker|upload-worker|object-worker|media-worker|search-indexer|async-import|async-admin|reader-init|migrate) target=$1; shift ;;
  *)
    echo "Usage: ./deploy/local-run.sh {api|async-control|committer|upload-worker|object-worker|media-worker|gc-worker|aisvc|parser-svc|search-indexer|async-import|async-admin|reader-init|migrate} [args...]" >&2
    exit 2
    ;;
esac

# shellcheck disable=SC1091
task_metrics_addr=${WORKER_METRICS_ADDR:-}
source "$deploy_dir/local-env.sh"
if [[ "$target" == reader-init ]]; then
  exec docker compose --env-file "$deploy_dir/.env" -f "$deploy_dir/compose.yaml" --profile app run --rm --no-deps async-reader-init
fi
if [[ -n "$task_metrics_addr" ]]; then
  export WORKER_METRICS_ADDR=$task_metrics_addr
elif [[ ${WORKER_METRICS_ADDR:-:8083} == :8083 ]]; then
  case "$target" in
    async-control|worker) metrics_port=18083 ;;
    committer) metrics_port=18084 ;;
    upload-worker) metrics_port=18085 ;;
    object-worker) metrics_port=18086 ;;
    media-worker) metrics_port=18087 ;;
    gc-worker) metrics_port=18088 ;;
    aisvc) metrics_port=18089 ;;
    parser-svc) metrics_port=18090 ;;
    search-indexer) metrics_port=18091 ;;
    *) metrics_port=18092 ;;
  esac
  export WORKER_METRICS_ADDR="127.0.0.1:$metrics_port"
fi
cd "$root_dir/backend"
exec go run "./cmd/$target" "$@"
