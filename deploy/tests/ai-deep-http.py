#!/usr/bin/env python3
"""HTTP boundaries using synthetic users in the explicit E2 fixture."""
import argparse
import importlib.util
import json
from pathlib import Path
from urllib.parse import urlencode
import uuid


def main():
    p=argparse.ArgumentParser();p.add_argument("--fixture",type=Path,required=True);p.add_argument("--out",type=Path,required=True);args=p.parse_args()
    spec=importlib.util.spec_from_file_location("process",Path(__file__).with_name("ai-deep-process.py"));m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
    fixture=json.loads(args.fixture.read_text());owner=m.Client(fixture["base_url"]);owner.token=fixture["access_token"]
    other=m.Client(fixture["base_url"]);_,auth=other.request("POST","/api/v1/auth/register",{"username":"negative_"+uuid.uuid4().hex[:12],"password":uuid.uuid4().hex},(201,));other.token=auth["access_token"]
    anonymous=m.Client(fixture["base_url"]);fid=fixture["file_ids"][0];checks=[]
    def probe(label,client,method,path,body=None,status=(200,),case="F01"):
        row=dict(case_id=case,probe=label,status="PASS")
        try:
            code,response=client.request(method,path,body,status);row["http_status"]=code
            if code>=400:assert "error" in response and "request_id" in response
        except Exception as e:row.update(status="FAIL",error=str(e))
        checks.append(row)
        return response if row["status"]=="PASS" else None
    _,job=owner.request("POST",f"/api/v1/files/{fid}/ai/reprocess",{"targets":["summary"]},(202,));jid=job["job_id"]
    for method,path,body in [("POST","/api/v1/search",{"query":"rollback","mode":"fulltext"}),("POST","/api/v1/ai/ask",{"question":"rollback"}),("GET","/api/v1/ai/files",None),("GET",f"/api/v1/files/{fid}/ai",None),("POST",f"/api/v1/files/{fid}/ai/reprocess",{"targets":["summary"]}),("GET",f"/api/v1/ai/jobs/{jid}",None)]:
        probe("unauthenticated "+path,anonymous,method,path,body,(401,))
    probe("cross-owner overview",other,"GET",f"/api/v1/files/{fid}/ai",status=(404,))
    probe("cross-owner command",other,"POST",f"/api/v1/files/{fid}/ai/reprocess",{"targets":["summary"]},(404,))
    probe("cross-owner job",other,"GET",f"/api/v1/ai/jobs/{jid}",status=(404,))
    probe("cross-owner selected Ask",other,"POST","/api/v1/ai/ask",{"question":"rollback","scope":{"mode":"selected","file_ids":[fid]}},(409,),"D09")
    for n in [0,51]:probe("selected boundary "+str(n),owner,"POST","/api/v1/ai/ask",{"question":"rollback","scope":{"mode":"selected","file_ids":[str(uuid.uuid4()) for _ in range(n)]}},(400,),"D09")
    for query in ["limit=0","limit=101","sort=invalid","cursor=invalid","folder_id=invalid"]:probe("invalid list "+query,owner,"GET","/api/v1/ai/files?"+query,status=(400,),case="D13")
    page=owner.get("/api/v1/ai/files?limit=1")["page"]
    if page["has_more"]:
        cursor=page["next_cursor"]
        probe("cursor bound to principal",other,"GET","/api/v1/ai/files?"+urlencode({"limit":1,"cursor":cursor}),status=(400,),case="D12")
        probe("cursor bound to filter",owner,"GET","/api/v1/ai/files?"+urlencode({"limit":1,"cursor":cursor,"q":"different"}),status=(400,),case="D12")
    for size in [65535,65536,65537]:
        body={"query":"alpha","mode":"name","padding":""};body["padding"]="x"*(size-len(json.dumps(body).encode()))
        assert len(json.dumps(body).encode())==size
        probe("request bytes "+str(size),owner,"POST","/api/v1/search",body,(200,) if size<=65536 else (400,))
    probe("oversized question",owner,"POST","/api/v1/ai/ask",{"question":"x"*8193},(400,))
    probe("invalid search mode",owner,"POST","/api/v1/search",{"query":"rollback","mode":"invalid"},(400,))
    args.out.mkdir(parents=True,exist_ok=True);(args.out/"http-negative-results.json").write_text(json.dumps(checks,indent=2))
    print(json.dumps({"checks":len(checks),"pass":sum(r["status"]=="PASS" for r in checks),"failures":[r for r in checks if r["status"]!="PASS"]},ensure_ascii=False))
    if any(r["status"]!="PASS" for r in checks):raise SystemExit(1)


if __name__=="__main__":main()
