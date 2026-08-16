#!/usr/bin/env bash

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  echo "Source this file from a shell or use local-run.sh/local-frontend.sh." >&2
  exit 2
fi

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"

if [[ ! -f "$env_file" ]]; then
  echo "Missing $env_file; run ./deploy/init-env.sh first." >&2
  return 1
fi

# Load the same business settings used by Compose.
set -a
# shellcheck disable=SC1090
source "$env_file"
set +a

if [[ "${S3_PUBLIC_PATH_PREFIX:-/storage}" != "/storage" ]]; then
  echo "S3_PUBLIC_PATH_PREFIX must be /storage for the bundled development proxy." >&2
  return 1
fi
if [[ "${S3_SECURE:-false}" != "false" ]]; then
  echo "S3_SECURE must be false for the bundled MinIO service." >&2
  return 1
fi

# Compose service names are only resolvable inside Docker. Derive host-side
# endpoints from the same ports without changing business-level settings.
app_host=${APP_BIND_IP:-127.0.0.1}
infra_host=${INFRA_BIND_IP:-127.0.0.1}
proxy_app_host=$app_host
proxy_infra_host=$infra_host
[[ "$proxy_app_host" == "0.0.0.0" ]] && proxy_app_host=127.0.0.1
[[ "$proxy_infra_host" == "0.0.0.0" ]] && proxy_infra_host=127.0.0.1

export HTTP_ADDR="${app_host}:${API_PORT:-8081}"
export POSTGRES_DSN="postgres://${POSTGRES_USER:-yunpan}:${POSTGRES_PASSWORD:-yunpan}@${infra_host}:${POSTGRES_PORT:-5432}/${POSTGRES_DB:-yunpan}?sslmode=disable"
export REDIS_ADDR="${infra_host}:${REDIS_PORT:-6379}"
export S3_ENDPOINT="${infra_host}:${MINIO_PORT:-9000}"
export S3_SECURE=false
export S3_ACCESS_KEY="${S3_ACCESS_KEY:-${MINIO_ROOT_USER:-yunpan}}"
export S3_SECRET_KEY="${S3_SECRET_KEY:-${MINIO_ROOT_PASSWORD:-yunpan-dev-secret}}"
export YUNPAN_LOCAL_API_URL="http://${proxy_app_host}:${API_PORT:-8081}"
export YUNPAN_LOCAL_STORAGE_URL="http://${proxy_infra_host}:${MINIO_PORT:-9000}"
