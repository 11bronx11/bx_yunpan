#!/usr/bin/env python3
"""Bounded, serial checks against explicitly supplied dedicated test resources.

Example: python3 deploy/tests/async-check.py --config /private/test-env.json \
    --output artifacts/async-validation/run-id --suite regression
Configuration is a JSON object of TEST_* environment variables, never deploy/.env.
This runner does not deploy, restart, or clean external services.
"""

import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import time
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]
REQUIRED = (
    "TEST_POSTGRES_DSN", "TEST_CONTROL_POSTGRES_DSN",
    "TEST_POSTGRES_READONLY_DSN", "TEST_RABBITMQ_URL",
    "TEST_MINIO_ENDPOINT", "TEST_MINIO_ACCESS_KEY", "TEST_MINIO_SECRET_KEY",
)
# Child-process helpers and retired execution paths are intentionally not
# required. All skips remain visible in the recorded JSON summary.
CORE_CASES = {
    "TestLongProviderErrorConvergesAndReleasesOnce",
    "TestConcurrentClaimsHaveOneExecution", "TestBackoffDurationExtremes",
    "TestLeaseResultsAndDurableBudget", "TestFinalCrashedAttemptConverges",
    "TestRelaySafeWatermarkAndAtomicTakeover",
    "TestNoticeIdentityAndGenerationFenceExecutionAndResults",
    "TestConflictIsolationFairnessAndGlobalBudgets",
    "TestPriorityMinimumShareAndFencedScheduler",
    "TestStoppedRedoAndResultRetention",
    "TestTwoCommittersDistributeAndTakeOverResultShards",
    "TestHealthSeparatesLiveFromDependencyReadiness",
    "TestBatchCoalescesQuotaAndDuplicateResults",
    "TestBatchRollbackFallsBackAndKeepsFailedResultRecoverable",
    "TestReceiptConflictIsolatedWithoutPoisoningSibling",
    "TestExpiryScanRetainsComputedAndStoppedWork",
    "TestLegacyImportPreservesFanoutBudgetsAndConclusions",
    "TestMultipartUnknownSuccessRecoveryAndDirectedSweep",
    "TestEventRetentionRequiresAcceptedPosition",
    "TestAsyncCoreRealDependencies",
}
RETIRED = (
    "TestAsyncUploadAndGCWithTwoMaintenanceScanners",
    "TestDispatcherRealRabbitCrashBeforeACK",
    "TestRealRabbitLostDeliveryReplaysOriginalTask",
    "TestRealRabbitLostDeliveriesAcrossFiveQueues",
)
EXPECTED_SKIPS = {
    "TestRabbitMQStuckOwnedConsumerHelper", "TestStuckHandlerProcessHelper",
    "TestStuckStoreHelper", "TestRabbitMQCrashConsumerHelper",
    "TestRabbitMQStability", "TestRabbitMQTransport/broker_restart_preserves_confirmed_tasks",
}


def resource_lock():
    # PostgreSQL xmin spans databases within one cluster. Concurrent fault
    # suites with paused/open transactions can invalidate another suite's
    # timing assertions even when their schemas and databases differ.
    lock = ROOT / "artifacts/async-validation/resource.lock"
    lock.parent.mkdir(parents=True, exist_ok=True)
    stream = lock.open("a")
    print("Waiting for exclusive async test resources", flush=True)
    fcntl.flock(stream, fcntl.LOCK_EX)
    return stream


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, ensure_ascii=False) + "\n")


def psql(dsn, sql):
    u = urlsplit(dsn)
    if u.scheme not in ("postgres", "postgresql") or not u.hostname or not u.path:
        raise ValueError("test PostgreSQL DSNs must use URL syntax")
    env = os.environ.copy()
    env.update(PGHOST=u.hostname, PGPORT=str(u.port or 5432),
               PGDATABASE=unquote(u.path[1:]), PGUSER=unquote(u.username or ""),
               PGPASSWORD=unquote(u.password or ""), PGCONNECT_TIMEOUT="5")
    return subprocess.run(["psql", "-X", "-At", "-v", "ON_ERROR_STOP=1"],
                          input=sql, text=True, capture_output=True, env=env, timeout=15)


def preflight(config):
    missing = [key for key in REQUIRED if not config.get(key)]
    if missing:
        raise ValueError("missing required test variables: " + ", ".join(missing))
    identities = {}
    for key in REQUIRED[:3]:
        name = unquote(urlsplit(config[key]).path[1:])
        if not re.search(r"(?:test|async_|validation)", name, re.I):
            raise ValueError(key + " must explicitly name a dedicated test database")
        probe = psql(config[key], "SELECT current_database(),inet_server_addr(),inet_server_port(),current_user;")
        if probe.returncode:
            raise RuntimeError(key + " connection failed (credentials omitted)")
        identities[key] = probe.stdout.strip().split("|")
    if identities[REQUIRED[0]][:3] == identities[REQUIRED[1]][:3]:
        raise ValueError("business and control resolve to the same database")
    if identities[REQUIRED[0]][:3] != identities[REQUIRED[2]][:3]:
        raise ValueError("readonly connection must resolve to the business database")
    versions = {}
    for key in REQUIRED[:2]:
        p = psql(config[key], "SELECT version,dirty FROM schema_migrations;")
        if p.returncode or p.stdout.strip() != ("34|f" if key == REQUIRED[0] else "2|f"):
            raise ValueError(key + " must already have clean current migrations")
        versions[key] = p.stdout.strip()
    # Disable the session setting: database ACLs must still reject writes/DDL.
    denied = {}
    for name, sql in {
        "insert": "INSERT INTO public.file_entries DEFAULT VALUES",
        "update": "UPDATE public.file_entries SET name=name WHERE false",
        "delete": "DELETE FROM public.file_entries WHERE false",
        "ddl": "CREATE TABLE public.async_permission_probe(id integer)",
    }.items():
        p = psql(config[REQUIRED[2]], "BEGIN; SET LOCAL transaction_read_only=off; " + sql + "; ROLLBACK;")
        denied[name] = p.returncode != 0 and "permission denied" in p.stderr.lower()
    if not all(denied.values()):
        raise ValueError("readonly database ACL did not reject every write/DDL probe")
    return {"identities": identities, "migrations": versions, "readonly_denied": denied}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--suite", choices=["core", "regression", "race", "web", "parser", "static"], required=True)
    args = parser.parse_args()
    os.umask(0o077)
    config = json.loads(args.config.read_text())
    if not isinstance(config, dict) or any(not k.startswith("TEST_") or not isinstance(v, str) for k, v in config.items()):
        raise ValueError("configuration must contain string TEST_* values only")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    guard = resource_lock() if args.suite in ("core", "regression", "race") else None
    env = os.environ.copy()
    env.update(config)
    env.update(TEST_ASYNC_CORE="true", GOTOOLCHAIN="go1.25.13", GOMAXPROCS="2", CI="true")
    retired_pattern = "^(" + "|".join(RETIRED) + ")$"
    secrets = [v for k, v in config.items() if any(x in k for x in ("DSN", "URL", "KEY"))]

    def redact(value):
        for secret in sorted(secrets, key=len, reverse=True):
            if secret:
                value = value.replace(secret, "<redacted>")
        return value

    manifest = {"started_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "suite": args.suite, "config_sha256": hashlib.sha256(args.config.read_bytes()).hexdigest(),
                "config_keys": sorted(config), "preflight": preflight(config), "commands": [],
                "excluded_retired_cases": {name: "Old execution authority retired; new control/core integration covers recovery and GC" for name in RETIRED}}
    manifest["head"] = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    paths = subprocess.check_output(["git", "ls-files", "-co", "--exclude-standard", "-z"], cwd=ROOT).split(b"\0")
    manifest["files"] = {os.fsdecode(p): hashlib.sha256((ROOT / os.fsdecode(p)).read_bytes()).hexdigest()
                         for p in sorted(set(paths)) if p and (ROOT / os.fsdecode(p)).is_file()}
    manifest_path = output / (args.suite + "-manifest.json")
    write_json(manifest_path, manifest)
    core = ["./internal/asynccontrol", "./internal/asyncjobs", "./internal/bootstrap"]
    suites = {
        "core": [("core", ["go", "test", "-p=1", *core, "-count=1", "-json", "-timeout=5m", "-skip", retired_pattern], "backend", 600)],
        "regression": [
            ("build", ["go", "build", "-p=1", "./cmd/..."], "backend", 600),
            ("vet", ["go", "vet", "-p=1", "./..."], "backend", 600),
            ("regression", ["go", "test", "-p=1", "./...", "-count=1", "-json", "-timeout=5m", "-skip", retired_pattern], "backend", 900)],
        "race": [("race", ["go", "test", "-race", "-p=1", *core,
                    "./internal/upload", "./internal/objectstore", "./internal/media", "./internal/identity",
                    "./internal/drive", "./internal/sharing", "./internal/ownerepoch", "./internal/platform/cache",
                    "-count=1", "-json", "-timeout=5m", "-skip", retired_pattern], "backend", 1200)],
        "web": [("web-lint", ["npm", "run", "lint"], "web", 300),
                ("web-test", ["npm", "test", "--", "--runInBand", "--watchAll=false", "--testTimeout=30000"], "web", 600),
                ("web-build", ["npm", "run", "build"], "web", 600)],
        "parser": [("parser", ["python3", "-m", "unittest", "parser_worker.test_parsers", "-v"], "backend", 180)],
        "static": [("shell", ["bash", "-n", *["deploy/" + p + ".sh" for p in ["up", "build", "local-run", "local-env", "infra-up", "init-env", "status", "config-check"]]], ".", 30),
                   ("diff", ["git", "diff", "--check"], ".", 30)],
    }
    failed = False
    for name, command, directory, timeout in suites[args.suite]:
        start = time.monotonic()
        print("START " + name, flush=True)
        proc = subprocess.Popen(command, cwd=ROOT / directory, env=env, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, start_new_session=True)
        try:
            content, _ = proc.communicate(timeout=timeout)
            code = proc.returncode
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            content, _ = proc.communicate()
            code = 124
        path = output / (name + (".jsonl" if "-json" in command else ".log"))
        path.write_text(redact(content))
        row = {"name": name, "command": command, "exit_code": code,
               "seconds": round(time.monotonic() - start, 3), "log": path.name}
        if "-json" in command:
            events = []
            for line in content.splitlines():
                try:
                    events.append(json.loads(line))
                except ValueError:
                    pass
            row["top_level"] = {a: sum(e.get("Action") == a and bool(e.get("Test")) and "/" not in e["Test"] for e in events) for a in ["pass", "fail", "skip"]}
            row["subtests_included"] = {a: sum(e.get("Action") == a and bool(e.get("Test")) for e in events) for a in ["pass", "fail", "skip"]}
            row["skips"] = [{"package": e.get("Package"), "test": e["Test"]} for e in events if e.get("Action") == "skip" and e.get("Test")]
            row["unexpected_skips"] = [e for e in row["skips"] if e["test"] not in EXPECTED_SKIPS]
            required = set(CORE_CASES) if args.suite in ("core", "regression", "race") else set()
            if args.suite in ("regression", "race"):
                required.add("TestCachedPagesObserveBackgroundContentEpoch")
            if config.get("TEST_REDIS_FAULTS") == "1" and args.suite in ("regression", "race"):
                required.add("TestJSONStoreRealRedisFallbackBudget")
            if args.suite == "regression" and config.get("TEST_SEARCH_ES_ENDPOINT") and config.get("TEST_SEARCH_QDRANT_ENDPOINT"):
                required.update(["TestExternalContainers", "TestElasticsearchChineseFulltext", "TestExternalTombstonesSurviveReorderedWrites", "TestExternalLiveRebuildAndDocumentRetirement"])
            if args.suite == "regression" and config.get("TEST_CHINESE_PARSER_WORKER") == "1":
                required.add("TestChineseParserToSearchPipeline")
            passed = {e.get("Test") for e in events if e.get("Action") == "pass"}
            row["required_not_passed"] = sorted(required - passed)
            if row["required_not_passed"] or row["unexpected_skips"] or any(e.get("Action") == "fail" for e in events):
                code = code or 1
        row["status"] = "PASS" if code == 0 else "FAIL"
        manifest["commands"].append(row)
        write_json(manifest_path, manifest)
        print(json.dumps({k: v for k, v in row.items() if k not in ("command", "skips")}), flush=True)
        failed |= code != 0
        if name == "build" and code:
            break
    manifest["ended_utc"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    manifest["status"] = "FAIL" if failed else "PASS"
    write_json(manifest_path, manifest)
    if guard:
        guard.close()
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
