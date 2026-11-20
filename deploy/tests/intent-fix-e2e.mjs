// All inputs use the deployed HTTP API. SQL assertions are read-only.
import assert from 'node:assert/strict';
import {randomBytes,randomUUID} from 'node:crypto';
import {writeFileSync} from 'node:fs';
process.env.FINAL_HELPER='1';process.env.FINAL_RESULT_NAME='intent-fix-e2e.json';
const {fixture,report,request,group,until,create,parts,download,rows,quote,save,output,snapshot}=await import('./final-acceptance.mjs');
const username='intentfix_'+randomUUID().slice(0,12),password=randomUUID()+'Aa1!';
const auth=(await request(null,'/auth/register',{method:'POST',body:{username,password},expected:[201]})).data;
const owner={id:auth.user.id,username,password,token:auth.access_token};owner.root=(await request(owner,'/folders/root')).data.id;
fixture.users.push(owner);
writeFileSync(`${output}/private-fixture.json`,JSON.stringify(fixture,null,2),{mode:0o600});
await group('same-owner-content-races-settle-all-intents',async()=>{
 const rounds=[];
 for(let n=0;n<4;n++){
  const bytes=randomBytes(32768);
  const starts=await Promise.all(Array.from({length:4},(_,i)=>create(owner,bytes,`round-${n}-${i}.bin`)));
  await Promise.all(starts.map(async x=>{
   const sent=await parts(owner,x.upload,bytes);
   await request(owner,`/uploads/${x.upload.id}/parts/confirm?view=delta`,{method:'POST',body:{parts:sent.confirmed}});
  }));
  await Promise.all(starts.map(x=>request(owner,`/uploads/${x.upload.id}/complete`,{method:'POST',expected:[202]})));
  const done=await Promise.all(starts.map(x=>until(async()=>(await request(owner,`/uploads/${x.upload.id}`)).data,s=>['completed','failed'].includes(s.status))));
  const winners=done.filter(s=>s.status==='completed'),losers=done.filter(s=>s.status==='failed');
  assert.equal(winners.length,1);assert.equal(losers.length,3);assert.ok(losers.every(s=>s.error_code==='upload.commit_precondition_failed'));
  await download(owner,winners[0].completed_entry_id,bytes);
  const list=starts.map(x=>quote(x.upload.id)).join(',');
  const operations=await rows('postgres',`SELECT o.id,o.operation_type,o.state,o.last_error,o.completed_at,s.id AS session_id,s.status AS session_status,s.error_code,s.reserved_bytes FROM storage_operations o JOIN upload_sessions s ON s.id=o.upload_session_id WHERE o.upload_session_id IN (${list}) AND o.operation_type='canonical.ensure' ORDER BY s.filename`);
  for(const o of operations){
   assert.ok(o.completed_at);assert.equal(o.reserved_bytes,0);
   assert.equal(o.state,o.session_status==='completed'?'succeeded':'failed');
   if(o.state==='failed')assert.equal(o.last_error,'upload.commit_precondition_failed');
  }
  rounds.push({round:n,sessions:done.map(s=>({id:s.id,status:s.status,error_code:s.error_code})),operations});
 }
 const failedCopies=rounds.flatMap(r=>r.operations).filter(o=>o.state==='failed');
 assert.ok(failedCopies.length>=3,'race did not exercise losing canonical copy commit');
 return {rounds,failed_canonical_intents:failedCopies.length};
});
await group('final-receipts-quota-and-no-overdue-control-work',async()=>{
 const s=await until(async()=>snapshot('intent-fix-final'),s=>s.missing_inbox.length===0&&s.tasks.every(t=>Number(t.actionable)===0),90000);
 assert.deepEqual(s.receipts.missing_or_conflicting,[]);assert.deepEqual(s.task_errors,[]);
 for(const c of [...s.business_checks,...s.ai_checks])assert.equal(Number(c.violations),0,c.check_name);
 for(const q of s.quota){assert.equal(Number(q.session_reserved_bytes),0);assert.equal(Number(q.occupied_bytes),Number(q.actual_logical_bytes));}
 const unfinished=await rows('postgres',`SELECT o.id,o.state,o.operation_type FROM storage_operations o JOIN upload_sessions s ON s.id=o.upload_session_id WHERE s.user_id=${quote(owner.id)} AND s.status IN ('completed','failed','aborted','expired') AND o.operation_type IN ('canonical.ensure','multipart.create','multipart.complete') AND o.state IN ('pending','running')`);
 assert.deepEqual(unfinished,[]);return {quota:s.quota,receipts:s.receipts,events:s.events,operations:s.operations};
});
report.status=report.groups.every(g=>g.status==='PASS')?'PASS':'FAIL';report.finished_at=new Date().toISOString();save();
console.log(JSON.stringify({status:report.status,groups:report.groups.map(g=>({name:g.name,status:g.status,error:g.error}))}));
if(report.status!=='PASS')process.exitCode=1;
