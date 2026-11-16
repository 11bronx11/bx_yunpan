#!/usr/bin/env python3
"""Bounded, opt-in AI validation against inventoried test infrastructure.

Credentials stay in the process environment and private artifact directory.
This runner never changes deploy/.env or stops an existing service.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
HISTORICAL = ROOT / "artifacts/ai-split-20261116"


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")
    path.chmod(0o600)


def environment():
    env = os.environ.copy()
    env.update(json.loads((HISTORICAL / "test-env.json").read_text()))
    for key in ["TEST_STABILITY_DURATION", "TEST_RABBITMQ_CONTAINER",
                "TEST_AI_BROWSER_WEB_ROOT", "TEST_AI_BROWSER_READY_FILE"]:
        env.pop(key, None)
    env["PATH"] = str(HISTORICAL / "parser-venv/bin") + ":" + env["PATH"]
    env.update(GOMAXPROCS="2", GOFLAGS="-p=1", GOTOOLCHAIN="go1.25.13", TEST_AI_SPLIT="true",
               TEST_ETCD_ENDPOINT="127.0.0.1:27490")
    return env


def snapshot(out):
    paths = subprocess.check_output(["git", "ls-files", "-z"], cwd=ROOT).decode().split("\0")
    files = {p: hashlib.sha256((ROOT / p).read_bytes()).hexdigest()
             for p in paths if p and (ROOT / p).is_file()}
    write_json(out / "manifest/source.json", {
        "head": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT).decode().strip(),
        "status": subprocess.check_output(["git", "status", "--porcelain=v1"], cwd=ROOT).decode(),
        "files": files,
        "aggregate_sha256": hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest(),
    })
    rows = []
    for line in (ROOT / "docs/AI_SYSTEM_TEST_PLAN_2026-11-17.md").read_text().splitlines():
        m = re.match(r"\| ([A-HJ]\d{2})/(P[012]) \| (.*?) \| (.*?) \|", line)
        if m:
            rows.append(dict(case_id=m[1], priority=m[2], status="NOT_RUN",
                             operation=m[3], expected=m[4], evidence=[]))
    write_json(out / "manifest/cases.json", rows)
    with (out / "results.jsonl").open("a") as f:
        for row in rows:
            f.write(json.dumps(row, ensure_ascii=False) + "\n")
    for label, cmd in {
        "host": ["uname", "-a"], "memory": ["free", "-m"],
        "disk": ["df", "-h", str(ROOT)], "go": ["go", "version"],
        "node": ["node", "--version"], "python": ["python3", "--version"],
        "compose": ["docker", "compose", "version"],
    }.items():
        r = subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True)
        (out / ("manifest/" + label + ".txt")).write_text(r.stdout + r.stderr)
    containers = json.loads(subprocess.check_output(
        ["docker", "inspect", *subprocess.check_output(["docker", "ps", "-q"]).decode().split()]))
    inventory = []
    for c in containers:
        inventory.append({"name": c["Name"].lstrip("/"), "image": c["Image"],
                          "project": c["Config"]["Labels"].get("com.docker.compose.project"),
                          "state": c["State"]["Status"], "ports": c["NetworkSettings"]["Ports"],
                          "mounts": [{k: m.get(k) for k in ["Type", "Name", "Source", "Destination"]}
                                     for m in c["Mounts"]],
                          "env_keys": sorted(x.split("=", 1)[0] for x in c["Config"]["Env"])})
    write_json(out / "resource-inventory.json", {
        "preexisting": inventory, "owned": [],
        "limits": {"minimum_available_memory_mib": 512, "minimum_free_disk_gib": 2,
                   "GOMAXPROCS": 2, "build_parallelism": 1,
                   "fault_seconds": 15, "recovery_seconds": 180},
        "excluded": ["long stability", "soak", "capacity certification"]})


def phase_snapshot(out, phase):
    # Include newly added tests and runners, which HEAD alone cannot identify.
    paths = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"],
        cwd=ROOT).decode().split("\0")
    files = {p: hashlib.sha256((ROOT / p).read_bytes()).hexdigest()
             for p in sorted(set(paths)) if p and (ROOT / p).is_file()
             and "__pycache__" not in Path(p).parts}
    write_json(out / ("manifest/" + phase + "-source.json"), {
        "head": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT).decode().strip(),
        "files": files,
        "aggregate_sha256": hashlib.sha256(json.dumps(files, sort_keys=True).encode()).hexdigest(),
        "deploy_env_sha256": hashlib.sha256((ROOT / "deploy/.env").read_bytes()).hexdigest(),
    })


def run(out, phase, command, cwd, env, timeout=900):
    original = phase
    attempt = 1
    while (out / (phase + ".log")).exists():
        attempt += 1
        phase = original + "-attempt-" + str(attempt)
    log = out / (phase + ".log")
    phase_snapshot(out, phase)
    start = time.monotonic()
    with log.open("w") as f:
        try:
            result = subprocess.run(command, cwd=cwd, env=env, stdout=f,
                                    stderr=subprocess.STDOUT, timeout=timeout)
            rc = result.returncode
        except subprocess.TimeoutExpired:
            rc = 124
    log.chmod(0o600)
    summary = {"phase": phase, "returncode": rc,
               "elapsed_seconds": round(time.monotonic() - start, 3), "log": str(log.relative_to(out))}
    summary["command"] = command
    if command[0] == "go":
        summary["go_version"] = subprocess.check_output(
            ["go", "version"], cwd=cwd, env=env).decode().strip()
    if "-json" in command:
        tests, packages, top_levels = {}, {}, {}
        for line in log.read_text(errors="replace").splitlines():
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if row.get("Action") in ("pass", "fail", "skip"):
                target = tests if row.get("Test") else packages
                target[row["Package"] + ("/" + row["Test"] if row.get("Test") else "")] = row["Action"]
                if row.get("Test") and "/" not in row["Test"]:
                    top_levels[row["Package"] + "/" + row["Test"]] = row["Action"]
        summary.update(tests=tests, packages=packages)
        summary["counts"] = {s: sum(v == s for v in tests.values()) for s in ["pass", "fail", "skip"]}
        summary["top_level_counts"] = {s: sum(v == s for v in top_levels.values()) for s in ["pass", "fail", "skip"]}
        if original == "fullchain":
            expected = "github.com/11bronx11/bx_yunpan/backend/internal/bootstrap/TestAISplitRealDependencies"
            if tests.get(expected) != "pass":
                summary["returncode"] = rc = 1
                summary["error"] = "Required top-level test did not PASS"
    write_json(out / (phase + "-summary.json"), summary)
    print(json.dumps({k: v for k, v in summary.items() if k not in ["tests", "packages"]}), flush=True)
    return rc


def main():
    p = argparse.ArgumentParser()
    p.add_argument("phase", choices=["init", "fullchain", "baseline", "targeted", "security", "race", "all", "build-vet", "parser", "web"])
    p.add_argument("--out", type=Path)
    args = p.parse_args()
    os.umask(0o077)
    if args.phase == "init":
        stamp = datetime.datetime.now(datetime.timezone(datetime.timedelta(hours=8))).strftime("%Y%m%dT%H%M%S%z")
        out = ROOT / "artifacts" / ("ai-deep-validation-" + stamp)
        for sub in ["manifest", "backend", "browser", "faults", "migration", "reconciliation"]:
            (out / sub).mkdir(parents=True, exist_ok=True)
        snapshot(out)
        (ROOT / "artifacts/ai-deep-current.txt").write_text(str(out))
        print(str(out))
        return
    out = args.out or Path((ROOT / "artifacts/ai-deep-current.txt").read_text())
    env = environment()
    backend = ROOT / "backend"
    commands = {
        "fullchain": [["go", "test", "-json", "-p=1", "./internal/bootstrap", "-run", "^TestAISplitRealDependencies$", "-count=1", "-timeout", "8m"]],
        "baseline": [["go", "test", "-json", "-p=1", "./internal/aisystem", "./internal/aigateway", "./internal/asyncjobs", "./internal/ai/rag", "-count=1", "-timeout", "8m"]],
        "targeted": [["go", "test", "-json", "-p=1", "./internal/aisystem", "-run", "^Test(Publish|ReceiptFailure|InvalidModel|EvidenceRevalidation|ProjectionCoverage|AICommitterSIGKILL)", "-count=1", "-timeout", "4m"]],
        "security": [["go", "test", "-json", "-p=1", "./internal/platform/serviceauth", "./internal/core", "./internal/aiprotocol", "./internal/platform/config", "./internal/platform/dependencies", "-count=1", "-timeout", "4m"]],
        "race": [["go", "test", "-race", "-json", "-p=1", "./internal/aisystem", "./internal/platform/serviceauth", "./internal/ai/rag", "-run", "^Test(AcceptedPublishReceiptAndAtomicContinuations|SupersededStagingAndTombstoneNeverBecomeVisible|MaintenanceIdentityAndGraceProtectAcceptedArtifacts|ConcurrentAICommittersPublishOneReceipt|ArtifactGCBatchMakesProgressPastProtectedCandidates|MutualTLSHandshakeMatrix|ParentChildCoverageAndStableRanges)$", "-count=1", "-timeout", "8m"]],
        "all": [["go", "test", "-json", "-p=1", "./...", "-count=1", "-timeout", "8m"]],
        "build-vet": [["go", "build", "-p=1", "./cmd/..."], ["go", "vet", "-p=1", "./..."]],
        "parser": [[str(HISTORICAL / "parser-venv/bin/python3"), "-m", "unittest", "parser_worker.test_parsers", "-v"]],
        "web": [["npm", "run", "lint"], ["npm", "test", "--", "--runInBand"], ["npm", "run", "build"]],
    }
    env["CI"] = "true"
    for i, command in enumerate(commands[args.phase]):
        name = args.phase if len(commands[args.phase]) == 1 else args.phase + "-" + str(i + 1)
        if run(out, name, command, ROOT / "web" if args.phase == "web" else backend, env):
            sys.exit(1)


if __name__ == "__main__":
    main()
