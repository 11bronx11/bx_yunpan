#!/usr/bin/env python3
"""Core upload smoke with fresh databases/bucket and owned application processes.

Uses an existing dedicated TEST_* JSON, never deploy/.env. Retains test data;
stops only the processes started here. No fault drills or capacity testing.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import time
from urllib.parse import urlsplit, urlunsplit
from urllib.request import Request, urlopen

spec = importlib.util.spec_from_file_location("process_checks", Path(__file__).with_name("async-process.py"))
core = importlib.util.module_from_spec(spec)
spec.loader.exec_module(core)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--binaries", type=Path, required=True)
    parser.add_argument("--web-root", type=Path, required=True)
    parser.add_argument("--browser-wait", type=int, default=600)
    args = parser.parse_args()
    os.umask(0o077)
    config = json.loads(args.config.read_text())
    guard = core.checks.resource_lock()
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=False)
    suffix = secrets.token_hex(5)
    processes = []
    results = []

    def sql(dsn, query):
        result = core.checks.psql(dsn, query)
        if result.returncode: raise RuntimeError("dedicated test SQL failed; inspect local test services")
        return result.stdout.strip()

    def database(source, name):
        u = urlsplit(source)
        if not any(s in u.path.lower() for s in ("test", "validation", "async_")):
            raise ValueError("requires explicitly named dedicated test databases")
        return urlunsplit((u.scheme, u.netloc, "/" + name, u.query, ""))

    def record(name, **extra):
        results.append(dict(case=name, status="PASS", **extra))
        core.checks.write_json(out / "results.json", results)
        print("PASS " + name, flush=True)

    business, control = "upload_validation_b_" + suffix, "upload_validation_c_" + suffix
    biz = database(config["TEST_POSTGRES_DSN"], business)
    ctrl = database(config["TEST_CONTROL_POSTGRES_DSN"], control)
    reader_dsn = database(config["TEST_POSTGRES_READONLY_DSN"], business)
    for source, name in [(config["TEST_POSTGRES_DSN"], business), (config["TEST_CONTROL_POSTGRES_DSN"], control)]:
        sql(source, 'CREATE DATABASE "' + name + '"')
    bucket = "upload-validation-" + suffix
    storage = core.Minio(config["TEST_MINIO_ENDPOINT"], access_key=config["TEST_MINIO_ACCESS_KEY"], secret_key=config["TEST_MINIO_SECRET_KEY"], secure=False)
    storage.make_bucket(bucket)
    api_port, web_port = core.port(), core.port()
    env = dict(os.environ, POSTGRES_PRIMARY_DSN=biz, POSTGRES_READONLY_DSN=reader_dsn, ASYNC_CONTROL_DSN=ctrl,
        RABBITMQ_URL=config["TEST_RABBITMQ_URL"], ASYNC_BROKER_PREFIX="bx.upload-smoke." + suffix,
        S3_ENDPOINT=config["TEST_MINIO_ENDPOINT"], S3_PUBLIC_ENDPOINT=f"127.0.0.1:{web_port}", S3_PUBLIC_PATH_PREFIX="/storage",
        S3_ACCESS_KEY=config["TEST_MINIO_ACCESS_KEY"], S3_SECRET_KEY=config["TEST_MINIO_SECRET_KEY"], S3_BUCKET=bucket,
        AUTH_SIGNING_SEED=secrets.token_hex(32), SHARE_SECRET=secrets.token_hex(32), APP_ENV="development",
        AI_ENABLED="false", ETCD_ENABLED="false", PARSER_REMOTE_ENABLED="false", SEARCH_ENABLED="false", CACHE_ENABLED="false",
        REDIS_ADDR=config["TEST_REDIS_ADDR"], POSTGRES_MAX_OPEN_CONNS="4", POSTGRES_MAX_IDLE_CONNS="1",
        ASYNC_CONTROL_MAX_OPEN_CONNS="4", ASYNC_CONTROL_MAX_IDLE_CONNS="1", ASYNC_CONTROL_CRITICAL_CONNS="1",
        ASYNC_POLL_MIN="100ms", ASYNC_POLL_MAX="300ms", ASYNC_RETRY_BASE="100ms", ASYNC_RETRY_CAP="1s", GOMAXPROCS="2")
    core.checks.write_json(out / "private-env.json", {k: v for k, v in env.items() if os.environ.get(k) != v})
    core.checks.write_json(out / "inventory.json", dict(business=business, control=control, bucket=bucket, retained=True))
    binaries = args.binaries.resolve()
    try:
        for label, command in [("business-migrate", ["up"]), ("control-migrate", ["control", "up"])]:
            with (out / (label + ".log")).open("w") as log:
                subprocess.run([str(binaries / "migrate"), *command], cwd=core.checks.ROOT / "backend", env=env, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=120)
        reader = urlsplit(reader_dsn).username
        if not reader or not reader.replace("_", "").isalnum(): raise ValueError("invalid dedicated reader")
        sql(biz, f'GRANT CONNECT ON DATABASE "{business}" TO "{reader}"; GRANT USAGE ON SCHEMA public TO "{reader}"; GRANT SELECT ON ALL TABLES IN SCHEMA public TO "{reader}";')
        for role in ["api", "async-control", "committer", "upload-worker", "object-worker"]:
            local = dict(env, SERVICE_INSTANCE_ID="upload-smoke-" + role, WORKER_METRICS_ADDR=f"127.0.0.1:{core.port()}", HTTP_ADDR=f"127.0.0.1:{api_port}")
            with (out / (role + ".log")).open("w") as log:
                processes.append(subprocess.Popen([str(binaries / role)], cwd=core.checks.ROOT / "backend", env=local, stdout=log, stderr=subprocess.STDOUT, start_new_session=True))
        nginx = out / "nginx.conf"
        nginx.write_text(f'''pid {out}/nginx.pid;
error_log {out}/nginx-error.log;
events {{ worker_connections 128; }}
http {{
 include /etc/nginx/mime.types;
 access_log {out}/nginx-access.log;
 client_body_temp_path {out}/client-body;
 proxy_temp_path {out}/proxy-temp;
 server {{ listen 127.0.0.1:{web_port}; root {args.web_root.resolve()}; client_max_body_size 32m;
  location /api/ {{ proxy_pass http://127.0.0.1:{api_port}; proxy_set_header Host $http_host; }}
  location /storage/ {{ proxy_pass http://{config['TEST_MINIO_ENDPOINT']}/; proxy_set_header Host $http_host; proxy_request_buffering off; }}
  location / {{ try_files $uri /index.html; }}
 }}
}}
''')
        with (out / "nginx.log").open("w") as log:
            processes.append(subprocess.Popen(["nginx", "-p", str(out), "-c", str(nginx), "-g", "daemon off;"], stdout=log, stderr=subprocess.STDOUT, start_new_session=True))
        base = f"http://127.0.0.1:{web_port}"

        def ready():
            try: return urlopen(f"http://127.0.0.1:{api_port}/health/ready", timeout=1).status == 200
            except OSError: return False

        core.wait("API readiness", ready)
        client = core.Client(base)
        username, password = "upload_" + suffix, secrets.token_urlsafe(18)
        auth = client.request("POST", "/api/v1/auth/register", dict(username=username, password=password), status=201)
        client.token = auth["access_token"]
        root = client.request("GET", "/api/v1/folders/root")["id"]
        payload = os.urandom(20 * 1024 * 1024 + 123)
        session = client.create(root, "core-smoke.bin", payload)["upload"]
        sid = session["id"]
        core.wait("initialization", lambda: client.request("GET", "/api/v1/uploads/" + sid)["status"] == "created")
        signed = client.request("POST", f"/api/v1/uploads/{sid}/parts/presign", {"part_numbers": [3, 1, 2]})["parts"]

        def put(item):
            number = item["part_number"]
            body = payload[(number - 1) * session["part_size"]:number * session["part_size"]]
            with urlopen(Request(item["url"], body, method="PUT"), timeout=30) as response:
                return dict(part_number=number, etag=response.headers["ETag"].strip('"'), size_bytes=len(body))

        with core.ThreadPoolExecutor(max_workers=3) as pool: parts = list(pool.map(put, signed))
        ack = client.request("POST", f"/api/v1/uploads/{sid}/parts/confirm?view=delta", {"parts": parts})
        assert len(ack["acknowledged_parts"]) == 3 and "confirmed_parts" not in ack
        ack = client.request("POST", f"/api/v1/uploads/{sid}/parts/confirm?view=delta", {"parts": parts[:1]})
        assert len(ack["acknowledged_parts"]) == 1
        conflict = dict(parts[0], etag="conflicting-etag")
        client.request("POST", f"/api/v1/uploads/{sid}/parts/confirm?view=delta", {"parts": [conflict]}, status=409)
        client.request("POST", f"/api/v1/uploads/{sid}/complete", status=202)
        core.wait("completed", lambda: client.request("GET", "/api/v1/uploads/" + sid)["status"] == "completed")
        completed = client.request("GET", "/api/v1/uploads/" + sid)
        client.download(completed["completed_entry_id"], payload)
        client.request("POST", f"/api/v1/uploads/{sid}/parts/confirm?view=delta", {"parts": parts})
        record("core-multipart-delta-idempotency-conflict-download", bytes=len(payload), sha256=hashlib.sha256(payload).hexdigest())
        core.checks.write_json(out / "browser-fixture.json", dict(base_url=base, username=username, password=password))
        print("BROWSER_READY " + str(out), flush=True)
        core.wait("browser smoke", lambda: (out / "browser-complete.json").exists(), args.browser_wait)
        browser = json.loads((out / "browser-complete.json").read_text())
        assert browser["status"] == "PASS", "browser smoke failed"
        record("browser-window-upload-pause-resume-download")
    finally:
        for proc in reversed(processes):
            if proc.poll() is None: os.killpg(proc.pid, signal.SIGTERM)
        for proc in processes:
            try: proc.wait(timeout=15)
            except subprocess.TimeoutExpired: os.killpg(proc.pid, signal.SIGKILL); proc.wait(timeout=5)
        guard.close()


if __name__ == "__main__": main()
