#!/usr/bin/env python3
"""Bounded correctness probes; reported timings are observations, not capacity."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import importlib.util
import json
from pathlib import Path
import statistics
import time
import uuid


def main():
    p=argparse.ArgumentParser()
    p.add_argument("--fixture",type=Path,required=True)
    p.add_argument("--out",type=Path,required=True)
    args=p.parse_args()
    spec=importlib.util.spec_from_file_location("process",Path(__file__).with_name("ai-deep-process.py"))
    m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
    fixture=json.loads(args.fixture.read_text())
    results=[]
    for concurrency in [1,4,8]:
        started=time.monotonic()
        def read(n):
            client=m.Client(fixture["base_url"]);client.token=fixture["access_token"]
            start=time.monotonic()
            if n%3==0:
                _,r=client.request("POST","/api/v1/search",{"query":"rollback","mode":"fulltext"})
                assert r["hits"]
            elif n%3==1:
                _,r=client.request("POST","/api/v1/ai/ask",{"question":"rollback","scope":{"mode":"selected","file_ids":[fixture["file_ids"][0]]}})
                assert r["citations"] and all(c["file_id"]==fixture["file_ids"][0] for c in r["citations"])
                assert r["retrieval"]["evidence_tokens"]<=12000
            else: assert client.get("/api/v1/ai/files?limit=1")["items"]
            return time.monotonic()-start
        row={"case_id":"H05","concurrency":concurrency,"request_limit":24,"status":"PASS"}
        try:
            with ThreadPoolExecutor(max_workers=concurrency) as pool: times=list(pool.map(read,range(24)))
            row.update(completed=len(times),p50_seconds=round(statistics.median(times),3),p95_seconds=round(sorted(times)[int(.95*(len(times)-1))],3))
        except Exception as e: row.update(status="FAIL",error=str(e))
        row["elapsed_seconds"]=round(time.monotonic()-started,3)
        if row["elapsed_seconds"]>60:row.update(status="FAIL",error="exceeded bounded workload duration")
        results.append(row)
    client=m.Client(fixture["base_url"]);client.token=fixture["access_token"]
    for concurrency in [1,4,8]:
        key=str(uuid.uuid4());fid=fixture["file_ids"][0]
        def rebuild(_):return client.request("POST",f"/api/v1/files/{fid}/ai/reprocess",{"targets":["summary"]},(202,),{"Idempotency-Key":key})[1]
        row={"case_id":"F02","probe":"concurrent duplicate reprocess","concurrency":concurrency,"status":"PASS"}
        try:
            with ThreadPoolExecutor(max_workers=concurrency) as pool: jobs=list(pool.map(rebuild,range(concurrency)))
            ids={j["job_id"] for j in jobs};assert len(ids)==1
            client.request("POST",f"/api/v1/files/{fid}/ai/reprocess",{"targets":["vectors"]},(409,),{"Idempotency-Key":key})
            job_id=ids.pop()
            m.wait("concurrent user job",lambda:client.get("/api/v1/ai/jobs/"+job_id)["status"]=="succeeded")
            row["job_id"]=job_id
        except Exception as e:row.update(status="FAIL",error=str(e))
        results.append(row)
    args.out.mkdir(parents=True,exist_ok=True)
    (args.out/"concurrency-results.json").write_text(json.dumps(results,indent=2))
    print(json.dumps(results,ensure_ascii=False))
    if any(r["status"]=="FAIL" for r in results):raise SystemExit(1)


if __name__=="__main__":main()
