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
export POSTGRES_PRIMARY_DSN="${POSTGRES_PRIMARY_DSN:-${POSTGRES_DSN:-postgres://${POSTGRES_USER:-yunpan}:${POSTGRES_PASSWORD:-yunpan}@${infra_host}:${POSTGRES_PORT:-5432}/${POSTGRES_DB:-yunpan}?sslmode=disable}}"
export POSTGRES_DSN="$POSTGRES_PRIMARY_DSN"
export REDIS_ADDR="${infra_host}:${REDIS_PORT:-6379}"
export S3_ENDPOINT="${infra_host}:${MINIO_PORT:-9000}"
export S3_SECURE=false
export S3_ACCESS_KEY="${S3_ACCESS_KEY:-${MINIO_ROOT_USER:-yunpan}}"
export S3_SECRET_KEY="${S3_SECRET_KEY:-${MINIO_ROOT_PASSWORD:-yunpan-dev-secret}}"
# aisvc:8082 只在 Docker 网络内可解析，宿主机改指本机端口。
aisvc_port=${AISVC_GRPC_ADDR##*:}
export AISVC_GRPC_TARGET="${proxy_app_host}:${aisvc_port:-8082}"
# parser-svc:8084 只在 Docker 网络内可解析，宿主机运行时改指本机端口。
parser_port=${PARSER_GRPC_ADDR##*:}
export PARSER_GRPC_TARGET="${proxy_app_host}:${parser_port:-8084}"
# otel-collector:4317 同理只在 Docker 网络内可解析。
if [[ "${OTEL_EXPORTER_OTLP_ENDPOINT:-}" == otel-collector:* ]]; then
  export OTEL_EXPORTER_OTLP_ENDPOINT="${proxy_infra_host}:${OTEL_EXPORTER_OTLP_ENDPOINT##*:}"
fi
# etcd 只在 compose 内网暴露，宿主机直跑时没有可连的地址：关掉服务发现，
# 退回直连 AISVC_GRPC_TARGET 和 PARSER_GRPC_TARGET。
if [[ "${ETCD_ENDPOINTS:-}" == etcd:* ]]; then
  export ETCD_ENABLED=false
fi
export YUNPAN_LOCAL_API_URL="http://${proxy_app_host}:${API_PORT:-8081}"
export YUNPAN_LOCAL_STORAGE_URL="http://${proxy_infra_host}:${MINIO_PORT:-9000}"

# Host endpoints are derived from the same configuration, including explicit
# Compose DSNs. Independent external hosts are preserved.
export POSTGRES_READONLY_DSN="${POSTGRES_READONLY_DSN:-postgres://${POSTGRES_READER_USER:-yunpan_async_reader}:${POSTGRES_READER_PASSWORD:-${POSTGRES_PASSWORD:-yunpan}}@${infra_host}:${POSTGRES_PORT:-5432}/${POSTGRES_DB:-yunpan}?sslmode=disable}"
export ASYNC_CONTROL_DSN="${ASYNC_CONTROL_DSN:-postgres://${ASYNC_POSTGRES_USER:-yunpan_control}:${ASYNC_POSTGRES_PASSWORD:-${POSTGRES_PASSWORD:-yunpan}}@${infra_host}:${ASYNC_POSTGRES_PORT:-5433}/${ASYNC_POSTGRES_DB:-yunpan_control}?sslmode=disable}"
export RABBITMQ_URL="${RABBITMQ_URL:-amqp://${RABBITMQ_USER:-yunpan}:${RABBITMQ_PASSWORD:-yunpan-dev-secret}@${infra_host}:${RABBITMQ_PORT:-5672}/}"
export POSTGRES_PRIMARY_DSN="${POSTGRES_PRIMARY_DSN//@postgres:5432/@${infra_host}:${POSTGRES_PORT:-5432}}"
export POSTGRES_DSN="$POSTGRES_PRIMARY_DSN"
export POSTGRES_READONLY_DSN="${POSTGRES_READONLY_DSN//@postgres:5432/@${infra_host}:${POSTGRES_PORT:-5432}}"
export ASYNC_CONTROL_DSN="${ASYNC_CONTROL_DSN//@async-postgres:5432/@${infra_host}:${ASYNC_POSTGRES_PORT:-5433}}"
export RABBITMQ_URL="${RABBITMQ_URL//@rabbitmq:5672/@${infra_host}:${RABBITMQ_PORT:-5672}}"
