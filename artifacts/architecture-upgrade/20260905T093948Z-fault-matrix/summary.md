# Local fault matrix continuation

Date: 2026-09-05 UTC

## Completed local rehearsals

- P4 cache failure: stopping `redis-cache` left API `/readyz` at `status=ok` with only `redis_cache=degraded`; restore returned all probes to `ok`.
- P4 broker failure: stopping `redis-broker` left API `/readyz` available with `redis_broker=degraded`; a PostgreSQL-backed V2 event write probe passed while the broker was down.
- P4 coordination failure: stopping `redis-coordination` produced Relay lock-renewal failure and `outbox dispatcher lost leadership`; restore returned coordination healthy.
- P2 Relay window: stopping Relay before inserting a `file.created.v1` event left it pending; restart published both independent destinations (`2/2`, Asynq maintenance and search projection), then the probe event was deleted.
- P3 AI isolation: stopping one or both `ai-query` replicas and stopping `ai-indexer` left API readiness and storage health HTTP 200; all services were restored.
- P0.4 object-storage failure: stopping MinIO changed API readiness to `status=unavailable` with `object_storage=unavailable` and web storage probe HTTP 504; restore returned HTTP 200 and readiness `ok`.

## Focused verification

With the live PostgreSQL DSN, these packages passed with `-count=1`:

`internal/outbox`, `internal/recovery`, `internal/storageops`, `internal/ai`, `internal/platform/httpapi`, `internal/platform/cache`.

## Boundary

These are reversible single-host fault windows and do not satisfy production HA/PITR, broker failover, ES/Qdrant cutover, Kafka DLQ/watermark replay, Gateway, or physical database split gates.
