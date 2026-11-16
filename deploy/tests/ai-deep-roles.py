#!/usr/bin/env python3
"""Read-only inventory and rollback-only privilege probes on an explicit project."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import uuid


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--project", required=True)
    p.add_argument("--out", type=Path, required=True)
    args = p.parse_args()
    os.umask(0o077)
    ids = subprocess.check_output(["docker", "ps", "-q", "--filter", "label=com.docker.compose.project=" + args.project]).decode().split()
    if not ids:
        raise SystemExit("explicit project has no running containers")
    containers = json.loads(subprocess.check_output(["docker", "inspect", *ids]))
    roles = {c["Config"]["Labels"]["com.docker.compose.service"]: c for c in containers}
    results = []
    inventory = []
    for role, c in roles.items():
        env = dict(x.split("=", 1) for x in c["Config"]["Env"] if "=" in x)
        nonempty = sorted(k for k, v in env.items() if v)
        inventory.append({"role": role, "name": c["Name"], "image": c["Image"], "nonempty_env_keys": nonempty})
        forbidden = []
        if role == "api":
            forbidden = ["ASYNC_CONTROL_DSN", "RABBITMQ_URL", "AI_QUERY_DSN", "AI_COMPUTE_DSN", "AI_COMMIT_DSN", "AI_MIGRATION_DSN"]
        elif role.startswith("ai-"):
            forbidden = ["POSTGRES_DSN", "POSTGRES_PRIMARY_DSN", "POSTGRES_READONLY_DSN", "AI_MIGRATION_DSN"]
            if role == "ai-query": forbidden += ["ASYNC_CONTROL_DSN", "RABBITMQ_URL"]
        elif role == "parser-svc":
            forbidden = [k for k in nonempty if "DSN" in k or k == "RABBITMQ_URL"]
        if forbidden:
            present = [k for k in forbidden if env.get(k)]
            results.append({"case_id": "A01", "probe": role + " credentials", "status": "FAIL" if present else "PASS", "forbidden_nonempty_keys": present})

    def sql(service, user, statement, allowed, case, label):
        c = roles[service]
        env = dict(x.split("=", 1) for x in c["Config"]["Env"] if "=" in x)
        database = env["POSTGRES_DB"]
        query = "\\set VERBOSITY sqlstate\nSET default_transaction_read_only=off;\nBEGIN;\n" + statement + ";\nROLLBACK;\n"
        r = subprocess.run(["docker", "exec", "-i", c["Id"], "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", database], input=query, text=True, capture_output=True, timeout=20)
        # An arbitrary SQL failure is not evidence of authorization enforcement.
        ok = r.returncode == 0 if allowed else r.returncode != 0 and "42501" in r.stderr
        results.append({"case_id": case, "probe": label, "role": user, "status": "PASS" if ok else "FAIL", "returncode": r.returncode, "sqlstate_permission_denied": "42501" in r.stderr})

    sql("ai-postgres", "yunpan_ai_query", "SELECT * FROM ai_published_objects LIMIT 0", True, "A02", "query published view")
    for label, statement in {
        "query staging SELECT": "SELECT * FROM ai_artifact_manifests LIMIT 0",
        "query private base SELECT": "SELECT * FROM ai_objects LIMIT 0",
        "query base UPDATE": "UPDATE ai_objects SET desired_source_seq=desired_source_seq WHERE false",
        "query view UPDATE": "UPDATE ai_published_objects SET desired_source_seq=desired_source_seq WHERE false",
        "query DDL": "CREATE TABLE public.ai_deep_privilege_probe(id int)",
    }.items(): sql("ai-postgres", "yunpan_ai_query", statement, False, "A02", label)
    aid, task, obj, output = [str(uuid.uuid4()) for _ in range(4)]
    insert = f"INSERT INTO ai_artifact_manifests(artifact_id,task_id,generation,kind,input_digest,artifact_hash,typed_output_ref,row_count,byte_count,complete,data,object_id,content_generation) VALUES('{aid}','{task}',1,'ai.source.prepare','{'a'*64}','{'b'*64}','{output}',0,2,true,'{{}}','{obj}',1)"
    sql("ai-postgres", "yunpan_ai_compute", insert, True, "A03", "compute insert staging")
    sql("ai-postgres", "yunpan_ai_compute", "SELECT * FROM ai_published_objects LIMIT 0; SELECT * FROM ai_projection_catalog LIMIT 0", True, "A03", "compute published view/catalog")
    for label, statement in {
        "compute staging SELECT": "SELECT * FROM ai_artifact_manifests LIMIT 0",
        "compute staging UPDATE": "UPDATE ai_artifact_manifests SET complete=false WHERE false",
        "compute staging DELETE": "DELETE FROM ai_artifact_manifests WHERE false",
        "compute active UPDATE": "UPDATE ai_objects SET active_source_id=NULL WHERE false",
        "compute receipt DELETE": "DELETE FROM ai_commit_receipts WHERE false",
        "compute DDL": "CREATE TABLE public.ai_deep_privilege_probe(id int)",
    }.items(): sql("ai-postgres", "yunpan_ai_compute", statement, False, "A03", label)
    sql("ai-postgres", "yunpan_ai_commit", "SELECT * FROM ai_commit_receipts LIMIT 0; UPDATE ai_objects SET desired_source_seq=desired_source_seq WHERE false", True, "A03", "committer publication privileges")
    core = roles["core-internal"]
    from urllib.parse import urlparse
    core_env = dict(x.split("=", 1) for x in core["Config"]["Env"] if "=" in x)
    reader = urlparse(core_env["POSTGRES_READONLY_DSN"]).username
    sql("postgres", reader, "SELECT * FROM file_objects LIMIT 0", True, "A04", "core business read")
    sql("postgres", reader, "UPDATE file_objects SET reference_count=reference_count WHERE false", False, "A04", "core business write")
    sql("postgres", reader, "CREATE TABLE public.ai_deep_privilege_probe(id int)", False, "A04", "core DDL")
    identities = []
    for service in ["postgres", "async-postgres", "ai-postgres"]:
        c = roles[service]
        env = dict(x.split("=", 1) for x in c["Config"]["Env"] if "=" in x)
        r = subprocess.run(["docker", "exec", c["Id"], "psql", "-X", "-At", "-U", env["POSTGRES_USER"], "-d", env["POSTGRES_DB"], "-c", "SELECT current_database(),current_user,(SELECT system_identifier FROM pg_control_system()),version,dirty FROM schema_migrations;"], capture_output=True, text=True, timeout=20)
        if r.returncode: raise SystemExit("database identity probe failed: " + service)
        identities.append({"service": service, "identity": r.stdout.strip()})
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / "role-probes.json").write_text(json.dumps({"project": args.project, "environment": inventory, "database_identities": identities, "probes": results}, indent=2))
    print(json.dumps({"probes": len(results), "pass": sum(r["status"] == "PASS" for r in results), "fail": sum(r["status"] == "FAIL" for r in results)}))
    if any(r["status"] == "FAIL" for r in results): raise SystemExit(1)


if __name__ == "__main__": main()
