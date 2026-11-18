#!/usr/bin/env python3
"""Run backend-only parity checks with explicit dedicated test credentials.

This runner never loads deploy/.env, changes running services, or runs web tools.
Provide a private JSON environment file pointing to migrated dedicated databases.
"""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import subprocess
import time

ROOT = Path(__file__).resolve().parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=["parity", "fullchain", "all", "build-vet", "parser", "race", "models"])
    parser.add_argument("--env-file", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    os.chmod(args.out, 0o700)
    env = os.environ.copy()
    env.update(json.loads(args.env_file.read_text()))
    env.update(GOMAXPROCS="2", GOFLAGS="-p=1", GOTOOLCHAIN="go1.25.13")
    commands = {
        "parity": [["go", "test", "-json", "-p=1", "./internal/aisystem", "./internal/platform/grpcx", "./internal/core", "./internal/ai", "./internal/aiprotocol", "./internal/aigateway", "./internal/ai/rag", "-count=1", "-timeout=8m"], ["go", "test", "-json", "-p=1", "./internal/bootstrap", "-run=^TestInternalRPCTraceGovernance$", "-count=1", "-timeout=2m"]],
        "fullchain": [["go", "test", "-json", "-p=1", "./internal/bootstrap", "-run=^TestAISplitRealDependencies$", "-count=1", "-timeout=8m"]],
        "all": [["go", "test", "-json", "-p=1", "./...", "-count=1", "-timeout=8m"]],
        "build-vet": [["go", "build", "-p=1", "./cmd/..."], ["go", "vet", "-p=1", "./..."]],
        "parser": [["python3", "-m", "unittest", "parser_worker.test_parsers", "-v"]],
        "race": [["go", "test", "-race", "-json", "-p=1", "./internal/aisystem", "./internal/platform/grpcx", "./internal/bootstrap", "-run=Test(QueryEmbeddingCache|LogicalAliases|Vision|InternalRPC|SummaryReduces|ShadowRead|ManyLogical)", "-count=1", "-timeout=5m"]],
        "models": [["go", "test", "-json", "-p=1", "./internal/aisystem", "-run=^TestRealModelImagePipeline$", "-count=1", "-timeout=5m"]],
    }
    tracked = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=ROOT).decode().split("\0")
    manifest = {p: hashlib.sha256((ROOT / p).read_bytes()).hexdigest()
                for p in tracked if p and (ROOT / p).is_file() and "__pycache__" not in Path(p).parts}
    (args.out / (args.phase + "-source.json")).write_text(json.dumps(manifest, sort_keys=True))
    for index, command in enumerate(commands[args.phase]):
        label = args.phase if len(commands[args.phase]) == 1 else args.phase + "-" + str(index + 1)
        log = args.out / (label + ".log")
        start = time.monotonic()
        with log.open("w") as output:
            result = subprocess.run(command, cwd=ROOT / "backend", env=env, stdout=output, stderr=subprocess.STDOUT)
        summary = {"phase": label, "command": command, "returncode": result.returncode,
                   "elapsed_seconds": round(time.monotonic() - start, 2), "log": log.name}
        tests = {}
        for line in log.read_text(errors="replace").splitlines():
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if row.get("Test") and row.get("Action") in ["pass", "fail", "skip"]:
                tests[row["Package"] + "/" + row["Test"]] = row["Action"]
        summary["tests"] = tests
        summary["counts"] = {s: sum(v == s for v in tests.values()) for s in ["pass", "fail", "skip"]}
        if command[0] == "python3":
            match = re.search(r"Ran (\d+) tests?", log.read_text())
            if match and result.returncode == 0:
                summary["counts"]["pass"] = int(match.group(1))
        (args.out / (label + "-summary.json")).write_text(json.dumps(summary, indent=2) + "\n")
        print(json.dumps({k: v for k, v in summary.items() if k != "tests"}), flush=True)
        if result.returncode:
            raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
