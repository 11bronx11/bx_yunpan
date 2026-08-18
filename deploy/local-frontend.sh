#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)

# shellcheck disable=SC1091
source "$deploy_dir/local-env.sh"
export HOST="${APP_BIND_IP:-127.0.0.1}"
export PORT="${WEB_PORT:-3000}"
cd "$root_dir/web"
exec npm start
