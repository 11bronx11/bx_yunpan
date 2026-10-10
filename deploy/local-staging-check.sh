#!/usr/bin/env bash
set -euo pipefail

# Reversible single-host staging rehearsal for core fail/recover behavior.
# It never deletes volumes and does not claim multi-host HA evidence.
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"
compose_file="$deploy_dir/compose.yaml"
run_id=$(date -u +%Y%m%dT%H%M%SZ)
artifact_dir=${LOCAL_STAGING_ARTIFACT_DIR:-"$deploy_dir/../artifacts/architecture-upgrade/${run_id}-local-staging"}
mkdir -p "$artifact_dir"
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }
[[ -f "$env_file" ]] || { echo "Missing $env_file" >&2; exit 1; }

compose=(docker compose --env-file "$env_file" -f "$compose_file" --profile app --profile distributed --profile ai --profile ai-split --profile redis-split --profile search --profile search-external)
web_port=$(awk -F= '$1 == "WEB_PORT" {print $2}' "$env_file" | tail -1)
web_port=${web_port:-3000}
bind_ip=$(awk -F= '$1 == "APP_BIND_IP" {print $2}' "$env_file" | tail -1)
bind_ip=${bind_ip:-127.0.0.1}
[[ "$bind_ip" == "0.0.0.0" ]] && bind_ip=127.0.0.1
base_url="http://${bind_ip}:${web_port}"
postgres_user=$(awk -F= '$1 == "POSTGRES_USER" {print $2}' "$env_file" | tail -1)
postgres_user=${postgres_user:-yunpan}
postgres_db=$(awk -F= '$1 == "POSTGRES_DB" {print $2}' "$env_file" | tail -1)
postgres_db=${postgres_db:-yunpan}
postgres_port=$(awk -F= '$1 == "POSTGRES_PORT" {print $2}' "$env_file" | tail -1)
postgres_port=${postgres_port:-5432}

log_file="$artifact_dir/run.log"
: > "$log_file"
exec > >(tee -a "$log_file") 2>&1
stopped_services=()
stopped_containers=()
test_db=""
probe_event_id=""
restore() {
  if ((${#stopped_services[@]} > 0)); then
    printf 'restoring services: %s\n' "${stopped_services[*]}"
    "${compose[@]}" start "${stopped_services[@]}" >/dev/null || true
  fi
  for container_id in "${stopped_containers[@]}"; do
    docker start "$container_id" >/dev/null 2>&1 || true
  done
}
cleanup_test_db() {
  if [[ -n "$probe_event_id" ]]; then
    psql -Atc "delete from outbox_events where id = '$probe_event_id';" >/dev/null 2>&1 || true
  fi
  if [[ -n "$test_db" ]]; then
    psql -Atc "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$test_db' AND pid <> pg_backend_pid();" >/dev/null 2>&1 || true
    psql -Atc "DROP DATABASE IF EXISTS \"$test_db\";" >/dev/null 2>&1 || true
  fi
}
trap 'cleanup_test_db; restore' EXIT INT TERM

wait_http() {
  local url=$1 expected=${2:-200} attempts=${3:-30} code
  for ((i=1; i<=attempts; i++)); do
    code=$(curl -sS -o /dev/null -w '%{http_code}' "$url" || true)
    if [[ "$code" == "$expected" ]]; then return 0; fi
    sleep 1
  done
  echo "probe failed: $url expected HTTP $expected, got ${code:-no-response}" >&2
  return 1
}
assert_http_200() { wait_http "$1" 200 15; }
assert_http_not_200() {
  local url=$1 code
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$url" || true)
  [[ "$code" != "200" ]] || { echo "expected non-200: $url" >&2; return 1; }
}
psql() { "${compose[@]}" exec -T postgres psql -U "$postgres_user" -d "$postgres_db" "$@"; }

echo "local staging rehearsal: baseline"
assert_http_200 "$base_url/healthz"
assert_http_200 "$base_url/readyz"
assert_http_200 "$base_url/storage-healthz"

mapfile -t api_ids < <("${compose[@]}" ps -q api)
(( ${#api_ids[@]} >= 2 )) || { echo "need at least two API replicas" >&2; exit 1; }
echo "local staging rehearsal: one API replica down"
api_to_stop=${api_ids[0]}
docker stop "$api_to_stop" >/dev/null
stopped_containers+=("$api_to_stop")
assert_http_200 "$base_url/readyz"
docker start "$api_to_stop" >/dev/null
stopped_containers=()
wait_http "$base_url/readyz" 200 30

echo "local staging rehearsal: cache unavailable"
"${compose[@]}" stop redis-cache >/dev/null
stopped_services+=(redis-cache)
assert_http_200 "$base_url/readyz"
"${compose[@]}" start redis-cache >/dev/null
stopped_services=()
wait_http "$base_url/readyz" 200 30

echo "local staging rehearsal: broker unavailable and outbox recovery"
"${compose[@]}" stop outbox-relay rabbitmq >/dev/null
stopped_services+=(outbox-relay rabbitmq)
assert_http_200 "$base_url/readyz"
event_id=$(psql -Atc 'select gen_random_uuid()' | tr -d '[:space:]')
probe_event_id="$event_id"
aggregate_id=$(psql -Atc 'select gen_random_uuid()' | tr -d '[:space:]')
payload=$(printf '{"owner_id":"%s","file_entry_id":"%s","object_id":"%s"}' "$aggregate_id" "$aggregate_id" "$aggregate_id")
psql -Atc "INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, event_version, payload, status, available_at, message_kind, aggregate_version, producer, occurred_at) VALUES ('$event_id'::uuid, 'file_entry', '$aggregate_id'::uuid, 'file.created.v1', 1, '$payload'::jsonb, 'pending', now(), 'domain_event', 1, 'local-staging-check', now());" >/dev/null
sleep 3
pending_before=$(psql -Atc "select count(*) from outbox_events where id = '$event_id' and status <> 'published';" | tr -d '[:space:]')
[[ "$pending_before" == "1" ]] || { echo "broker outage did not leave retryable outbox" >&2; exit 1; }
"${compose[@]}" start rabbitmq outbox-relay >/dev/null
stopped_services=()
status=pending
for ((i=1; i<=30; i++)); do
  status=$(psql -Atc "select status from outbox_events where id = '$event_id';" | tr -d '[:space:]')
  [[ "$status" == "published" ]] && break
  sleep 1
done
[[ "$status" == "published" ]] || { echo "outbox did not recover after broker restore" >&2; exit 1; }
psql -Atc "delete from outbox_events where id = '$event_id';" >/dev/null
probe_event_id=""

echo "local staging rehearsal: object storage unavailable"
"${compose[@]}" stop minio >/dev/null
stopped_services+=(minio)
assert_http_not_200 "$base_url/storage-healthz"
"${compose[@]}" start minio >/dev/null
stopped_services=()
wait_http "$base_url/storage-healthz" 200 30

echo "local staging rehearsal: focused Go state-machine tests"
"${compose[@]}" stop outbox-relay >/dev/null
stopped_services+=(outbox-relay)
postgres_password=$(awk -F= '$1 == "POSTGRES_PASSWORD" {print $2}' "$env_file" | tail -1)
test_db="yunpan_local_staging_${$}"
psql -Atc "CREATE DATABASE \"$test_db\";" >/dev/null
test_dsn="postgres://${postgres_user}:${postgres_password}@postgres:5432/${test_db}?sslmode=disable"
"${compose[@]}" run --rm --no-deps -e "POSTGRES_PRIMARY_DSN=$test_dsn" -e "POSTGRES_DSN=$test_dsn" migrate up
test_host_dsn="postgres://${postgres_user}:${postgres_password}@127.0.0.1:${postgres_port}/${test_db}?sslmode=disable"
(export TEST_POSTGRES_DSN="$test_host_dsn"; cd "$deploy_dir/../backend"; go test ./... -count=1)

"${compose[@]}" start outbox-relay >/dev/null
stopped_services=()
wait_http "$base_url/readyz" 200 30
"$deploy_dir/status.sh" | tee "$artifact_dir/status.txt"
"$deploy_dir/config-check.sh" | tee "$artifact_dir/config-check.txt"
psql -Atc "select 'migration=' || version || ':' || dirty from schema_migrations order by version desc limit 1; select 'outbox_pending=' || count(*) from outbox_events where status <> 'published'; select 'outbox_delivery_pending=' || count(*) from outbox_deliveries where status <> 'published'; select 'storage_non_terminal=' || count(*) from storage_operations where state in ('pending','running','failed');" | tee "$artifact_dir/database-state.txt"
echo "LOCAL_STAGING_REHEARSAL=PASS"
