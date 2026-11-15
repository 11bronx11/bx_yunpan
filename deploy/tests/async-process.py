#!/usr/bin/env python3
"""Real binaries + local Nginx smoke/fault checks, using dedicated resources.

Run with a Python environment containing minio==7.2.20. Build commands into
--binaries first (go build -o DIR/ ./cmd/...). No application internals are
called to manufacture work. All user work enters through public HTTP APIs.
Fresh databases and a bucket are retained for review; owned processes stop.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import time
import traceback
from urllib.error import HTTPError
from urllib.parse import urlsplit, urlunsplit
from urllib.request import Request, urlopen

from minio import Minio

spec = importlib.util.spec_from_file_location("checks", Path(__file__).with_name("async-check.py"))
checks = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checks)


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait(label, predicate, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if value := predicate():
            return value
        time.sleep(0.1)
    raise AssertionError("timeout: " + label)


class Client:
    def __init__(self, base):
        self.base, self.token, self.share = base, "", ""

    def request(self, method, path, data=None, status=200, key=None):
        req = Request(self.base + path, data=None if data is None else json.dumps(data).encode(), method=method)
        req.add_header("Content-Type", "application/json")
        req.add_header("If-Match", "1")
        req.add_header("Idempotency-Key", key or secrets.token_hex(16))
        if self.token:
            req.add_header("Authorization", "Bearer " + self.token)
        if self.share:
            req.add_header("X-Share-Token", self.share)
        try:
            response = urlopen(req, timeout=15)
        except HTTPError as error:
            response = error
        body = response.read()
        if response.status != status:
            raise AssertionError(f"{method} {path}: {response.status}, {body[:1000]!r}")
        return json.loads(body) if body else {}

    def create(self, folder, name, data, mime="application/octet-stream", sha=None):
        return self.request("POST", "/api/v1/uploads", {
            "folder_id": folder, "filename": name, "sha256": sha or hashlib.sha256(data).hexdigest(),
            "size_bytes": len(data), "mime_type": mime}, status=201)

    def finish(self, session, data, parts=1, expected="completed", corrupt_etag=False):
        sid = session["id"]
        wait("multipart initialization", lambda: self.request("GET", "/api/v1/uploads/" + sid)["status"] == "created")
        signed = self.request("POST", f"/api/v1/uploads/{sid}/parts/presign", {"part_numbers": list(range(1, parts + 1))})["parts"]
        assert session["part_count"] == parts
        size = session["part_size"]
        confirmations = []
        for i, item in enumerate(sorted(signed, key=lambda item: item["part_number"])):
            payload = data[i * size:(i + 1) * size] if i < parts - 1 else data[i * size:]
            assert urlsplit(item["url"]).path.startswith("/storage/")
            with urlopen(Request(item["url"], data=payload, method="PUT"), timeout=30) as response:
                assert response.status == 200
                confirmations.append({"part_number": i + 1, "etag": response.headers["ETag"].strip('"'), "size_bytes": len(payload)})
        if corrupt_etag:
            confirmations[0]["etag"] = "0" * 32
        self.request("POST", f"/api/v1/uploads/{sid}/parts/confirm", {"parts": list(reversed(confirmations))})
        self.request("POST", f"/api/v1/uploads/{sid}/complete", status=202)
        def done():
            item = self.request("GET", "/api/v1/uploads/" + sid)
            return item if item["status"] == expected else None
        return wait("upload " + expected, done)

    def download(self, file_id, expected):
        signed = self.request("GET", f"/api/v1/files/{file_id}/download-url")["url"]
        assert urlsplit(signed).path.startswith("/storage/")
        with urlopen(signed, timeout=30) as response:
            assert response.status == 200
            downloaded = response.read()
        assert hashlib.sha256(downloaded).digest() == hashlib.sha256(expected).digest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--binaries", type=Path, required=True)
    parser.add_argument("--web-root", type=Path, required=True)
    parser.add_argument("--browser-wait", type=int, default=0)
    args = parser.parse_args()
    os.umask(0o077)
    config = json.loads(args.config.read_text())
    guard = checks.resource_lock()
    checks.preflight(config)
    output, binaries = args.output.resolve(), args.binaries.resolve()
    output.mkdir(parents=True, exist_ok=True)
    checks.write_json(output / "binaries.json", {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in binaries.iterdir() if p.is_file()})
    suffix = secrets.token_hex(5)
    business, control, bucket = "async_validation_b_" + suffix, "async_validation_c_" + suffix, "async-validation-" + suffix
    processes, events, cases = {}, [], []

    def record(event, **details):
        row = {"event": event, "utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "monotonic": time.monotonic(), **details}
        events.append(row)
        with (output / "events.jsonl").open("a") as stream:
            stream.write(json.dumps(row) + "\n")

    def sql(dsn, query):
        result = checks.psql(dsn, query)
        if result.returncode:
            raise AssertionError(result.stderr)
        return result.stdout.strip()

    def db_url(source, name):
        u = urlsplit(source)
        return urlunsplit((u.scheme, u.netloc, "/" + name, u.query, ""))

    def passed(case, **details):
        cases.append({"case": case, "status": "PASS", **details})
        checks.write_json(output / "cases.json", cases)
        print("PASS " + case, flush=True)

    for name in (business, control):
        sql(config["TEST_POSTGRES_DSN"], 'CREATE DATABASE "' + name + '";')
    biz = db_url(config["TEST_POSTGRES_DSN"], business)
    ctrl = db_url(config["TEST_CONTROL_POSTGRES_DSN"], control)
    read = db_url(config["TEST_POSTGRES_READONLY_DSN"], business)
    reader = urlsplit(read).username
    assert reader and reader.replace("_", "").isalnum()
    storage = Minio(config["TEST_MINIO_ENDPOINT"], access_key=config["TEST_MINIO_ACCESS_KEY"], secret_key=config["TEST_MINIO_SECRET_KEY"], secure=False)
    storage.make_bucket(bucket)
    api_ports, web_port = [port(), port()], port()
    env = os.environ.copy()
    env.update(POSTGRES_PRIMARY_DSN=biz, POSTGRES_READONLY_DSN=read, ASYNC_CONTROL_DSN=ctrl,
               RABBITMQ_URL=config["TEST_RABBITMQ_URL"], ASYNC_BROKER_PREFIX="bx.validation." + suffix,
               S3_ENDPOINT=config["TEST_MINIO_ENDPOINT"], S3_PUBLIC_ENDPOINT=f"127.0.0.1:{web_port}",
               S3_PUBLIC_PATH_PREFIX="/storage", S3_ACCESS_KEY=config["TEST_MINIO_ACCESS_KEY"],
               S3_SECRET_KEY=config["TEST_MINIO_SECRET_KEY"], S3_BUCKET=bucket,
               AUTH_SIGNING_SEED=secrets.token_hex(32), SHARE_SECRET=secrets.token_hex(32),
               AI_ENABLED="false", ETCD_ENABLED="false", PARSER_REMOTE_ENABLED="false", SEARCH_ENABLED="false",
               CACHE_ENABLED="true", REDIS_ADDR=config["TEST_REDIS_ADDR"],
               POSTGRES_MAX_OPEN_CONNS="3", POSTGRES_MAX_IDLE_CONNS="1",
               ASYNC_CONTROL_MAX_OPEN_CONNS="3", ASYNC_CONTROL_MAX_IDLE_CONNS="1", ASYNC_CONTROL_CRITICAL_CONNS="1",
               ASYNC_LEASE="3s", ASYNC_POLL_MIN="100ms", ASYNC_POLL_MAX="300ms", ASYNC_RETRY_BASE="100ms", ASYNC_RETRY_CAP="1s",
               OBJECT_GC_DELAY="1s", ASYNC_GC_SCAN_INTERVAL="2s", ASYNC_WRITE_CONCURRENCY="2", ASYNC_WRITE_RATE="32",
               ASYNC_RESULT_HIGH="16", ASYNC_QUEUE_INFLIGHT="4", ASYNC_OWNER_INFLIGHT="2", GOMAXPROCS="2",
               ASYNC_LEGACY_WRITERS_STOPPED="true", LOG_LEVEL="info", APP_ENV="development")
    checks.write_json(output / "private-env.json", {k: v for k, v in env.items() if k in set(env) - set(os.environ) or k in ("GOMAXPROCS",)})
    checks.write_json(output / "inventory.json", {"business": business, "control": control, "bucket": bucket,
                      "broker_prefix": env["ASYNC_BROKER_PREFIX"], "api_ports": api_ports, "web_port": web_port,
                      "processes": "recorded in events.jsonl", "retained": True})

    def command(label, cmd):
        with (output / (label + ".log")).open("w") as stream:
            result = subprocess.run(cmd, cwd=checks.ROOT / "backend", env=env, stdout=stream, stderr=subprocess.STDOUT, timeout=120)
        assert result.returncode == 0, label + " failed; inspect its private log"

    def start(role, name, **overrides):
        local = dict(env, SERVICE_INSTANCE_ID=name, WORKER_METRICS_ADDR=f"127.0.0.1:{port()}", **overrides)
        stream = (output / (name + ".log")).open("a")
        proc = subprocess.Popen([str(binaries / role)], cwd=checks.ROOT / "backend", env=local, stdout=stream, stderr=subprocess.STDOUT, start_new_session=True)
        stream.close()
        processes[name] = proc
        record("start", name=name, role=role, pid=proc.pid)
        return proc

    def send(name, sig):
        proc = processes[name]
        os.killpg(proc.pid, sig)
        record("signal", name=name, signal=signal.Signals(sig).name, pid=proc.pid)

    def csql(query):
        return sql(ctrl, query)

    def bsql(query):
        return sql(biz, query)

    def shards():
        return csql("SELECT holder,count(*) FROM module_leases WHERE name LIKE 'committer:%' AND lease_until>clock_timestamp() GROUP BY holder ORDER BY holder;")

    try:
        for label, args_ in [("business-up", ["up"]), ("business-up-again", ["up"]),
                             ("control-up", ["control", "up"]), ("control-up-again", ["control", "up"])]:
            command(label, [str(binaries / "migrate"), *args_])
        assert bsql("SELECT version,dirty FROM schema_migrations") == "34|f"
        assert csql("SELECT version,dirty FROM schema_migrations") == "2|f"
        bsql(f'GRANT CONNECT ON DATABASE "{business}" TO "{reader}"; GRANT USAGE ON SCHEMA public TO "{reader}"; GRANT SELECT ON ALL TABLES IN SCHEMA public TO "{reader}"; ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO "{reader}";')
        command("empty-import", [str(binaries / "async-import")])
        passed("MG-01-empty-and-idempotent-dual-migrations")
        for i, api_port in enumerate(api_ports):
            start("api", "api-" + str(i), HTTP_ADDR=f"127.0.0.1:{api_port}", ASYNC_CONTROL_DSN="", RABBITMQ_URL="amqp://127.0.0.1:1/")
        nginx = output / "nginx.conf"
        nginx.write_text(f'''pid {output}/nginx.pid;
error_log {output}/nginx-error.log;
events {{ worker_connections 128; }}
http {{
  include /etc/nginx/mime.types;
  access_log {output}/nginx-access.log;
  client_body_temp_path {output}/client-body;
  proxy_temp_path {output}/proxy-temp;
  upstream apis {{ server 127.0.0.1:{api_ports[0]}; server 127.0.0.1:{api_ports[1]}; }}
  server {{
    listen 127.0.0.1:{web_port};
    root {args.web_root.resolve()};
    client_max_body_size 32m;
    location /api/ {{ proxy_pass http://apis; proxy_set_header Host $http_host; }}
    location /storage/ {{ proxy_pass http://{config['TEST_MINIO_ENDPOINT']}/; proxy_set_header Host $http_host; proxy_request_buffering off; }}
    location / {{ try_files $uri /index.html; }}
  }}
}}
''')
        stream = (output / "nginx.log").open("w")
        processes["nginx"] = subprocess.Popen(["nginx", "-p", str(output), "-c", str(nginx), "-g", "daemon off;"], stdout=stream, stderr=subprocess.STDOUT, start_new_session=True)
        stream.close()
        base = f"http://127.0.0.1:{web_port}"
        def api_ready():
            try:
                return urlopen(f"http://127.0.0.1:{api_ports[0]}/health/ready", timeout=1).status == 200
            except OSError:
                return False
        wait("API readiness", api_ready)
        owner = Client(base)
        username, password = "validation_" + suffix, secrets.token_urlsafe(18)
        account = owner.request("POST", "/api/v1/auth/register", {"username": username, "password": password}, status=201)
        owner.token = account["access_token"]
        owner_id = account["user"]["id"]
        root = owner.request("GET", "/api/v1/folders/root")["id"]
        owner.request("POST", "/api/v1/search", {"query": "disabled", "mode": "hybrid"}, status=503)
        payload = b"independent process admission " + os.urandom(64)
        admitted = owner.create(root, "admitted.bin", payload)["upload"]
        assert admitted["status"] == "initializing"
        assert int(bsql(f"SELECT count(*) FROM outbox_events WHERE aggregate_id='{admitted['id']}'")) > 0
        passed("SMK-02-API-admission-with-background-unavailable")
        for role in ["async-control", "committer"]:
            for i in range(2):
                start(role, role + "-" + str(i))
        for role in ["upload-worker", "object-worker", "media-worker", "gc-worker"]:
            start(role, role)
        finished = owner.finish(admitted, payload)
        owner.download(finished["completed_entry_id"], payload)
        passed("SMK-02-independent-roles-upload-and-proxy-download", sha256=hashlib.sha256(payload).hexdigest())
        wait("two shard owners", lambda: len(shards().splitlines()) == 2 and all(x.endswith("|16") for x in shards().splitlines()))
        passed("SH-two-processes-16-shards-each", ownership=shards())
        data = os.urandom(20 * 1024 * 1024 + 333)
        multipart = owner.finish(owner.create(root, "three-parts.bin", data)["upload"], data, parts=3)
        owner.download(multipart["completed_entry_id"], data)
        passed("UP-01-three-parts-reversed-confirmation-and-download", bytes=len(data), sha256=hashlib.sha256(data).hexdigest())
        wait("prior results drained", lambda: csql("SELECT count(*) FROM tasks WHERE state IN ('running','computed')") == "0")
        send("upload-worker", signal.SIGSTOP)
        pending_data = b"persisted result through a killed committer " + os.urandom(100)
        pending = owner.create(root, "kill-window.bin", pending_data)["upload"]
        wait("event accepted before pausing committers", lambda: csql(f"SELECT count(*) FROM tasks WHERE resource_key='session:{pending['id']}' AND kind='multipart.create' AND state='dispatched'") == "1")
        send("committer-0", signal.SIGSTOP)
        send("committer-1", signal.SIGTERM)
        processes["committer-1"].wait(timeout=15)
        send("upload-worker", signal.SIGCONT)
        task_query = f"SELECT id,generation,attempts FROM tasks WHERE resource_key='session:{pending['id']}' AND kind='multipart.create' AND state='computed'"
        before = wait("persisted computed result", lambda: csql(task_query))
        record("computed-barrier", task=before)
        send("committer-0", signal.SIGKILL)
        processes["committer-0"].wait(timeout=5)
        start("committer", "committer-1")
        wait("survivor owns every shard", lambda: shards() == "committer-1|32")
        recovered = owner.finish(pending, pending_data)
        owner.download(recovered["completed_entry_id"], pending_data)
        task_id, generation, attempts = before.split("|")
        assert csql(f"SELECT generation,attempts FROM tasks WHERE id='{task_id}'") == generation + "|" + attempts
        semantic_key = csql(f"SELECT semantic_key FROM tasks WHERE id='{task_id}'")
        assert bsql(f"SELECT count(*) FROM async_commit_receipts WHERE semantic_key='{semantic_key}'") == "1"
        passed("FI-03-SIGKILL-and-SH-takeover-computed-without-recompute", task_id=task_id, generation=generation, attempts=attempts, ownership=shards())
        start("committer", "committer-0")
        wait("committer rejoins", lambda: len(shards().splitlines()) == 2 and all(x.endswith("|16") for x in shards().splitlines()))
        relay_owner = csql("SELECT holder FROM module_leases WHERE name='relay' AND lease_until>clock_timestamp()")
        assert relay_owner in ("async-control-0", "async-control-1"), relay_owner
        send(relay_owner, signal.SIGKILL)
        processes[relay_owner].wait(timeout=5)
        successor = wait("Relay lease takeover", lambda: (x := csql("SELECT holder FROM module_leases WHERE name='relay' AND lease_until>clock_timestamp()")) and x != relay_owner and x)
        probe = owner.create(root, "relay-takeover.bin", b"control takeover")["upload"]
        wait("takeover accepts new event", lambda: owner.request("GET", "/api/v1/uploads/" + probe["id"])["status"] == "created")
        start("async-control", relay_owner)
        owner.request("DELETE", "/api/v1/uploads/" + probe["id"], status=204)
        passed("FI-02-SIGKILL-lease-takeover-and-old-process-restart", old=relay_owner, successor=successor)
        send("upload-worker", signal.SIGSTOP)
        canceled = owner.create(root, "cancel-initializing.bin", b"cancel before create")["upload"]
        owner.request("DELETE", "/api/v1/uploads/" + canceled["id"], status=204)
        send("upload-worker", signal.SIGCONT)
        wait("aborted terminal", lambda: owner.request("GET", "/api/v1/uploads/" + canceled["id"])["status"] == "aborted")
        passed("UP-03-cancel-initializing-and-late-work")
        bad = owner.create(root, "bad-sha.bin", b"wrong SHA", sha="a" * 64)["upload"]
        owner.finish(bad, b"wrong SHA", expected="failed")
        passed("UP-04-hash-rejection")
        invalid = owner.create(root, "wrong-etag.bin", b"invalid ETag boundary")["upload"]
        rejected = owner.finish(invalid, b"invalid ETag boundary", expected="failed", corrupt_etag=True)
        assert rejected["error_code"] == "upload.async_exhausted"
        assert bsql(f"SELECT reserved_bytes FROM upload_sessions WHERE id='{invalid['id']}'") == "0"
        passed("UP-04-long-MinIO-error-exhaustion-and-quota-release", error_code=rejected["error_code"])
        share = owner.request("POST", "/api/v1/shares", {"file_id": finished["completed_entry_id"]}, status=201)
        other = Client(base)
        other_account = other.request("POST", "/api/v1/auth/register", {"username": "borrow_" + suffix, "password": password}, status=201)
        other.token = other_account["access_token"]
        other_root = other.request("GET", "/api/v1/folders/root")["id"]
        other.share = other.request("POST", "/api/v1/public/shares/resolve", {"share_key": share["share_key"]})["share_access_token"]
        imported = other.request("POST", "/api/v1/shares/" + share["share"]["id"] + "/import", {"target_folder_id": other_root}, status=201)["imported_file"]
        oid = finished["completed_object_id"]
        assert bsql(f"SELECT reference_count FROM file_objects WHERE id='{oid}'") == "2"
        owner.request("DELETE", "/api/v1/files/" + finished["completed_entry_id"], status=204)
        other.download(imported["id"], payload)
        other.request("DELETE", "/api/v1/files/" + imported["id"], status=204)
        wait("authorized object GC", lambda: bsql(f"SELECT status FROM file_objects WHERE id='{oid}'") == "deleted")
        passed("GC-02-share-reference-survives-until-last-delete")
        timings = []
        def small_upload(i):
            started = time.monotonic()
            data = os.urandom(256)
            result = owner.finish(owner.create(root, f"short-{i}.bin", data)["upload"], data)
            owner.download(result["completed_entry_id"], data)
            return round(time.monotonic() - started, 3)
        for concurrency in [2, 4]:
            with ThreadPoolExecutor(max_workers=concurrency) as pool:
                values = list(pool.map(small_upload, range(concurrency * 3, concurrency * 3 + 6)))
            timings.append({"concurrency": concurrency, "operations": len(values), "end_to_end_seconds": values})
        checks.write_json(output / "short-concurrency.json", timings)
        passed("short-concurrency-sample", samples=12, levels=[2, 4])
        checks.write_json(output / "browser-fixture.json", {"base_url": base, "username": username, "password": password})
        checks.write_json(output / "frontend-ready.json", {"base_url": base, "expires_in_seconds": args.browser_wait})
        print("BROWSER_READY " + base, flush=True)
        if args.browser_wait:
            wait("browser completion marker", lambda: (output / "browser-complete.json").exists(), timeout=args.browser_wait)
            assert json.loads((output / "browser-complete.json").read_text())["status"] == "PASS"
        # Allow result reports and immediate cleanups to settle; delayed expiry
        # and safety-window cleanup tasks are intentionally not forced due.
        wait("immediate work drained", lambda: csql("SELECT count(*) FROM tasks WHERE state IN ('running','dispatched','computed','convergence_pending') OR (state IN ('ready','retry_wait') AND not_before<=clock_timestamp())") == "0")
        assert csql("SELECT count(*) FROM tasks WHERE state IN ('needs_attention','quarantined','exhausted','convergence_pending')") == "0"
        snapshot = bsql("""BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
SELECT 'quota_mismatches',count(*) FROM user_quota_usage q WHERE q.occupied_bytes<0 OR q.occupied_bytes <> COALESCE((SELECT sum(o.size_bytes) FROM file_entries f JOIN file_objects o ON o.id=f.object_id WHERE f.owner_id=q.owner_id AND f.deleted_at IS NULL),0)+COALESCE((SELECT sum(s.reserved_bytes) FROM upload_sessions s WHERE s.user_id=q.owner_id),0);
SELECT 'reference_mismatches',count(*) FROM file_objects o WHERE o.reference_count <> (SELECT count(*) FROM file_entries f WHERE f.object_id=o.id AND f.deleted_at IS NULL);
SELECT 'bad_completed_sessions',count(*) FROM upload_sessions WHERE status='completed' AND (reserved_bytes<>0 OR completed_entry_id IS NULL OR completed_object_id IS NULL);
COMMIT;""")
        (output / "business-reconcile.txt").write_text(snapshot + "\n")
        assert "quota_mismatches|0" in snapshot and "reference_mismatches|0" in snapshot and "bad_completed_sessions|0" in snapshot, snapshot
        passed("INV-06-07-08-single-snapshot-reconciliation", snapshot=snapshot)
        tasks = json.loads(csql("SELECT COALESCE(json_agg(x),'[]'::json) FROM (SELECT t.semantic_key,t.state,t.input_digest,t.result_id,r.result_digest FROM tasks t LEFT JOIN results r ON r.id=t.result_id) x"))
        receipts = json.loads(bsql("SELECT COALESCE(json_agg(x),'[]'::json) FROM (SELECT semantic_key,input_digest,result_digest,result_id,outcome FROM async_commit_receipts) x"))
        by_task, by_receipt = {t["semantic_key"]: t for t in tasks}, {r["semantic_key"]: r for r in receipts}
        for receipt in receipts:
            task = by_task[receipt["semantic_key"]]
            assert all(task[key] == receipt[key] for key in ("input_digest", "result_digest", "result_id"))
            assert task["state"] == receipt["outcome"]
        terminal = {"succeeded", "superseded", "rejected", "failed"}
        assert all(t["semantic_key"] in by_receipt for t in tasks if t["state"] in terminal)
        checks.write_json(output / "quiescent-cross-db.json", {"tasks": tasks, "receipts": receipts, "mismatches": 0})
        passed("INV-05-quiescent-result-receipt-reconciliation", receipts=len(receipts), mismatches=0)
        checks.write_json(output / "control-summary.json", {"states": csql("SELECT kind,state,count(*),max(attempts) FROM tasks GROUP BY kind,state ORDER BY kind,state"), "quarantine": csql("SELECT count(*) FROM quarantines"), "shards": shards()})
        checks.write_json(output / "summary.json", {"status": "PASS", "cases": len(cases), "browser_marker": (output / "browser-complete.json").exists()})
        return 0
    except Exception as error:
        record("failure", type=type(error).__name__, message=str(error))
        (output / "failure.txt").write_text(traceback.format_exc())
        checks.write_json(output / "summary.json", {"status": "FAIL", "passed_cases": len(cases), "error": str(error)})
        print("FAIL " + str(error), flush=True)
        return 1
    finally:
        for name, proc in reversed(list(processes.items())):
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGCONT)
                os.killpg(proc.pid, signal.SIGTERM)
        for name, proc in processes.items():
            try:
                proc.wait(timeout=15)
            except subprocess.TimeoutExpired:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=5)
            record("stopped", name=name, pid=proc.pid, exit_code=proc.returncode)
        checks.write_json(output / "residual-resources.json", {"processes_stopped": True, "databases_retained": [business, control], "bucket_retained": bucket, "broker_prefix_retained": env["ASYNC_BROKER_PREFIX"]})
        guard.close()


if __name__ == "__main__":
    raise SystemExit(main())
