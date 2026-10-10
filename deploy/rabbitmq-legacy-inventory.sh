#!/usr/bin/env bash
set -euo pipefail
# Reads a snapshot of a stopped legacy Redis volume. The source is mounted
# read-only; Redis replays AOF in the disposable container's filesystem.
# Never run a cutover against a changing source without first stopping writers.
volume=${1:?usage: rabbitmq-legacy-inventory.sh LEGACY_REDIS_VOLUME}
[[ "$volume" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]+$ ]] || exit 2
docker run --rm --network none -v "$volume:/legacy:ro" --entrypoint sh redis:8.10-alpine -ec '
 mkdir /snapshot
 cp -a /legacy/. /snapshot/
 redis-server --dir /snapshot --appendonly yes --save "" --daemonize yes --logfile /tmp/redis.log
 for i in $(seq 1 30); do [ "$(redis-cli ping 2>/dev/null)" = PONG ] && break; sleep 0.1; done
 [ "$(redis-cli ping)" = PONG ] || { echo "Redis snapshot did not finish loading" >&2; exit 1; }
 for db in $(seq 0 15); do
  result=$(redis-cli -n "$db" --raw EVAL '\''local cursor="0"; local counts={}; repeat local r=redis.call("SCAN",cursor,"MATCH","asynq:*","COUNT",1000); cursor=r[1]; for _,k in ipairs(r[2]) do local kind=redis.call("TYPE",k).ok; if kind=="hash" then local state=redis.call("HGET",k,"state"); if state then counts[state]=(counts[state] or 0)+1 end end end until cursor=="0"; return cjson.encode(counts)'\'' 0)
  printf "db=%s task_states=%s\n" "$db" "$result"
 done
'
