# P4/P2/P3 local fault rehearsal

Date: 2026-09-05 UTC

## P4 Redis role isolation

- Baseline `/healthz` and `/readyz`: all hard and advisory probes `ok`.
- Stopped `redis-cache`: `/readyz` stayed `status=ok`; only `redis_cache=degraded`. Restart returned it to `ok` and the container became healthy.
- Stopped `redis-broker`: `/readyz` stayed `status=ok` with `redis_broker=degraded`; a PostgreSQL-backed V2 event rollout probe passed while the broker was down. Restart returned it to healthy.
- Stopped `redis-coordination`: `/readyz` stayed `status=ok` with `redis_coordination=degraded`; outbox relay logs reported lock renewal failure followed by `outbox dispatcher lost leadership`. Restart returned it to healthy.

## Current recovery state

- Full Compose profile restored: two API replicas, two AI query replicas, AI indexer, outbox relay, search indexer, four dedicated workers, split Redis, PostgreSQL, MinIO, etcd, and web.
- All containers healthy/running after restoration.
- PostgreSQL migration `19|false`; Outbox pending/failed `0/0`.

## Boundary

These are reversible single-host fault checks. They do not provide Redis HA/failover, PostgreSQL HA/PITR, Kafka, ES/Qdrant, Gateway, or physical database split evidence; those remain staging-gated.
