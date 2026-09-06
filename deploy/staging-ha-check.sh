#!/usr/bin/env bash
set -euo pipefail

# This gate is intentionally non-destructive. It checks that a staging target
# has the required HA endpoints and credentials before an operator runs the
# failover/PITR playbook. It never promotes, fences, or deletes a database.
required=(POSTGRES_PRIMARY_DSN POSTGRES_REPLICA_DSNS BACKUP_BUCKET WAL_ARCHIVE_TARGET)
missing=0
for key in "${required[@]}"; do
  if [[ -z "${!key:-}" ]]; then
    echo "missing staging HA variable: $key" >&2
    missing=$((missing + 1))
  fi
done
if ((missing > 0)); then
  echo "staging HA gate: blocked until dedicated PostgreSQL HA/PITR endpoints are configured" >&2
  exit 2
fi

echo "staging HA gate: prerequisites present"
echo "next: run backup -> PITR -> planned failover -> old-primary fencing -> rewind/catch-up -> optional planned failback"
