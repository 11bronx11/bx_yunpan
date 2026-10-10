#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)
env_file="$deploy_dir/.env"

command -v go >/dev/null 2>&1 || { echo "go is required" >&2; exit 1; }
[[ -f "$env_file" ]] || { echo "Missing $env_file; run ./deploy/init-env.sh first." >&2; exit 1; }

# Export only the deployment values needed by the operator command.  The
# command never echoes this environment, so credentials stay out of evidence.
set -a
# shellcheck disable=SC1090
source "$env_file"
set +a

cd "$root_dir/backend"
exec go run ./cmd/search-backfill "$@"
