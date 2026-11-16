#!/usr/bin/env python3
"""Short single-dependency drills, restricted to an explicit isolated project.

An independent watchdog restores the stopped container within 15 seconds even
if this runner crashes. New synthetic accounts and files are retained for review.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid
from urllib.error import HTTPError
from urllib.request import Request, urlopen


class Client:
    def __init__(self, base): self.base, self.token = base, ""

    def request(self, method, path, body=None, allowed=(200,), headers=None):
        raw = None if body is None else json.dumps(body).encode()
        h = {"Content-Type": "application/json", "Authorization": "Bearer " + self.token,
             "Idempotency-Key": str(uuid.uuid4())}
        h.update(headers or {})
        try: response = urlopen(Request(self.base + path, raw, h, method=method), timeout=12)
        except HTTPError as e: response = e
        with response:
            data = response.read()
            if response.status not in allowed:
                raise AssertionError(f"{method} {path}: status={response.status}, expected={allowed}")
            return response.status, json.loads(data) if data else None

    def get(self, path): return self.request("GET", path)[1]


def wait(label, fn, seconds=180):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        value = fn()
        if value: return value
        time.sleep(.3)
    raise AssertionError("bounded recovery timeout: " + label)


def prepare_upload(client, root, name, data):
    _, r = client.request("POST", "/api/v1/uploads", {"folder_id": root, "filename": name,
        "sha256": hashlib.sha256(data).hexdigest(), "size_bytes": len(data), "mime_type": "text/plain"}, (201,))
    session = r["upload"]
    sid = session["id"]
    wait("multipart create", lambda: client.get("/api/v1/uploads/" + sid)["status"] == "created")
    _, r = client.request("POST", f"/api/v1/uploads/{sid}/parts/presign", {"part_numbers": [1]})
    with urlopen(Request(r["parts"][0]["url"], data, method="PUT"), timeout=15) as response:
        assert response.status == 200
        etag = response.headers["ETag"].strip('"')
    client.request("POST", f"/api/v1/uploads/{sid}/parts/confirm", {"parts": [{"part_number": 1, "etag": etag, "size_bytes": len(data)}]})
    return sid


def upload(client, root, name, data):
    sid = prepare_upload(client, root, name, data)
    client.request("POST", f"/api/v1/uploads/{sid}/complete", allowed=(202,))
    session = wait("upload commit", lambda: (lambda s: s if s["status"] == "completed" else None)(client.get("/api/v1/uploads/" + sid)))
    return session["completed_entry_id"]


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--project", required=True)
    p.add_argument("--base", required=True)
    p.add_argument("--out", type=Path, required=True)
    p.add_argument("--mode", choices=["smoke", "faults"], default="smoke")
    p.add_argument("--roles", default="ai-query,ai-postgres,async-postgres,rabbitmq,core-internal,etcd")
    args = p.parse_args()
    if not args.project.startswith("bx-ai-") or "127.0.0.1:" not in args.base:
        raise SystemExit("requires explicitly named local isolated AI project")
    os.umask(0o077)
    args.out.mkdir(parents=True, exist_ok=True)
    ids = subprocess.check_output(["docker", "ps", "-q", "--filter", "label=com.docker.compose.project=" + args.project]).decode().split()
    containers = json.loads(subprocess.check_output(["docker", "inspect", *ids]))
    roles = {c["Config"]["Labels"]["com.docker.compose.service"]: c for c in containers}
    for role in ["postgres", "async-postgres", "ai-postgres", "rabbitmq", "web", "ai-query"]:
        if role not in roles: raise SystemExit("project missing required service: " + role)
    client = Client(args.base)
    suffix = uuid.uuid4().hex[:12]
    username, password = "deep_" + suffix, uuid.uuid4().hex
    _, auth = client.request("POST", "/api/v1/auth/register", {"username": username, "password": password}, (201,))
    client.token = auth["access_token"]
    root = client.get("/api/v1/folders/root")["id"]
    checks = []
    def record(case_id, probe, status="PASS", **extra):
        row = dict(case_id=case_id, probe=probe, status=status, **extra)
        checks.append(row)
        with (args.out / "process-results.jsonl").open("a") as f: f.write(json.dumps(row, ensure_ascii=False) + "\n")
        print(json.dumps(row, ensure_ascii=False), flush=True)
    alpha_data = ("alpha rollback 保留数据，核验引用和权限。\n" * 4 + suffix).encode()
    a = upload(client, root, "alpha.txt", alpha_data)
    b = upload(client, root, "beta.txt", ("beta rollback 回滚保留历史快照。\n" * 3 + suffix).encode())
    for fid in [a, b]:
        wait("all independent capabilities", lambda: all(client.get(f"/api/v1/files/{fid}/ai")["capabilities"][k]["available"] for k in ["source", "text", "vector", "summary"]))
    record("T0", "real Nginx multipart/source/text/vector/summary", file_ids=[a,b])
    for mode, field, backend in [("fulltext", "text_backend", "elasticsearch"), ("semantic", "vector_backend", "qdrant")]:
        def projected():
            _, r = client.request("POST", "/api/v1/search", {"query": "rollback", "mode": mode})
            return r["retrieval"][field] == backend and r["hits"]
        wait("projection " + backend, projected)
        record("D01", "real " + backend + " hit")
    _, answer = client.request("POST", "/api/v1/ai/ask", {"question": "rollback 策略", "scope": {"mode": "selected", "file_ids": [a,b]}})
    assert answer["citations"]
    for c in answer["citations"]:
        assert c["file_id"] in [a,b] and all(c.get(k) for k in ["source_version_id", "content_version_id", "parent_id", "child_id"])
    record("D14", "scoped Ask real versioned references")
    fixture = dict(base_url=args.base, username=username, password=password, access_token=client.token, root=root, file_ids=[a,b])
    (args.out / "browser-fixture.json").write_text(json.dumps(fixture))
    (args.out / "browser-fixture.json").chmod(0o600)
    if args.mode == "smoke": return
    for role, case_id in [("ai-query","C01"),("ai-postgres","C01"),("ai-workers","C01"),("async-postgres","C02"),("rabbitmq","C03"),("core-internal","C04"),("etcd","C07")]:
        if role not in args.roles.split(","): continue
        stopped_roles = [role] if role != "ai-workers" else ["ai-content-worker", "ai-embedding-worker", "ai-summary-worker", "ai-text-projector", "ai-vector-projector", "ai-committer"]
        containers_to_stop = [roles[name]["Id"] for name in stopped_roles]
        completing = None
        if role in ["async-postgres", "rabbitmq"]:
            completing = prepare_upload(client, root, "complete-during-"+role+".txt", ("complete "+suffix+role).encode())
        # Watchdog has no credentials and outlives this process if necessary.
        watchdog = subprocess.Popen([sys.executable, "-c", "import subprocess,time,sys;time.sleep(15);subprocess.run(['docker','start',*sys.argv[1:]],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)", *containers_to_stop], start_new_session=True)
        job = None
        started = time.monotonic()
        try:
            subprocess.run(["docker", "kill", "--signal=KILL", *containers_to_stop], check=True, capture_output=True, timeout=10)
            client.request("POST", "/api/v1/auth/login", {"login":username,"password":password})
            client.get("/readyz")
            assert client.get("/api/v1/folders/root")["id"] == root
            _, names = client.request("POST", "/api/v1/search", {"query": "alpha", "mode": "name"})
            assert names["hits"]
            download = client.get(f"/api/v1/files/{a}/download-url")
            try:
                with urlopen(download["url"], timeout=5) as response:
                    assert response.status == 200 and response.read() == alpha_data
            except Exception:
                raise AssertionError("download bytes unavailable or differ during fault") from None
            if role in ["async-postgres", "rabbitmq"]:
                _, job = client.request("POST", f"/api/v1/files/{a}/ai/reprocess", {"targets":["summary"]}, (202,))
                _, created = client.request("POST", "/api/v1/uploads", {"folder_id":root,"filename":"during-"+role+".txt","sha256":hashlib.sha256((suffix+role).encode()).hexdigest(),"size_bytes":len(suffix+role),"mime_type":"text/plain"}, (201,))
                sid = created["upload"]["id"]
                client.request("POST", f"/api/v1/uploads/{completing}/complete", allowed=(202,))
                record("C02",role+" stopped: upload Create/Complete and reprocess accepted")
            elif role == "ai-workers":
                _, search = client.request("POST", "/api/v1/search", {"query":"rollback","mode":"fulltext"})
                assert search["hits"]
                _, job = client.request("POST", f"/api/v1/files/{a}/ai/reprocess", {"targets":["summary"]}, (202,))
                assert client.get("/api/v1/ai/jobs/"+job["job_id"])["status"] in ["pending","running","processing"]
            elif role in ["ai-query", "ai-postgres", "core-internal"]:
                code, _ = client.request("POST", "/api/v1/search", {"query":"rollback","mode":"fulltext"}, (503,504))
                record(case_id, role+" content fails within deadline", http_status=code)
            record(case_id, role+" stopped: core readiness/folder/name/download remain available")
        except Exception as e:
            record(case_id, role+" fault probes", "FAIL", error=str(e))
        finally:
            subprocess.run(["docker", "start", *containers_to_stop], check=True, capture_output=True, timeout=30)
            watchdog.terminate()
            watchdog.wait(timeout=5)
        def recovered():
            try: return bool(client.get("/api/v1/ai/files").get("items"))
            except Exception: return False
        try:
            wait(role+" recovery",recovered)
            if job:
                wait("durable user job", lambda: client.get("/api/v1/ai/jobs/"+job["job_id"])["status"]=="succeeded")
                if role in ["async-postgres", "rabbitmq"]:
                    wait("durable upload intent", lambda: client.get("/api/v1/uploads/"+sid)["status"]=="created")
                    wait("durable upload complete", lambda: client.get("/api/v1/uploads/"+completing)["status"]=="completed")
            record(case_id,role+" restored and durable work converges",elapsed_seconds=round(time.monotonic()-started,3))
        except Exception as e: record(case_id,role+" recovery","FAIL",error=str(e))
    if any(x["status"]=="FAIL" for x in checks): raise SystemExit(1)


if __name__ == "__main__": main()
