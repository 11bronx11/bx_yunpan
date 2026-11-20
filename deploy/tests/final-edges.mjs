import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {randomBytes,randomUUID} from 'node:crypto';
import {setTimeout as sleep} from 'node:timers/promises';
process.env.FINAL_HELPER='1';process.env.FINAL_RESULT_NAME=process.argv.includes('--gc-only')?'edges-gc-corrected-results.json':'edges-results.json';
const {fixture,report,request,group,until,create,parts,finish,upload,download,rows,quote,save,output}=await import('./final-acceptance.mjs');
Object.assign(fixture,JSON.parse(readFileSync(`${output}/private-fixture.json`)));
const owner=fixture.users[0],doc=fixture.files.find(x=>x.name==='final-policy.txt');
if(!process.argv.includes('--gc-only')){
await group('4-parallel-same-content-uploads-exactly-one-owner-entry',async()=>{
 const bytes=randomBytes(24000);
 const starts=await Promise.all(Array.from({length:4},(_,n)=>create(owner,bytes,`race-content-${n}-${randomUUID().slice(0,5)}.bin`)));
 const done=await Promise.all(starts.map(s=>finish(owner,s.upload,bytes)));
 assert.equal(done.filter(s=>s.status==='completed').length,1);
 assert.ok(done.filter(s=>s.status==='failed').every(s=>s.error_code==='upload.file_exists'));
 const winner=done.find(s=>s.status==='completed');await download(owner,winner.completed_entry_id,bytes);
 return {sessions:done.map(s=>({id:s.id,status:s.status,error_code:s.error_code})),winner:winner.completed_entry_id,object_id:winner.completed_object_id};
});
await group('complete-abort-races-3-terminal-and-no-reservation-leak',async()=>{
 const outcomes=[];
 for(let n=0;n<3;n++){
  const bytes=randomBytes(2000),s=await create(owner,bytes,`complete-abort-final-${n}-${randomUUID().slice(0,5)}.bin`);
  const sent=await parts(owner,s.upload,bytes);
  await request(owner,`/uploads/${s.upload.id}/parts/confirm?view=delta`,{method:'POST',body:{parts:sent.confirmed}});
  const result=await Promise.all([request(owner,`/uploads/${s.upload.id}/complete`,{method:'POST',expected:[202,409]}),request(owner,`/uploads/${s.upload.id}`,{method:'DELETE',expected:[204,409]})]);
  const done=await until(async()=>(await request(owner,`/uploads/${s.upload.id}`)).data,s=>['completed','failed','aborted'].includes(s.status));
  assert.ok(['completed','aborted'].includes(done.status),done.error_code);outcomes.push({id:s.upload.id,status:done.status,requests:result.map(r=>r.status)});
 }
 return outcomes;
});
await group('actual-share-expiry-closes-old-token',async()=>{
 const share=(await request(owner,'/shares',{method:'POST',body:{file_id:doc.id,expires_at:new Date(Date.now()+2500).toISOString()},expected:[201]})).data;
 const resolved=(await request(null,'/public/shares/resolve',{method:'POST',body:{share_key:share.share_key}})).data;
 const headers={'X-Share-Token':resolved.share_access_token};await request(null,'/public/shares/content',{headers});await sleep(2600);
 await request(null,'/public/shares/content',{headers,expected:[404]});await request(null,'/public/shares/resolve',{method:'POST',body:{share_key:share.share_key},expected:[404]});
 return {expired_access:404,waited_ms:2600};
});
}
await group('last-reference-delete-respects-configured-GC-delay',async()=>{
 const file=await upload(owner,randomBytes(4096),'last-ref-'+randomUUID().slice(0,5)+'.bin');
 const meta=(await request(owner,`/files/${file.id}`)).data;
 await request(owner,`/files/${file.id}`,{method:'DELETE',headers:{'If-Match':String(meta.version)},expected:[204]});
 await request(owner,`/files/${file.id}/download-url`,{expected:[404]});
 const objects=await rows('postgres',`SELECT id,status,reference_count,gc_eligible_at FROM file_objects WHERE id=${quote(file.object)}`);
 assert.equal(Number(objects[0].reference_count),0);
 assert.ok(Date.parse(objects[0].gc_eligible_at)+24*3600000>Date.now());
 const tasks=await until(()=>rows('async-postgres',`SELECT kind,state,not_before FROM tasks WHERE kind='object.authorize_delete' AND input::text LIKE ${quote('%'+file.object+'%')}`),xs=>xs.length>0);
 assert.ok(Date.parse(tasks[0].not_before)>=Date.parse(objects[0].gc_eligible_at)+24*3600000-1000);
 return {...objects[0],tasks};
});
report.finished_at=new Date().toISOString();save();console.log(JSON.stringify(report.groups.map(x=>({name:x.name,status:x.status,error:x.error})),null,2));
if(report.groups.some(x=>x.status==='FAIL'))process.exitCode=1;
