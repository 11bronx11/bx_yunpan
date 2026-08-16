#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)

case "${1:-}" in
  api|worker|migrate) target=$1; shift ;;
  *)
    echo "Usage: ./deploy/local-run.sh {api|worker|migrate} [args...]" >&2
    exit 2
    ;;
esac

# shellcheck disable=SC1091
source "$deploy_dir/local-env.sh"
cd "$root_dir/backend"
exec go run "./cmd/$target" "$@"
