# VPN refresh closeout

Date: 2026-09-05 UTC

## Dependency and image refresh

- Go modules downloaded with temporary `GOPROXY=https://goproxy.cn,direct`.
- Web dependencies installed with temporary `npm ci --registry=https://registry.npmmirror.com --ignore-scripts` (1434 packages).
- Docker Hub `registry-1.docker.io` still timed out from the Docker daemon after VPN enablement.
- Current-source Go binaries were compiled serially with `go build -p=1 GOMAXPROCS=2` and injected into the existing verified Alpine runtime layers; local `bx-yunpan-*:local` images were recreated without changing repository defaults.

## Runtime verification

- `./deploy/up.sh --no-build --distributed-workers --ai-split --redis-split --search-indexer --api-replicas 2 --aisvc-replicas 2`: pass.
- 20 Compose containers healthy, including two API replicas, two AI query replicas, AI indexer, outbox relay, search indexer, four dedicated workers, and split Redis services.
- `./deploy/status.sh`: pass; API readiness, AI gRPC health, worker metrics, web/API/storage probes all pass.
- `./deploy/config-check.sh`: pass.
- PostgreSQL migration: `19|false`; Outbox pending/failed: `0/0`.
- Container `/app/service` hash matches the corresponding current-source host build for all eight backend targets; `/app/migrations/000019_search_workflow_metadata.up.sql` is present.
- Explicit SHA-256 comparison for `migrate`, `api`, `outbox-relay`, `object-worker`, `media-worker`, `maintenance-worker`, `search-indexer`, and `aisvc`: all `MATCH`.

## Quality gates

- `TEST_POSTGRES_DSN=... go test ./...`: pass.
- `go vet ./...`: pass.
- Direct frontend ESLint: pass.
- `bash -n deploy/*.sh` and `git diff --check`: pass.

## Boundary

This is single-host development verification. Dedicated staging/production PostgreSQL HA/PITR, Redis failover, MinIO failover, ES/Qdrant rebuild and cutover, Kafka broker/DLQ/watermark replay, Gateway TLS, and physical database split remain gated by the staging endpoint checks and were not claimed as complete.
