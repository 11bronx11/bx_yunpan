# P6-P9 Follow-up Evidence

Date: 2026-09-05 UTC

- PostgreSQL migration: `19|false`.
- `go test ./...`: passed in a clean environment; PostgreSQL focused tests passed with `TEST_POSTGRES_DSN`.
- `go vet ./...`: passed.
- `bash -n deploy/*.sh`: passed.
- `git diff --check`: passed.
- Search: index version lifecycle, content snapshot checkpoint, ACL epoch projection, tombstone/version guards.
- Kafka: domain-event-only routes, envelope publication, Kafka-only delivery dispatcher and PostgreSQL delivery state probe.
- Workflow: durable command success/failure, explicit state transitions, compensation convergence and terminal-state guards.
- P9: `deploy/staging-p9-check.sh` intentionally exits `2` without dedicated staging endpoints and owners.
- Runtime: `./deploy/status.sh` and `./deploy/config-check.sh` passed; app + distributed + ai-split + redis-split + search Compose rendered successfully.
- Images: Docker Hub build was refused by TLS timeout; nine local images were refreshed serially from host-built binaries using the existing local runtime layers. Container `/app/service` SHA-256 matched host output for `migrate`, `api`, `outbox-relay`, `object-worker`, `media-worker`, `maintenance-worker`, `aisvc`, `search-indexer`, and `worker`.
- After full-profile restart: 2 API replicas, 2 AI query replicas, 1 AI indexer, search-indexer, outbox/object/media/maintenance workers, split Redis roles, PostgreSQL, MinIO, etcd and web are healthy; migration reports `19|false`.

This is source and single-node evidence. It does not claim PostgreSQL/Redis/MinIO HA, ES/Qdrant cutover, Kafka broker fault/DLQ replay, remote Saga canary, Gateway certificate operation, or physical database migration.
