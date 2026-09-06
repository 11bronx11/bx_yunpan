# Single-host core hot-path validation

Date: 2026-09-05 UTC

## Real HTTP flow passed

- Register and login through the web/API entrypoint.
- Root folder lookup and child folder creation.
- One-part presigned multipart upload through Nginx to MinIO.
- Upload completion and asynchronous status convergence to `completed`.
- Download URL retrieval and byte-for-byte downloaded-content comparison.
- Share creation, public resolve, public content access, revoke.
- File deletion and empty-folder deletion.
- Logout.

## Reliability probes passed

- Repeated upload request with the same `Idempotency-Key` returned the same upload session ID.
- One API replica was stopped; `/readyz` returned HTTP 200 for 10/10 probes through the remaining replica.
- The stopped replica was restarted and became healthy; `deploy/status.sh` passed.
- Cache Redis was stopped; `/readyz` remained `status=ok` with `redis_cache=degraded`, login and root-folder access returned HTTP 200, and the cache container recovered healthy.

## PostgreSQL authority checks

- Schema migration: `19|false`.
- Outbox event pending/failed: `0/0`.
- Outbox delivery pending/failed: `0/0`.
- Non-terminal storage operations: `0`.
- Active upload sessions: `0`.

## Boundary

The provider-backed DashScope AI request path was not invoked to avoid an external billable dependency; AI service health, split roles, and offline/fallback behavior remain covered by the existing local tests and runtime probes. This evidence is a single-host core-flow result, not production HA, capacity, or RPO/RTO evidence.
