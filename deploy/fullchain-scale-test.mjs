import assert from 'node:assert/strict';
import {createHash, randomBytes, randomUUID} from 'node:crypto';
import {execFileSync, spawn} from 'node:child_process';
import {mkdirSync, readFileSync, writeFileSync} from 'node:fs';
import {resolve} from 'node:path';
import {setTimeout as sleep} from 'node:timers/promises';
import {deflateSync} from 'node:zlib';

const project = process.env.FULLCHAIN_PROJECT ?? 'bx-fullchain-20261106';
const base = process.env.FULLCHAIN_BASE ?? 'http://127.0.0.1:24000';
const output = resolve(process.env.FULLCHAIN_SCALE_OUTPUT ?? 'artifacts/scaling-20261106');
const envFile = resolve(process.env.FULLCHAIN_ENV_FILE ?? 'deploy/.env.fullchain-20261106');
assert.match(project, /^bx-fullchain-/);
assert.equal(new URL(base).hostname, '127.0.0.1');
mkdirSync(output, {recursive: true});
const report = {project, base, started_at: new Date().toISOString(), groups: [], requests: [], transitions: [], resources: [], events: []};
const save = () => writeFileSync(`${output}/scaling-results.json`, JSON.stringify(report, null, 2) + '\n');
const docker = (...args) => execFileSync('docker', args, {encoding: 'utf8', timeout: 120000}).trim();
const ids = role => docker('ps', '-q', '--no-trunc', '--filter', `label=com.docker.compose.project=${project}`,
  ...(role ? ['--filter', `label=com.docker.compose.service=${role}`] : [])).split('\n').filter(Boolean);
const inspect = list => list.length ? JSON.parse(docker('inspect', ...list)) : [];
const roleOf = c => c.Config.Labels['com.docker.compose.service'];
const settings = c => Object.fromEntries(c.Config.Env.map(v => {const n = v.indexOf('='); return [v.slice(0, n), v.slice(n + 1)];}));
const ipOf = c => Object.values(c.NetworkSettings.Networks)[0].IPAddress;
const sql = query => docker('exec', `${project}-postgres-1`, 'sh', '-c',
  'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"', 'probe', query);
const quote = value => `'${String(value).replaceAll("'", "''")}'`;
const hash = value => createHash('sha256').update(value).digest('hex');
const baseline = inspect(ids());
const scalable = ['api', 'ai-query', 'ai-indexer', 'parser-svc', 'upload-worker', 'object-worker', 'media-worker', 'maintenance-worker', 'search-indexer', 'outbox-relay'];
const counts = Object.fromEntries(scalable.map(role => [role, baseline.filter(c => roleOf(c) === role).length]));
assert.ok(scalable.every(role => counts[role] > 0));
assert.equal(ids('worker').length, 0); assert.equal(ids('aisvc').length, 0);
const pgMax = Number(sql('SHOW max_connections'));
const pgReserved = Number(sql('SHOW superuser_reserved_connections'));
const baselineBudget = baseline.reduce((n, c) => n + (scalable.includes(roleOf(c)) ? Number(settings(c).POSTGRES_MAX_OPEN_CONNS || 0) : 0), 0);
const relayListeners = counts['outbox-relay'];
report.baseline = {counts, containers: baseline.map(c => ({id: c.Id, role: roleOf(c), image: c.Config.Image, image_id: c.Image})), pg_max_connections: pgMax, pool_limit_sum: baselineBudget, relay_listeners: relayListeners};
const compose = ['compose', '--project-name', project, '--env-file', envFile,
  ...['compose.yaml', 'compose.staging.yaml', 'compose.fullchain.yaml', 'compose.sharing.yaml'].flatMap(f => ['-f', resolve('deploy', f)]),
  ...['app', 'distributed', 'ai-split', 'parser', 'redis-split', 'search', 'search-external', 'observability'].flatMap(p => ['--profile', p])];
const queueFor = {'upload-worker': 'upload', 'object-worker': 'object', 'media-worker': 'media', 'maintenance-worker': 'maintenance', 'search-indexer': 'search', 'ai-indexer': 'ai'};
const privateEnv = Object.fromEntries(readFileSync(envFile, 'utf8').split('\n').filter(l => l && !l.startsWith('#') && l.includes('=')).map(l => {const n=l.indexOf('='); return [l.slice(0,n),l.slice(n+1)];}));
const brokerAuth = 'Basic ' + Buffer.from(`${privateEnv.RABBITMQ_USER}:${privateEnv.RABBITMQ_PASSWORD}`).toString('base64');
let stopping = false, probeStop = false, phase = 'setup', probeOwner, proof;
const fixtures = [];
const persist = () => writeFileSync(`${output}/.env.fixtures.json`, JSON.stringify(fixtures), {mode: 0o600});

async function until(action, check, timeout = 120000) {
  const deadline = Date.now() + timeout; let value, lastError;
  do {
    try { value = await action(); if (check(value)) return value; lastError = undefined; }
    catch (error) { lastError = error; }
    await sleep(200);
  } while (Date.now() < deadline);
  throw new Error(`state timeout: ${lastError?.message ?? JSON.stringify(value)?.slice(0, 250)}`);
}
async function request(user, path, {method='GET', body, headers={}, endpoint=base, expected=[200,201,202,204], quiet=false}={}) {
  const started = Date.now();
  const r = await fetch(`${endpoint}/api/v1${path}`, {method,
    headers: {'Content-Type':'application/json', ...(user ? {Authorization:`Bearer ${user.token}`} : {}), ...headers},
    body:body === undefined ? undefined : JSON.stringify(body), signal:AbortSignal.timeout(150000)});
  const text = await r.text(); let data;
  try {data=text ? JSON.parse(text) : null;} catch {data={text:text.slice(0,100)};}
  if (!quiet || !expected.includes(r.status)) report.requests.push({phase, method, path:path.split('?')[0], status:r.status, code:data?.error?.code, ms:Date.now()-started});
  assert.ok(expected.includes(r.status), `${method} ${path.split('?')[0]}: ${r.status} ${data?.error?.code ?? ''}`);
  return data;
}
async function register() {
  const account = {username:`scale-${randomUUID().slice(0,12)}`,password:`Scale-${randomUUID()}!`};
  const r=await request(null,'/auth/register',{method:'POST',body:account});
  const user={...account,id:r.user.id,token:r.access_token};
  user.root=(await request(user,'/folders/root')).id;
  fixtures.push(user); persist(); return user;
}
async function upload(user, bytes, filename, mime='text/plain') {
  const body={folder_id:user.root,filename,sha256:hash(bytes),size_bytes:bytes.length,mime_type:mime};
  const result=await request(user,'/uploads',{method:'POST',body,headers:{'Idempotency-Key':randomUUID()}});
  let file;
  if (result.mode==='instant') file=result.file;
  else {
    const id=result.upload.id;
    await until(()=>request(user,`/uploads/${id}`,{quiet:true}), s=>['created','uploading','failed'].includes(s.status));
    const part=(await request(user,`/uploads/${id}/parts/presign`,{method:'POST',body:{part_numbers:[1]}})).parts[0];
    const r=await fetch(part.url,{method:'PUT',body:bytes,signal:AbortSignal.timeout(60000)});
    assert.equal(r.status,200); const etag=r.headers.get('etag').replaceAll('"',''); await r.arrayBuffer();
    await request(user,`/uploads/${id}/parts/confirm`,{method:'POST',body:{parts:[{part_number:1,etag,size_bytes:bytes.length}]}});
    await request(user,`/uploads/${id}/complete`,{method:'POST'});
    file=await until(()=>request(user,`/uploads/${id}`,{quiet:true}),s=>['completed','failed','aborted'].includes(s.status));
    assert.equal(file.status,'completed',file.error_code);
  }
  return {id:file.completed_entry_id ?? file.id,object:file.completed_object_id ?? file.object_id,bytes,filename};
}
async function download(user, file) {
  const signed=await request(user,`/files/${file.id}/download-url`);
  const r=await fetch(signed.url,{signal:AbortSignal.timeout(60000)}); assert.equal(r.status,200);
  assert.equal(hash(Buffer.from(await r.arrayBuffer())),hash(file.bytes));
}
async function indexed(user, files) {
  await Promise.all(files.map(async file=>{
    const r=await until(()=>request(user,`/files/${file.id}/ai`,{quiet:true,expected:[200,404]}),s=>['indexed','failed','unsupported'].includes(s?.status),240000);
    assert.equal(r.status,'indexed',`${file.filename}: ${r.error_code}`);
  }));
}
async function metrics(c) {
  const port=roleOf(c)==='api' ? 8081 : 8083;
  const r=await fetch(`http://${ipOf(c)}:${port}/metrics`,{signal:AbortSignal.timeout(5000)});
  assert.ok(r.ok); return r.text();
}
function metric(text,name,labels={}) {
  return text.split('\n').filter(l=>l.startsWith(name+'{') || l.startsWith(name+' '))
    .filter(l=>Object.entries(labels).every(([k,v])=>l.includes(`${k}="${v}"`)))
    .reduce((sum,l)=>sum+Number(l.slice(l.lastIndexOf(' ')+1)),0);
}
function registrations(prefix) {
  const r=JSON.parse(docker('exec',`${project}-etcd-1`,'etcdctl','get',prefix,'--prefix','-w','json'));
  return (r.kvs ?? []).map(k=>({key:Buffer.from(k.key,'base64').toString(),address:Buffer.from(k.value,'base64').toString()}));
}
async function broker(path,body) {
  const r=await fetch('http://127.0.0.1:26673/api/'+path,{method:body ? 'POST':'GET',headers:{Authorization:brokerAuth,'Content-Type':'application/json'},body:body ? JSON.stringify(body):undefined,signal:AbortSignal.timeout(10000)});
  assert.ok(r.ok,`broker ${r.status}`); return r.json();
}
async function ping(queue,n=18) {
  const tasks=[];
  for(let i=0;i<n;i++) {
    const id='scale-'+randomUUID(); tasks.push(id);
    const r=await broker('exchanges/%2F/amq.default/publish',{routing_key:queue,payload:'{}',payload_encoding:'string',properties:{message_id:id,type:'system:ping',delivery_mode:2}});
    assert.equal(r.routed,true);
  }
  await until(()=>Number(sql(`SELECT count(*) FROM task_results WHERE task_id IN (${tasks.map(quote).join(',')}) AND status='succeeded'`)),n=>n===tasks.length);
  return tasks.length;
}
async function ready(role,n) {
  await until(async()=>{
    const cs=inspect(ids(role)); if(cs.length!==n) return false;
    for(const c of cs) {
      if(!c.State.Running || c.State.OOMKilled || (c.State.Health && c.State.Health.Status!=='healthy')) return false;
      try {await metrics(c);} catch {return false;}
    }
    return true;
  },Boolean);
  if(role==='ai-query' || role==='parser-svc') {
    const prefix=role==='ai-query' ? '/services/aisvc/' : '/services/document-parser/';
    await until(()=>registrations(prefix),r=>r.length===n);
  }
  if(queueFor[role]) await until(()=>broker('queues/%2F/'+queueFor[role]),q=>q.consumers===n);
}
async function scale(role,n) {
  const old=ids(role).length;
  const pool=Number(settings(baseline.find(c=>roleOf(c)===role)).POSTGRES_MAX_OPEN_CONNS || 0);
  const projected=baselineBudget+(n-counts[role])*pool+relayListeners+(role==='outbox-relay' ? n-counts[role] : 0);
  assert.ok(projected+pgReserved+5<=pgMax,`connection allocation too large: ${projected}`);
  const started=Date.now();
  report.transitions.push({role,from:old,to:n,started_at:new Date().toISOString(),pool_plus_listener_limit:projected}); save();
  await new Promise((resolveRun,reject)=>{
    const child=spawn('docker',[...compose,'up','-d','--no-build','--no-deps','--wait','--wait-timeout','120','--scale',`${role}=${n}`,role],{stdio:['ignore','pipe','pipe']});
    let outputText=''; child.stdout.on('data',d=>outputText+=d); child.stderr.on('data',d=>outputText+=d);
    child.on('error',reject); child.on('exit',code=>{
      writeFileSync(`${output}/${role}-${old}-to-${n}-${started}.log`,outputText);
      code===0 ? resolveRun() : reject(new Error(`scale ${role} to ${n}: exit ${code}`));
    });
  });
  await ready(role,n);
  report.transitions.at(-1).ms=Date.now()-started; save();
}
function png() {
  const crc=buf=>{let c=0xffffffff;for(const b of buf){c^=b;for(let n=0;n<8;n++) c=(c>>>1)^((c&1)?0xedb88320:0);}return(c^0xffffffff)>>>0;};
  const chunk=(kind,data)=>{const type=Buffer.from(kind),head=Buffer.alloc(4),tail=Buffer.alloc(4);head.writeUInt32BE(data.length);tail.writeUInt32BE(crc(Buffer.concat([type,data])));return Buffer.concat([head,type,data,tail]);};
  const header=Buffer.alloc(13);header.writeUInt32BE(32,0);header.writeUInt32BE(32,4);header[8]=8;header[9]=2;
  const pixels=Buffer.concat(Array.from({length:32},()=>Buffer.concat([Buffer.from([0]),randomBytes(96)])));
  return Buffer.concat([Buffer.from([137,80,78,71,13,10,26,10]),chunk('IHDR',header),chunk('IDAT',deflateSync(pixels)),chunk('IEND',Buffer.alloc(0))]);
}
async function workload(user,label,n=3,images=false) {
  return Promise.all(Array.from({length:n},(_,i)=>upload(user,images ? png() : Buffer.from(`扩缩容验收 ${label} ${randomUUID()}。恢复口令 SCALE941。文件应始终可检索、下载和导入。`),`${label}-${i}-${randomUUID().slice(0,6)}.${images ? 'png':'txt'}`,images ? 'image/png':'text/plain')));
}
async function checkFiles(user,files,images=false) {
  await Promise.all(files.map(f=>download(user,f)));
  if(images) await Promise.all(files.map(f=>until(()=>Number(sql(`SELECT count(*) FROM object_variants WHERE object_id=${quote(f.object)} AND status='ready'`)),n=>n>0)));
  else await indexed(user,files);
}
async function probeLoop() {
  while(!probeStop) {
    const started=Date.now();
    try {
      await request(probeOwner,'/folders/root',{quiet:true});
      const list=await request(probeOwner,'/shares',{quiet:true}); assert.ok(list.items.some(s=>s.id===proof.share));
      const r=await fetch(base+proof.url,{signal:AbortSignal.timeout(10000)}); assert.equal(r.status,200);
      assert.equal(hash(Buffer.from(await r.arrayBuffer())),proof.hash);
      report.probes.passed++;
    } catch(error) {report.probes.errors.push({phase,message:error.message});}
    report.probes.samples.push({phase,ms:Date.now()-started});
    await sleep(350);
  }
}
async function resourcesLoop() {
  while(!probeStop) {
    try {
      const memory=Object.fromEntries(readFileSync('/proc/meminfo','utf8').split('\n').filter(l=>l.includes(':')).map(l=>[l.split(':')[0],Number(l.split(':')[1].trim().split(' ')[0])]));
      report.resources.push({phase,time:new Date().toISOString(),pg_connections:Number(sql('SELECT count(*) FROM pg_stat_activity')),available_memory_KiB:memory.MemAvailable,swap_used_KiB:memory.SwapTotal-memory.SwapFree});
    } catch(error) {report.resource_error=error.message;}
    await sleep(1500);
  }
}
async function group(role,fn) {
  const start=Date.now(); phase=role;
  try {const evidence=await fn(); report.groups.push({role,pass:true,ms:Date.now()-start,evidence}); console.log(`PASS scale ${role}`);}
  catch(error) {report.groups.push({role,pass:false,ms:Date.now()-start,error:error.message}); console.log(`FAIL scale ${role}: ${error.message}`); throw error;}
  finally {if(ids(role).length!==counts[role]) await scale(role,counts[role]); save();}
}
let probePromise,resourcePromise;
const eventStream=spawn('docker',['events','--since',report.started_at,'--filter',`label=com.docker.compose.project=${project}`,'--format','{{json .}}'],{stdio:['ignore','pipe','ignore']});
let eventBuffer='';
eventStream.stdout.on('data',d=>{
  eventBuffer+=d;
  const lines=eventBuffer.split('\n'); eventBuffer=lines.pop();
  for(const line of lines) {try{const e=JSON.parse(line);if(['die','oom','stop','destroy','start','kill'].includes(e.Action))report.events.push({action:e.Action,id:e.Actor.ID,attributes:e.Actor.Attributes,time:e.time});}catch{}}
});
process.once('SIGTERM',()=>{stopping=true;}); process.once('SIGINT',()=>{stopping=true;});
try {
  probeOwner=await register();
  const file=await upload(probeOwner,Buffer.from(`稳定读取探针 ${randomUUID()} SCALE941`),'scale-read-proof.txt');
  await indexed(probeOwner,[file]);
  const share=await request(probeOwner,'/shares',{method:'POST',body:{file_id:file.id}});
  const access=await request(null,'/public/shares/resolve',{method:'POST',body:{share_key:share.share_key}});
  const content=await request(null,'/public/shares/content',{headers:{'X-Share-Token':access.share_access_token}});
  proof={share:share.share.id,url:content.download.url,hash:hash(file.bytes)};
  report.probes={passed:0,errors:[],samples:[]};
  probePromise=probeLoop(); resourcePromise=resourcesLoop();
  const selected=process.argv.slice(2); const roles=selected.length ? selected : scalable;
  assert.ok(roles.every(r=>scalable.includes(r)));
  for(const role of roles) {
    if(stopping) throw new Error('interrupted; restoring baseline');
    await group(role,async()=>{
      const user=await register();
      const prior=ids(role); await scale(role,counts[role]+1);
      const current=inspect(ids(role)), added=current.find(c=>!prior.includes(c.Id)); assert.ok(added);
      const evidence={baseline:counts[role],expanded:current.length,added:added.Name.slice(1)};
      if(role==='api') {
        const endpoint='http://'+docker('port',added.Id,'8081/tcp');
        assert.equal((await request(user,'/users/me',{endpoint})).id,user.id);
        const created=await request(user,'/folders',{endpoint,method:'POST',body:{parent_id:user.root,name:'new-replica-write'}});
        const visible=await request(user,`/folders/${user.root}/children`);
        assert.ok(visible.items.some(folder=>folder.id===created.id)); evidence.new_replica_cross_device_write=true;
      }
      if(queueFor[role]) {
        evidence.ping_receipts=await ping(queueFor[role]);
        evidence.new_replica_ping=metric(await metrics(added),'yunpan_worker_tasks_total',{task_type:'system:ping',result:'success'});
        assert.ok(evidence.new_replica_ping>0,'new replica must actually consume tasks');
      }
      const images=role==='media-worker';
      const files=await workload(user,role+'-expanded',3,images);
      await checkFiles(user,files,images);
      if(role==='ai-query') {
        const api=inspect(ids('api'))[0],endpoint='http://'+docker('port',api.Id,'8081/tcp');
        for(let n=0;n<8;n++) {
          const found=await request(user,'/search',{endpoint,method:'POST',body:{query:'扩缩容验收',mode:'fulltext',file_ids:files.map(f=>f.id),limit:5}});
          assert.ok(found.hits.length>0);
        }
        for(let n=0;n<2;n++) {
          const answer=await request(user,'/ai/ask',{endpoint,method:'POST',body:{question:'扩缩容验收文档的恢复口令是什么？',file_ids:[files[0].id]}});
          assert.ok(answer.answer.includes('SCALE941')); assert.ok(answer.citations.length>0);
        }
        evidence.new_query_search_calls=metric(await metrics(added),'yunpan_grpc_server_handled_total',{method:'/yunpan.ai.v1.AIService/Search',code:'OK'});
        evidence.new_query_ask_calls=metric(await metrics(added),'yunpan_grpc_server_handled_total',{method:'/yunpan.ai.v1.AIService/Ask',code:'OK'});
        assert.ok(evidence.new_query_search_calls>0 && evidence.new_query_ask_calls>0);
        let outstanding=4;
        const calls=Array.from({length:4},()=>request(user,'/ai/ask',{endpoint,method:'POST',body:{question:'请从文档确认恢复口令，并用一句话回答。',file_ids:[files[0].id]}}).finally(()=>outstanding--));
        const completed=Promise.all(calls); completed.catch(()=>{});
        await sleep(150); evidence.outstanding_asks_at_shrink=outstanding;
        await scale(role,counts[role]);
        for(const answer of await completed) {assert.ok(answer.answer.includes('SCALE941'));assert.ok(answer.citations.length>0);}
        evidence.completed_asks_during_shrink=4;
      } else {
        if(role==='parser-svc') {
          evidence.new_parser_calls=metric(await metrics(added),'yunpan_grpc_server_handled_total',{method:'/yunpan.ai.v1.DocumentParserService/ParseDocument',code:'OK'});
          assert.ok(evidence.new_parser_calls>0,'new parser must handle real documents');
        }
        const pending=workload(user,role+'-shrinking',counts[role]+2,images); pending.catch(()=>{});
        if(role==='ai-indexer') {
          evidence.inflight_at_shrink=await until(async()=>metric(await metrics(added),'yunpan_worker_inflight_tasks'),n=>n>0,30000);
        }
        await scale(role,1);
        const after=await pending; await checkFiles(user,after,images);
        evidence.completed_files_during_shrink=after.length;
      }
      const post=await workload(user,role+'-contracted',1,images); await checkFiles(user,post,images);
      if(queueFor[role]) evidence.post_shrink_ping_receipts=await ping(queueFor[role],6);
      await scale(role,counts[role]);
      evidence.restored=ids(role).length; return evidence;
    });
  }
  report.result='PASS';
} catch(error) {report.result='FAIL'; report.error=error.message; console.error(error.stack);}
finally {
  phase='restoring';
  for(const role of scalable) {
    if(ids(role).length!==counts[role]) {
      try {await scale(role,counts[role]);} catch(error) {report.restore_error=error.message;report.result='FAIL';}
    }
  }
  probeStop=true; await Promise.allSettled([probePromise,resourcePromise].filter(Boolean));
  eventStream.kill('SIGTERM');
  report.final=inspect(ids()).map(c=>({role:roleOf(c),id:c.Id,image:c.Config.Image,image_id:c.Image,health:c.State.Health?.Status,running:c.State.Running,oom:c.State.OOMKilled}));
  try {
    assert.equal(report.final.length,baseline.length);
    for(const c of report.final) {assert.ok(c.running && !c.oom);assert.ok(c.health===undefined || c.health==='healthy');assert.equal(c.image_id,baseline.find(b=>roleOf(b)===c.role).Image);}
    assert.equal(report.probes.errors.length,0,JSON.stringify(report.probes.errors));
    assert.ok(!report.events.some(e=>e.action==='oom'),'container OOM');
    assert.ok(!report.events.some(e=>e.action==='die' && e.attributes.exitCode==='137'),'forced container termination');
    assert.ok(scalable.every(role=>ids(role).length===counts[role]));
    assert.ok(baseline.filter(c=>!scalable.includes(roleOf(c))).every(c=>report.final.some(f=>f.id===c.Id)),'unrelated container changed');
  } catch(error) {report.result='FAIL'; report.final_error=error.message;}
  report.finished_at=new Date().toISOString(); save();
}
console.log(`scaling: ${report.result} (${report.groups.filter(g=>g.pass).length}/${report.groups.length}), continuous probes=${report.probes?.passed}, errors=${report.probes?.errors.length}`);
process.exitCode=report.result==='PASS' ? 0:1;
