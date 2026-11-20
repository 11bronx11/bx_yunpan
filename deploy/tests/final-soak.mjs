import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {randomBytes,randomUUID} from 'node:crypto';
import {setTimeout as sleep} from 'node:timers/promises';
process.env.FINAL_HELPER='1';process.env.FINAL_RESULT_NAME='soak-results.json';
const {fixture,report,request,upload,sample,save,output}=await import('./final-acceptance.mjs');
Object.assign(fixture,JSON.parse(readFileSync(`${output}/private-fixture.json`)));
const owner=fixture.users[0],doc=fixture.files.find(x=>x.name==='final-policy.txt');
// Fixtures can be older than access-token TTL. Start with a real login and
// rotate the refresh cookie before expiry, as the browser client does.
const login=await request(null,'/auth/login',{method:'POST',body:{login:owner.username,password:owner.password}});
owner.token=login.data.access_token;owner.cookie=login.cookie;
const started=Date.now(),end=started+600000,counts=Array(6).fill(0),unexpected=[],controlled=[];
report.soak={started_at:new Date().toISOString(),duration_required_ms:600000,reader_actors:6,counts,uploads:0,writes:0,session_rotations:0};save();
const work=[];
for(let actor=0;actor<6;actor++)work.push((async()=>{
 while(Date.now()<end){
  try{
   const paths=[`/folders/${owner.root}/children?limit=10`,'/users/me/quota','/uploads?limit=10','/ai/files?limit=10',`/files/${doc.id}`,`/files/${doc.id}/ai`];
   const r=await request(owner,paths[(counts[actor]+actor)%paths.length],{expected:[200,409,503],timeout:15000});
   if(r.status===409){assert.equal(r.data.error.code,'ai.scope_changed');controlled.push({actor,at:new Date().toISOString(),code:r.data.error.code,path:paths[(counts[actor]+actor)%paths.length]});}
   if(r.status===503){assert.equal(r.data.error.code,'service.overloaded');assert.equal(r.retry_after,'1');controlled.push({actor,at:new Date().toISOString(),path:paths[(counts[actor]+actor)%paths.length]});}
  }catch(error){unexpected.push({actor,at:new Date().toISOString(),error:error.message});}
  counts[actor]++;await sleep(150);
 }
})());
work.push((async()=>{let n=0;while(Date.now()<end){
 try{
  const folder=(await request(owner,'/folders',{method:'POST',body:{parent_id:owner.root,name:`steady-${n}-${randomUUID().slice(0,6)}`},expected:[201]})).data;
  const renamed=(await request(owner,`/folders/${folder.id}`,{method:'PATCH',body:{name:`steady-renamed-${n}`},headers:{'If-Match':String(folder.version)}})).data;
  await request(owner,`/folders/${folder.id}`,{method:'DELETE',headers:{'If-Match':String(renamed.version)},expected:[204]});report.soak.writes+=3;
  await upload(owner,randomBytes(16384),`steady-${n++}-${randomUUID().slice(0,5)}.bin`);report.soak.uploads++;
 }catch(error){unexpected.push({actor:'writer',at:new Date().toISOString(),error:error.message});}
 await sleep(Math.min(30000,Math.max(0,end-Date.now())));
}})());
work.push((async()=>{while(Date.now()<end){
 try{await sample('steady-soak');}catch(error){unexpected.push({actor:'monitor',error:error.message});}
 console.log(`SOAK elapsed=${Math.round((Date.now()-started)/1000)}s counts=${counts.join(',')} uploads=${report.soak.uploads}`);
 await sleep(Math.min(30000,Math.max(0,end-Date.now())));
}})());
work.push((async()=>{let next=Date.now()+240000;while(Date.now()<end){
 await sleep(Math.min(Math.max(0,next-Date.now()),Math.max(0,end-Date.now())));
 if(Date.now()>=end)break;
 try{
  const refreshed=await request(null,'/auth/refresh',{method:'POST',headers:{Cookie:owner.cookie}});
  owner.token=refreshed.data.access_token;owner.cookie=refreshed.cookie;report.soak.session_rotations++;
 }catch(error){unexpected.push({actor:'session-refresh',at:new Date().toISOString(),error:error.message});}
 next=Date.now()+240000;
}})());
await Promise.all(work);
const latencies=report.requests.filter(x=>x.ms!==undefined).map(x=>x.ms).sort((a,b)=>a-b);
Object.assign(report.soak,{finished_at:new Date().toISOString(),elapsed_ms:Date.now()-started,requests:report.requests.length,read_iterations:counts.reduce((a,b)=>a+b,0),rps:report.requests.length/((Date.now()-started)/1000),p95_ms:latencies[Math.floor(latencies.length*.95)],p99_ms:latencies[Math.floor(latencies.length*.99)],controlled_overload:controlled,unexpected});
report.status=unexpected.length||counts.some(n=>n<1000)||report.soak.elapsed_ms<600000?'FAIL':'PASS';
report.finished_at=new Date().toISOString();save();console.log(JSON.stringify({status:report.status,...report.soak,controlled_overload:controlled.length,unexpected:unexpected.slice(0,8)}));
if(report.status!=='PASS')process.exitCode=1;
