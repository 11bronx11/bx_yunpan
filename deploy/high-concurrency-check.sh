#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$deploy_dir/.." && pwd)
run_id=$(date -u +%Y%m%dT%H%M%SZ)-$$
artifact_dir="$repo_dir/artifacts/high-concurrency/$run_id"
mkdir -p "$artifact_dir"

# All credentials here belong to isolated, loopback-only rehearsal services.
# Never source deploy/.env or stop the application's existing services.
compose=(docker compose -f "$deploy_dir/compose.high-concurrency.yaml")
"${compose[@]}" up -d --wait
database="hc_$(date -u +%Y%m%d_%H%M%S)_$$"
"${compose[@]}" exec -T postgres psql -U hc -d hc -v ON_ERROR_STOP=1 -c "CREATE DATABASE $database"
export TEST_POSTGRES_DSN="postgres://hc:hc-local-only@127.0.0.1:${HC_POSTGRES_PORT:-15432}/$database?sslmode=disable"
export POSTGRES_PRIMARY_DSN="$TEST_POSTGRES_DSN"
export TEST_REDIS_ADDR="127.0.0.1:${HC_REDIS_PORT:-16379}"
export TEST_REDIS_FAULTS=1
export TEST_MINIO_ENDPOINT="127.0.0.1:${HC_MINIO_PORT:-19000}"
export TEST_MINIO_ACCESS_KEY=hc-local
export TEST_MINIO_SECRET_KEY=hc-local-only
export GOMAXPROCS=2
export GOFLAGS=-p=1
go_bin=${HC_GO_BIN:-go}

printf 'run=%s\ndatabase=%s\n' "$run_id" "$database" | tee "$artifact_dir/environment.txt"
uname -m >> "$artifact_dir/environment.txt"
free -m >> "$artifact_dir/environment.txt"
"${compose[@]}" ps >> "$artifact_dir/environment.txt"
cd "$repo_dir/backend"

"$go_bin" build -o bin/hc-migrate ./cmd/migrate
bin/hc-migrate up > "$artifact_dir/migration.log" 2>&1
bin/hc-migrate version >> "$artifact_dir/migration.log" 2>&1
bin/hc-migrate down-one >> "$artifact_dir/migration.log" 2>&1
bin/hc-migrate version >> "$artifact_dir/migration.log" 2>&1
bin/hc-migrate up >> "$artifact_dir/migration.log" 2>&1
bin/hc-migrate version >> "$artifact_dir/migration.log" 2>&1
"${compose[@]}" exec -T postgres psql -U hc -d "$database" -f /dev/stdin \
  < "$deploy_dir/high-concurrency-explain.sql" > "$artifact_dir/explain.log" 2>&1
printf '%s\n' 'Migration and EXPLAIN complete.'

"$go_bin" test ./... -count=1 -json > "$artifact_dir/tests.jsonl" 2> "$artifact_dir/tests.stderr.log"
printf '%s\n' 'Full Go suite with isolated dependencies passed.'
"$go_bin" vet ./... > "$artifact_dir/vet.log" 2>&1
# Existing integration suites retain some completed fixtures. The relay tests
# claim global work, so give the race phase a fresh database and run relay
# checks before upload fixtures can add unrelated events.
race_database="${database}_race"
"${compose[@]}" exec -T postgres psql -U hc -d hc -v ON_ERROR_STOP=1 -c "CREATE DATABASE $race_database"
export TEST_POSTGRES_DSN="postgres://hc:hc-local-only@127.0.0.1:${HC_POSTGRES_PORT:-15432}/$race_database?sslmode=disable"
export POSTGRES_PRIMARY_DSN="$TEST_POSTGRES_DSN"
bin/hc-migrate up > "$artifact_dir/race-migration.log" 2>&1
"$go_bin" test -race ./internal/platform/cache ./internal/drive ./internal/outbox ./internal/platform/dbmetrics ./internal/upload -count=1 -json \
  > "$artifact_dir/race.jsonl" 2> "$artifact_dir/race.stderr.log"
printf '%s\n' 'Vet and focused race checks passed.'

docker run --rm --network none --entrypoint promtool \
  -v "$deploy_dir/prometheus:/etc/prometheus:ro" -w /etc/prometheus \
  prom/prometheus:v3.13.2 check rules high-concurrency.rules.yaml > "$artifact_dir/alerts.log" 2>&1
docker run --rm --network none --entrypoint promtool \
  -v "$deploy_dir/prometheus:/etc/prometheus:ro" -w /etc/prometheus \
  prom/prometheus:v3.13.2 test rules high-concurrency.rules.test.yaml >> "$artifact_dir/alerts.log" 2>&1

"$go_bin" build -o bin/hc-api ./cmd/api
api_pids=()
stop_probe_apis() {
  for pid in "${api_pids[@]}"; do kill -TERM "$pid" 2>/dev/null || true; done
  for pid in "${api_pids[@]}"; do wait "$pid" 2>/dev/null || true; done
  api_pids=()
}
trap stop_probe_apis EXIT
# Allocate two distinct loopback ports; existing application listeners stay up.
read -r api_port_one api_port_two < <(node --input-type=module -e '
  import net from "node:net";
  const servers = [net.createServer(), net.createServer()];
  await Promise.all(servers.map(s => new Promise(r => s.listen(0, "127.0.0.1", r))));
  console.log(servers.map(s => s.address().port).join(" "));
  servers.forEach(s => s.close());
')
api_env=(env -u DASHSCOPE_API_KEY APP_ENV=development GIN_MODE=release LOG_LEVEL=warn
  AI_ENABLED=false ETCD_ENABLED=false CACHE_ENABLED=true
  REDIS_ADDR="$TEST_REDIS_ADDR" REDIS_PASSWORD=
  REDIS_COORDINATION_ADDR="$TEST_REDIS_ADDR" REDIS_COORDINATION_PASSWORD= REDIS_COORDINATION_DB=0
  REDIS_CACHE_ADDR="$TEST_REDIS_ADDR" REDIS_CACHE_PASSWORD= REDIS_CACHE_DB=0
  S3_ENDPOINT="$TEST_MINIO_ENDPOINT" S3_ACCESS_KEY=hc-local S3_SECRET_KEY=hc-local-only)
for port in "$api_port_one" "$api_port_two"; do
  "${api_env[@]}" HTTP_ADDR="127.0.0.1:$port" bin/hc-api > "$artifact_dir/api-$port.log" 2>&1 &
  api_pids+=("$!")
  ready=false
  for ((attempt=0; attempt<50; attempt++)); do
    if curl --silent --fail --max-time 1 "http://127.0.0.1:$port/health/live" >/dev/null; then ready=true; break; fi
    sleep 0.1
  done
  "$ready" || { echo "isolated API failed to start on $port" >&2; exit 1; }
done
HC_API_URLS="http://127.0.0.1:$api_port_one,http://127.0.0.1:$api_port_two" \
  node "$deploy_dir/high-concurrency-cache-smoke.mjs" > "$artifact_dir/http-cache-smoke.log" 2>&1
stop_probe_apis
printf '%s\n' 'Two-API HTTP cache and metrics smoke passed.'

"${compose[@]}" exec -T postgres psql -U hc -d "$database" -Atc \
  "SELECT version, dirty FROM schema_migrations; SELECT count(*) FROM users WHERE username LIKE 'hc-owner-%';" \
  > "$artifact_dir/final-state.txt"
node --input-type=module - "$artifact_dir" <<'JS'
import fs from 'node:fs';
const dir = process.argv[2];
const summary = {};
for (const name of ['tests', 'race']) {
  const rows = fs.readFileSync(`${dir}/${name}.jsonl`, 'utf8').trim().split('\n').map(JSON.parse);
  const result = { passed: 0, skipped: 0, failed: 0, packages: 0 };
  for (const row of rows) {
    if (row.Action === 'start') result.packages++;
    if (row.Test && row.Action === 'pass') result.passed++;
    if (row.Test && row.Action === 'skip') result.skipped++;
    if (row.Test && row.Action === 'fail') result.failed++;
  }
  summary[name] = result;
}
fs.writeFileSync(`${dir}/summary.json`, JSON.stringify(summary, null, 2) + '\n');
console.log(JSON.stringify(summary));
JS
printf 'ISOLATED_CHECK=PASS\nartifacts=%s\nretained_databases=%s,%s\n' "$artifact_dir" "$database" "$race_database"
