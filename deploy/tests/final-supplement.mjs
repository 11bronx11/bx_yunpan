import assert from 'node:assert/strict';
import {readFileSync,writeFileSync} from 'node:fs';
import {randomUUID,randomBytes} from 'node:crypto';
import {setTimeout as sleep} from 'node:timers/promises';
process.env.FINAL_HELPER='1';
if(process.argv.includes('--reconcile'))process.env.FINAL_RESULT_NAME='reconciliation-results.json';
const {fixture,report,request,group,until,upload,snapshot,save,output,rows,sql,quote}=await import('./final-acceptance.mjs');
Object.assign(fixture,JSON.parse(readFileSync(`${output}/private-fixture.json`)));
const owner=fixture.users[0],outsider=fixture.users[2],doc=fixture.files.find(x=>x.name==='final-policy.txt');
if(process.argv.includes('--reconcile')){
 await group('complete-owned-domain-convergence',async()=>{
  let s=await until(async()=>{const value=await snapshot('post-soak');console.log('RECONCILE actionable='+value.tasks.reduce((n,t)=>n+Number(t.actionable),0));return value;},x=>x.missing_inbox.length===0&&x.tasks.every(t=>Number(t.actionable)===0),180000);
  for(const row of [...s.business_checks,...s.ai_checks])assert.equal(Number(row.violations),0,row.check_name);
  assert.deepEqual(s.receipts.missing_or_conflicting,[],'missing or conflicting domain receipt');
  for(const q of s.quota){assert.equal(Number(q.session_reserved_bytes),0);assert.equal(Number(q.occupied_bytes),Number(q.actual_logical_bytes));}
  assert.ok(s.sessions.every(x=>['completed','failed','aborted','expired'].includes(x.status)));
  const baseline=JSON.parse(readFileSync(`${output}/db-baseline.json`));
  assert.deepEqual(s.global,baseline.global);
  for(const c of s.containers){const old=baseline.containers.find(x=>x.name===c.name);assert.equal(c.started_at,old.started_at);assert.equal(c.restart_count,old.restart_count);assert.equal(c.oom,false);assert.equal(c.health,'healthy');}
  const unexpected=s.task_errors.filter(t=>t.state==='stopped'&&(!/^ai\./.test(t.kind)||!/quota|payment|insufficient|timeout|network|provider|connection|unavailable/i.test(t.last_error||'')));assert.deepEqual(unexpected,[],'unexpected stopped tasks');
  return {checks:s.business_checks,ai_checks:s.ai_checks,quota:s.quota,events:s.events,tasks:s.tasks,stopped:s.task_errors};
 });
 report.finished_at=new Date().toISOString();writeFileSync(`${output}/reconciliation-results.json`,JSON.stringify(report,null,2),{mode:0o600});
}else{
 await group('AI-reprocess-idempotency-key-job-and-owner-isolation-corrected',async()=>{
  const key=randomUUID(),path=`/files/${doc.id}/ai/reprocess`;
  await request(owner,path,{method:'POST',body:{targets:['summary']},expected:[400]});
  const r=await request(owner,path,{method:'POST',body:{targets:['summary']},headers:{'Idempotency-Key':key},expected:[202]});const job=r.data;
  const replay=await request(owner,path,{method:'POST',body:{targets:['summary']},headers:{'Idempotency-Key':key},expected:[202]});assert.equal(replay.data.job_id,job.job_id);
  await request(outsider,`/ai/jobs/${job.job_id}`,{expected:[404]});
  const terminal=await until(async()=>(await request(owner,`/ai/jobs/${job.job_id}`)).data,j=>['succeeded','failed','partial','stopped','partially_succeeded'].includes(j.status),180000);
  assert.equal(terminal.status,'succeeded',JSON.stringify(terminal));return terminal;
 });
 await group('spike-32-actors-320-requests-bounded-admission-and-recovery',async()=>{
  const start=Date.now(),offset=report.requests.length,controlled=[];
  const outcomes=await Promise.allSettled(Array.from({length:32},async(_,actor)=>{
   for(let n=0;n<10;n++){
    const paths=['/users/me',`/folders/${owner.root}/children?limit=10`,'/users/me/quota','/uploads?limit=10'];
    const r=await request(owner,paths[(n+actor)%4],{expected:[200,503]});
    if(r.status===503){assert.equal(r.data.error.code,'service.overloaded');assert.equal(r.retry_after,'1');controlled.push(r.data.error.code);}
   }
  }));
  assert.deepEqual(outcomes.filter(x=>x.status==='rejected').map(x=>x.reason.message),[]);
  const total=report.requests.length-offset;assert.equal(total,320);
  await sleep(1100);const recoverStart=Date.now();for(let i=0;i<10;i++)await request(owner,`/folders/${owner.root}/children?limit=10`);
  const latencies=report.requests.slice(offset,offset+320).map(x=>x.ms).sort((a,b)=>a-b);
  return {actors:32,total,accepted:total-controlled.length,controlled_503:controlled.length,elapsed_ms:Date.now()-start,recovery_10_success_ms:Date.now()-recoverStart,p95_ms:latencies[Math.floor(latencies.length*.95)],p99_ms:latencies[Math.floor(latencies.length*.99)]};
 });
 await group('8-concurrent-real-upload-download-e2e',async()=>{
  const files=await Promise.all(Array.from({length:8},(_,i)=>upload(owner,randomBytes(32768),`supplement-spike-${i}-${randomUUID().slice(0,5)}.bin`)));
  return {files:files.map(x=>({id:x.id,e2e_ms:x.e2e_ms})),max_e2e_ms:Math.max(...files.map(x=>x.e2e_ms))};
 });
 report.finished_at=new Date().toISOString();save();
}
console.log(JSON.stringify({groups:report.groups.map(x=>({name:x.name,status:x.status,error:x.error})),finished_at:report.finished_at},null,2));
if(report.groups.some(x=>x.status==='FAIL'))process.exitCode=1;
