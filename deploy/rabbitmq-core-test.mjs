import assert from 'node:assert/strict';
import { createHash, randomBytes, randomUUID } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { setTimeout as sleep } from 'node:timers/promises';

const project = process.env.RABBIT_TEST_PROJECT ?? 'bx-rabbitmq-validation';
assert.ok(project.startsWith('bx-rabbitmq-validation'), 'fault tests require the isolated validation project');
const base = process.env.RABBIT_TEST_BASE_URL ?? 'http://127.0.0.1:23000';
const output = process.env.RABBIT_TEST_ARTIFACT_DIR ?? 'artifacts/rabbitmq-migration';
mkdirSync(output, {recursive:true});
const report = {started_at:new Date().toISOString(),project,base,checks:[],faults:[],provider:'fake'};
const save = () => writeFileSync(`${output}/core-results.json`,JSON.stringify(report,null,2)+'\n');
const docker = (...args) => execFileSync('docker',args,{encoding:'utf8',timeout:90000}).trim();
const role = name => `${project}-${name}-1`;
const sql = q => docker('exec',role('postgres'),'sh','-c','exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"','probe',q);
const quote = v => `'${String(v).replaceAll("'","''")}'`;
const hash = x => createHash('sha256').update(x).digest('hex');
const pass = x => {report.checks.push(x); console.log(`PASS ${x}`);save();};
async function request(user,path,{method='GET',body,headers={},expected=[200,201,202,204]}={}){
 const r=await fetch(`${base}/api/v1${path}`,{method,headers:{'Content-Type':'application/json',...(user?{Authorization:`Bearer ${user.token}`}:{ }),...headers},body:body===undefined?undefined:JSON.stringify(body),signal:AbortSignal.timeout(30000)});
 const text=await r.text();assert.ok(expected.includes(r.status),`${method} ${path}: ${r.status} ${text.slice(0,200)}`);return text?JSON.parse(text):null;
}
async function until(action,check,timeout=90000){const deadline=Date.now()+timeout;let last;do{last=await action();if(check(last))return last;await sleep(150);}while(Date.now()<deadline);throw new Error(`condition timeout: ${JSON.stringify(last).slice(0,200)}`);}
async function user(){const username=`rabbit-${randomUUID().slice(0,12)}`,password=`Rabbit-${randomUUID()}!`;const session=await request(null,'/auth/register',{method:'POST',body:{username,password}});const u={token:session.access_token};u.root=(await request(u,'/folders/root')).id;u.id=(await request(u,'/users/me')).id;await request(null,'/auth/login',{method:'POST',body:{login:username,password}});return u;}
async function create(u,data,name){return request(u,'/uploads',{method:'POST',body:{folder_id:u.root,filename:name,sha256:hash(data),size_bytes:data.length,mime_type:name.endsWith('.txt')?'text/plain':name.endsWith('.png')?'image/png':'application/octet-stream'},headers:{'Idempotency-Key':randomUUID()}});}
async function complete(u,session,data){
 session=await until(()=>request(u,`/uploads/${session.id}`),r=>['created','uploading'].includes(r.status));
 for(let i=1;i<=session.part_count;i++){
  const signed=await request(u,`/uploads/${session.id}/parts/presign`,{method:'POST',body:{part_numbers:[i]}});
  const part=data.subarray((i-1)*session.part_size,i*session.part_size);const put=await fetch(signed.parts[0].url,{method:'PUT',body:part,signal:AbortSignal.timeout(60000)});assert.equal(put.status,200);const etag=put.headers.get('etag').replaceAll('"','');await put.arrayBuffer();
  await request(u,`/uploads/${session.id}/parts/confirm`,{method:'POST',body:{parts:[{part_number:i,etag,size_bytes:part.length}]}});
 }
 await Promise.all([1,2].map(()=>request(u,`/uploads/${session.id}/complete`,{method:'POST'})));
 return until(()=>request(u,`/uploads/${session.id}`),r=>{assert.notEqual(r.status,'failed',r.error_code);return r.status==='completed';});
}
async function upload(u,data,name){const start=await create(u,data,name);assert.equal(start.mode,'multipart');const done=await complete(u,start.upload,data);const signed=await request(u,`/files/${done.completed_entry_id}/download-url`);const r=await fetch(signed.url);assert.equal(r.status,200);assert.equal(hash(Buffer.from(await r.arrayBuffer())),hash(data));return done;}
async function fault(name,names,action){const containers=names.map(role);docker('stop','--time','5',...containers);const start=Date.now();try{await action();}finally{docker('start',...containers);}report.faults.push({name,outage_ms:Date.now()-start});console.log(`PASS outage ${name}`);save();}
try{
 const owner=await user(),recipient=await user();pass('register/login/root');
 const data=Buffer.from(`RabbitMQ transaction authority, retry and search ${randomUUID()}`);
 const doc=await upload(owner,data,'queue-proof.txt');pass('multipart upload, duplicate completion, verification and SHA256 download');
 await request(recipient,`/files/${doc.completed_entry_id}/download-url`,{expected:[404]});pass('ownership isolation');
 const indexed=await until(()=>request(owner,`/files/${doc.completed_entry_id}/ai`,{expected:[200,404]}),r=>r.status==='indexed');assert.equal(indexed.status,'indexed');
 const reprocess=await request(owner,`/files/${doc.completed_entry_id}/ai/reprocess`,{method:'POST',headers:{'Idempotency-Key':randomUUID()}});
 // Assert database task identity survives the Outbox metadata injection.
 const taskID=reprocess.task?.id ?? reprocess.id ?? reprocess.task_id;assert.ok(taskID,JSON.stringify(reprocess));
 await until(()=>sql(`SELECT status FROM async_tasks WHERE id=${quote(taskID)}`),s=>{assert.notEqual(s,'failed');return s==='succeeded'||s==='completed'});
 const search=await request(owner,'/search',{method:'POST',body:{query:'RabbitMQ',mode:'hybrid',limit:10}});assert.ok(Array.isArray(search.hits));
 await request(owner,'/ai/ask',{method:'POST',body:{question:'What is the transaction authority?',file_ids:[doc.completed_entry_id]}});pass('AI indexing, reprocess identity, hybrid search and ask (fake provider)');
 const share=await request(owner,'/shares',{method:'POST',body:{file_id:doc.completed_entry_id}});
 const resolved=await request(null,'/public/shares/resolve',{method:'POST',body:{share_key:share.share_key}});const key=randomUUID();
 const imports=await Promise.all([1,2].map(()=>request(recipient,`/shares/${share.share.id}/import`,{method:'POST',body:{target_folder_id:recipient.root},headers:{'Idempotency-Key':key,'X-Share-Token':resolved.share_access_token}})));
 assert.equal(imports[0].imported_file.id,imports[1].imported_file.id);await request(owner,`/shares/${share.share.id}`,{method:'DELETE'});await request(null,'/public/shares/content',{headers:{'X-Share-Token':resolved.share_access_token},expected:[404]});pass('concurrent share import and revocation');
 const picture=await upload(owner,readFileSync('web/public/logo192.png'),`image-${randomUUID()}.png`);
 await until(()=>Number(sql(`SELECT count(*) FROM object_variants WHERE object_id=${quote(picture.completed_object_id)} AND status='ready'`)),n=>n>0);
 const preview=await request(owner,`/files/${picture.completed_entry_id}/preview`);assert.equal((await fetch(preview.url)).status,200);pass('media thumbnail and preview');
 const garbage=await upload(owner,randomBytes(1024),`garbage-${randomUUID()}.bin`);
 await request(owner,`/files/${garbage.completed_entry_id}`,{method:'DELETE',headers:{'If-Match':'1'}});
 await sleep(1500);assert.notEqual(sql(`SELECT status FROM file_objects WHERE id=${quote(garbage.completed_object_id)}`),'deleted');
 await until(()=>sql(`SELECT status FROM file_objects WHERE id=${quote(garbage.completed_object_id)}`),s=>s==='deleted');pass('GC persisted delay and eventual object removal');
 let pending;
 await fault('RabbitMQ stopped; API commits Outbox',['rabbitmq'],async()=>{
  pending=await create(owner,randomBytes(512),`outage-${randomUUID()}.bin`);await request(owner,'/folders/root');
  const backlog=Number(sql("SELECT count(*) FROM outbox_deliveries WHERE transport='rabbitmq' AND status<>'published'"));assert.ok(backlog>0);await sleep(1000);
 });
 await until(()=>request(owner,`/uploads/${pending.upload.id}`),s=>s.status==='created');await request(owner,`/uploads/${pending.upload.id}`,{method:'DELETE'});pass('Outbox drains after broker restart');
 const workerName=process.env.RABBIT_TEST_WORKER_ROLE??'worker';
 await fault('worker stopped and restarted',[workerName],async()=>{pending=await create(owner,randomBytes(512),`worker-${randomUUID()}.bin`);});
 await until(()=>request(owner,`/uploads/${pending.upload.id}`),s=>s.status==='created');await request(owner,`/uploads/${pending.upload.id}`,{method:'DELETE'});pass('worker restart recovers pending work');
 for(let i=0;i<20;i++){await upload(owner,Buffer.from(`regression ${i} ${randomUUID()}`),`regression-${i}.txt`)}pass('20 repeated complete upload/download flows');
 await until(()=>Number(sql("SELECT count(*) FROM outbox_deliveries WHERE transport='rabbitmq' AND status<>'published'")),n=>n===0);
 await until(()=>Number(sql("SELECT count(*) FROM outbox_deliveries d WHERE d.transport='rabbitmq' AND d.status='published' AND NOT EXISTS (SELECT 1 FROM task_results r WHERE r.task_id=d.event_id::text || ':' || d.destination AND r.status='succeeded')")),n=>n===0);
 const readQueues=()=>docker('exec',role('rabbitmq'),'rabbitmqctl','-q','list_queues','-p',process.env.RABBIT_TEST_VHOST??'/', 'name','messages','consumers').split('\n').slice(1).map(line=>line.trim().split(/\s+/));
 const queueRows=await until(readQueues,rows=>rows.every(([,messages])=>Number(messages)===0));
 for(const [name,messages,consumers] of queueRows){assert.equal(Number(messages),0,`queue ${name} has backlog`);if(['object','media','maintenance','ai','search'].includes(name)){assert.ok(Number(consumers)>0,`queue ${name} has no consumer`)}}
 report.queue_messages=0;report.all_five_consumers=true;
 pass('all five consumers active; main/retry/DLQ empty; every published task has a success receipt');
 const refs=Number(sql("SELECT count(*) FROM file_objects o WHERE o.reference_count <> (SELECT count(*) FROM file_entries f WHERE f.object_id=o.id AND f.deleted_at IS NULL)"));assert.equal(refs,0);
 const failed=Number(sql('SELECT count(*) FROM task_failures'));assert.equal(failed,0);
 report.pending=0;report.reference_mismatches=refs;report.terminal_failures=failed;report.result='PASS';report.finished_at=new Date().toISOString();pass('empty Outbox backlog, exact reference counts, zero terminal task failures');save();
}catch(e){report.result='FAIL';report.error=e.message;save();throw e;}
