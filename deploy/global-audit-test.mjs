import assert from 'node:assert/strict';
import { createHash, randomBytes, randomUUID } from 'node:crypto';
import { execFileSync, spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { mkdirSync, writeFileSync, appendFileSync, readFileSync } from 'node:fs';
import { performance } from 'node:perf_hooks';
import { setTimeout as sleep } from 'node:timers/promises';

const project = 'bx-yunpan-staging';
const dir = process.env.AUDIT_ARTIFACT_DIR ?? 'artifacts/global-audit/20260911';
const base = 'http://127.0.0.1:3002';
mkdirSync(dir, { recursive: true });
const docker = (...args) => execFileSync('docker', args, { encoding: 'utf8', timeout: 45000 }).trim();
const containers = role => docker('ps', '-q', '--filter', `label=com.docker.compose.project=${project}`, '--filter', `label=com.docker.compose.service=${role}`).split('\n').filter(Boolean);
const apis = containers('api').map(id => `http://${docker('port', id, '8081/tcp')}`);
assert.equal(apis.length, 2);
const sql = query => docker('exec', `${project}-postgres-1`, 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-U', 'hc_staging', '-d', 'hc_staging', '-c', query);
const quote = value => `'${String(value).replaceAll("'", "''")}'`;
const hash = data => createHash('sha256').update(data).digest('hex');
const report = { started_at: new Date().toISOString(), project, apis, checks: [], benchmarks: [], faults: [], methodology: 'Same-host closed-loop HTTP clients, keep-alive fetch, latency includes response body; AI fake provider; rate limit disabled in isolated AI query only.' };
const save = () => writeFileSync(`${dir}/runtime-results.json`, JSON.stringify(report, null, 2) + '\n');
const log = text => { console.log(text); appendFileSync(`${dir}/runtime.log`, text + '\n'); save(); };
async function request(user, path, { method = 'GET', body, headers = {}, endpoint = base, expected = [200, 201, 202, 204], timeout = 10000 } = {}) {
  const r = await fetch(`${endpoint}/api/v1${path}`, { method, signal: AbortSignal.timeout(timeout), headers: { 'Content-Type': 'application/json', ...(user ? { Authorization: `Bearer ${user.token}` } : {}), ...headers }, body: body === undefined ? undefined : JSON.stringify(body) });
  const raw = await r.text();
  assert.ok(expected.includes(r.status), `${method} ${path}: ${r.status} ${raw.slice(0, 250)}`);
  return raw ? JSON.parse(raw) : null;
}
async function register() {
  const username = `audit-${randomUUID().slice(0, 8)}`;
  const password = `Audit-${randomUUID()}!`;
  const session = await request(null, '/auth/register', { method: 'POST', body: { username, password } });
  const user = { token: session.access_token };
  const me = await request(user, '/users/me');
  user.id = me.id;
  user.root = (await request(user, '/folders/root')).id;
  await request(null, '/auth/login', { method: 'POST', body: { login: username, password } });
  return user;
}
async function until(action, predicate, ms = 60000) {
  const start = performance.now(); let value;
  do { value = await action(); if (predicate(value)) return value; await sleep(200); } while (performance.now() - start < ms);
  throw new Error(`condition timed out after ${ms}ms: ${JSON.stringify(value).slice(0, 300)}`);
}
async function createUpload(user, data, name = `audit-${randomUUID()}.bin`) {
  const body = { folder_id: user.root, filename: name, sha256: hash(data), size_bytes: data.length, mime_type: name.endsWith('.txt') ? 'text/plain' : name.endsWith('.png') ? 'image/png' : 'application/octet-stream' };
  const key = randomUUID();
  const result = await request(user, '/uploads', { method: 'POST', body, headers: { 'Idempotency-Key': key } });
  return { result, body, key };
}
async function finishUpload(user, session, data) {
  session = await until(() => request(user, `/uploads/${session.id}`), s => s.status !== 'initializing');
  assert.ok(['created', 'uploading'].includes(session.status), session.status);
  for (let i = 1; i <= session.part_count; i++) {
    const signed = await request(user, `/uploads/${session.id}/parts/presign`, { method: 'POST', body: { part_numbers: [i] } });
    const part = data.subarray((i - 1) * session.part_size, i * session.part_size);
    const put = await fetch(signed.parts[0].url, { method: 'PUT', body: part, signal: AbortSignal.timeout(60000) });
    assert.equal(put.status, 200);
    const etag = put.headers.get('etag').replaceAll('"', ''); await put.arrayBuffer();
    await request(user, `/uploads/${session.id}/parts/confirm`, { method: 'POST', body: { parts: [{ part_number: i, etag, size_bytes: part.length }] } });
  }
  await Promise.all(apis.map(endpoint => request(user, `/uploads/${session.id}/complete`, { method: 'POST', endpoint })));
  const completed = await until(() => request(user, `/uploads/${session.id}`), s => ['completed', 'failed'].includes(s.status));
  assert.equal(completed.status, 'completed', completed.error_code);
  return completed;
}
async function upload(user, data, name) {
  const start = performance.now();
  const { result } = await createUpload(user, data, name);
  assert.equal(result.mode, 'multipart');
  const done = await finishUpload(user, result.upload, data);
  const url = await request(user, `/files/${done.completed_entry_id}/download-url`);
  const response = await fetch(url.url, { signal: AbortSignal.timeout(60000) });
  assert.equal(response.status, 200);
  assert.equal(hash(Buffer.from(await response.arrayBuffer())), hash(data));
  return { session: done, elapsed_ms: performance.now() - start };
}
function stats(samples) {
  samples.sort((a, b) => a - b);
  const p = n => samples[Math.min(samples.length - 1, Math.ceil(samples.length * n) - 1)] ?? 0;
  return { p50_ms: p(.5), p95_ms: p(.95), p99_ms: p(.99), max_ms: p(1) };
}
async function benchmark(name, concurrency, seconds, action) {
  const started = performance.now(); const end = started + seconds * 1000;
  const cpuStart = process.cpuUsage();
  let requests = 0, failures = 0; const times = []; const errors = [];
  await Promise.all(Array.from({ length: concurrency }, async (_, client) => {
    while (performance.now() < end) {
      const begin = performance.now();
      try { await action(client, requests); } catch (err) { failures++; if (errors.length < 5) errors.push(err.message); }
      times.push(performance.now() - begin); requests++;
    }
  }));
  const elapsed = (performance.now() - started) / 1000;
  const cpu=process.cpuUsage(cpuStart);
  const row = { name, concurrency, seconds: elapsed, requests, failures, rps: requests / elapsed, successful_rps:(requests-failures)/elapsed, ...stats(times), client_cpu_percent:(cpu.user+cpu.system)/10000/elapsed, client_rss_mib:process.memoryUsage().rss/(1024*1024), errors };
  report.benchmarks.push(row); log(JSON.stringify(row));
  return row;
}
function snapshot(name) {
  const rows = docker('ps', '-q', '--filter', `label=com.docker.compose.project=${project}`).split('\n').filter(Boolean);
  writeFileSync(`${dir}/${name}-docker-stats.txt`, docker('stats', '--no-stream', '--format', '{{json .}}', ...rows) + '\n');
  writeFileSync(`${dir}/${name}-postgres.txt`, sql("SELECT count(*), state FROM pg_stat_activity GROUP BY state; SELECT pg_size_pretty(pg_database_size(current_database())); SELECT count(*), status FROM outbox_events GROUP BY status; SELECT count(*), state FROM storage_operations GROUP BY state;") + '\n');
  for (const role of ['api','outbox-relay','object-worker','media-worker','maintenance-worker','upload-worker','search-indexer','ai-query','ai-indexer']) {
    for (const id of containers(role)) {
      try { writeFileSync(`${dir}/${name}-${role}-${id.slice(0, 8)}.prom`, docker('exec', id, 'wget', '-qO-', `http://127.0.0.1:${role === 'api' ? 8081 : 8083}/metrics`)); } catch { /* Some AI roles expose gRPC health only. */ }
    }
  }
}
const stopped = new Set();
const monitorIDs=docker('ps','-q','--filter',`label=com.docker.compose.project=${project}`).split('\n').filter(Boolean);
const monitor=spawn('docker',['stats','--format','{{json .}}',...monitorIDs],{stdio:['ignore','pipe','ignore']});
const monitorLines=createInterface({input:monitor.stdout});
monitorLines.on('line',line=>{try{appendFileSync(`${dir}/resources.jsonl`,JSON.stringify({at:new Date().toISOString(),...JSON.parse(line)})+'\n');}catch{/* Ignore terminal refresh control lines. */}});
function stop(ids) { for (const id of ids) { stopped.add(id); docker('stop', '-t', '3', id); } }
function start(ids) {
  for (const id of ids) { docker('start', id); stopped.delete(id); }
  const current=containers('api');
  if(current.length===2) apis.splice(0,apis.length,...current.map(id=>`http://${docker('port',id,'8081/tcp')}`));
}
async function fault(name, roles, action) {
  const ids = roles.flatMap(containers); assert.ok(ids.length > 0);
  stop(ids); const begin = performance.now();
  try { await action(); } finally { start(ids); }
  report.faults.push({ name, outage_ms: performance.now() - begin, recovered: true }); log(`fault ${name}: recovered`);
}

try {
  const owner = await register(); const recipient = await register();
  report.fixture_owner_id = owner.id; report.fixture_recipient_id = recipient.id;
  log('registered isolated users; register/login/root passed');
  const before = await request(owner, `/folders/${owner.root}/children`, { endpoint: apis[1] });
  assert.equal(before.items.length, 0);
  let folder = await request(owner, '/folders', { method: 'POST', body: { parent_id: owner.root, name: 'before' }, endpoint: apis[0] });
  assert.equal((await request(owner, `/folders/${owner.root}/children`, { endpoint: apis[1] })).items[0].name, 'before');
  folder = await request(owner, `/folders/${folder.id}`, { method: 'PATCH', body: { name: 'after' }, headers: { 'If-Match': String(folder.version) }, endpoint: apis[0] });
  assert.equal((await request(owner, `/folders/${owner.root}/children`, { endpoint: apis[1] })).items[0].name, 'after');
  report.checks.push('cross-API cached directory create/rename');
  for (let i=0;i<10;i++) {
    const a=await request(owner,'/folders',{method:'POST',body:{parent_id:owner.root,name:`cycle-a-${i}`}});
    const b=await request(owner,'/folders',{method:'POST',body:{parent_id:owner.root,name:`cycle-b-${i}`}});
    const moves=await Promise.all(apis.map((endpoint,n)=>request(owner,`/folders/${n?b.id:a.id}/move`,{method:'POST',body:{target_folder_id:n?a.id:b.id},headers:{'If-Match':'1'},endpoint,expected:[200,400,409,422]})));
    assert.equal(moves.filter(r=>r.id).length,1,'concurrent reciprocal moves must reject one side');
  }
  report.checks.push('10 concurrent reciprocal folder moves, no cycle');

  const content = Buffer.from(`Distributed audit content ${randomUUID()}. PostgreSQL owns permissions; outbox retries are idempotent.\n`);
  const first = await upload(owner, content, 'audit-document.txt');
  const fileID = first.session.completed_entry_id;
  report.checks.push('real multipart create/PUT/confirm/duplicate complete/verify/download SHA256');
  await request(recipient, `/files/${fileID}/download-url`, { expected: [404] });
  const pending = await createUpload(owner, randomBytes(1024));
  const retries = await Promise.all(apis.map(endpoint => request(owner, '/uploads', { method:'POST', body:pending.body, headers:{'Idempotency-Key':pending.key}, endpoint })));
  assert.ok(retries.every(r => r.upload.id === pending.result.upload.id));
  await request(owner, '/uploads', { method:'POST', body:{...pending.body, filename:'changed.bin'}, headers:{'Idempotency-Key':pending.key}, expected:[409] });
  await request(owner, `/uploads/${pending.result.upload.id}`, { method:'DELETE' });
  report.checks.push('cross-API create idempotency, changed request 409, upload cancel, ownership denial');

  const share = await request(owner, '/shares', { method:'POST', body:{file_id:fileID} });
  const resolved = await request(null, '/public/shares/resolve', { method:'POST', body:{share_key:share.share_key} });
  const importKey = randomUUID();
  const imports = await Promise.all(apis.map(endpoint => request(recipient, `/shares/${share.share.id}/import`, { method:'POST', body:{target_folder_id:recipient.root}, headers:{'Idempotency-Key':importKey,'X-Share-Token':resolved.share_access_token}, endpoint })));
  assert.equal(imports[0].imported_file.id, imports[1].imported_file.id);
  await request(owner, `/shares/${share.share.id}`, {method:'DELETE'});
  await request(null, '/public/shares/content', { headers:{'X-Share-Token':resolved.share_access_token}, expected:[404] });
  report.checks.push('concurrent share import, single entry/quota/reference, revocation');
  const indexed = await until(() => request(owner, `/files/${fileID}/ai`, { expected:[200,404] }), r => ['indexed','failed'].includes(r.status));
  assert.equal(indexed.status, 'indexed', indexed.error_code);
  const search = await request(owner, '/search', {method:'POST',body:{query:'PostgreSQL',mode:'hybrid',limit:10}});
  assert.ok(Array.isArray(search.hits));
  await request(owner, '/ai/ask', {method:'POST',body:{question:'What owns permissions?',file_ids:[fileID]}});
  report.checks.push('AI index/search/ask with fake provider');
  const recycledData=randomBytes(4096);
  const recycled=await upload(owner,recycledData);
  const oldKey=sql(`SELECT object_key FROM file_objects WHERE id=${quote(recycled.session.completed_object_id)}`);
  await request(owner,`/files/${recycled.session.completed_entry_id}`,{method:'DELETE',headers:{'If-Match':'1'}});
  await until(()=>sql(`SELECT status FROM file_objects WHERE id=${quote(recycled.session.completed_object_id)}`),s=>s==='deleted');
  const revived=await upload(owner,recycledData);
  assert.equal(revived.session.completed_object_id,recycled.session.completed_object_id);
  const newKey=sql(`SELECT object_key FROM file_objects WHERE id=${quote(revived.session.completed_object_id)}`);
  assert.notEqual(newKey,oldKey);
  report.checks.push('real GC then same-hash revival uses a new physical key and preserves downloaded SHA256');
  const thumbnail=await upload(owner,readFileSync('web/public/logo192.png'),'audit-thumbnail.png');
  await until(()=>Number(sql(`SELECT count(*) FROM object_variants WHERE object_id=${quote(thumbnail.session.completed_object_id)} AND variant_type='thumbnail' AND status='ready'`)),n=>n===1);
  const preview=await request(owner,`/files/${thumbnail.session.completed_entry_id}/preview`);
  const previewBytes=await fetch(preview.url,{signal:AbortSignal.timeout(10000)});
  assert.equal(previewBytes.status,200);
  assert.ok((await previewBytes.arrayBuffer()).byteLength>0);
  report.checks.push('real PNG upload, media thumbnail generation and preview download');
  snapshot('baseline');

  const apiIDs = containers('api');
  stop([apiIDs[0]]);
  try {
    const begin=performance.now();let consecutive=0,failures=0,lastFailureMs=0;
    while(consecutive<20 && performance.now()-begin<30000) {
      try { await request(owner,'/folders/root');consecutive++; }
      catch { failures++;consecutive=0;lastFailureMs=performance.now()-begin;await sleep(200); }
    }
    assert.equal(consecutive,20,'gateway did not converge to the surviving API');
    report.faults.push({name:'one API down',successful_requests:consecutive,transient_failures:failures,recovery_ms:lastFailureMs,observation_ms:performance.now()-begin});save();
  } finally { start([apiIDs[0]]); }
  await fault('cache unavailable', ['redis-cache'], async () => {
    await benchmark('cache_down_directory',8,10,() => request(owner,`/folders/${owner.root}/children?limit=50`));
  });
  let brokerPending;
  await fault('broker unavailable', ['rabbitmq'], async () => {
    brokerPending = await createUpload(owner,randomBytes(512));
    assert.equal(brokerPending.result.upload.status,'initializing');
    await request(owner,'/folders/root');
    assert.ok(Number(sql("SELECT count(*) FROM outbox_deliveries WHERE status <> 'published'")) > 0);
    await sleep(2000);
  });
  let recovered = performance.now();
  await until(() => request(owner,`/uploads/${brokerPending.result.upload.id}`),s=>s.status==='created');
  report.faults.at(-1).recovery_ms = performance.now()-recovered;
  await request(owner,`/uploads/${brokerPending.result.upload.id}`,{method:'DELETE'});
  let minioPending;
  await fault('MinIO unavailable', ['minio'], async () => {
    minioPending = await createUpload(owner,randomBytes(512));
    await request(owner,'/folders/root');
    assert.notEqual((await fetch(`${base}/storage-healthz`,{signal:AbortSignal.timeout(10000)})).status,200);
    await sleep(4000);
  });
  recovered=performance.now();
  await until(() => request(owner,`/uploads/${minioPending.result.upload.id}`),s=>s.status==='created',120000);
  report.faults.at(-1).recovery_ms=performance.now()-recovered;
  await request(owner,`/uploads/${minioPending.result.upload.id}`,{method:'DELETE'});
  const relayIDs=containers('outbox-relay'); stop(relayIDs);
  const relayPending=await createUpload(owner,randomBytes(512)); start([relayIDs[1]]);
  recovered=performance.now(); await until(()=>request(owner,`/uploads/${relayPending.result.upload.id}`),s=>s.status==='created');
  report.faults.push({name:'relay restart and leadership takeover',recovery_ms:performance.now()-recovered}); start([relayIDs[0]]);
  await request(owner,`/uploads/${relayPending.result.upload.id}`,{method:'DELETE'});
  await fault('AI query unavailable', ['ai-query'], async () => {
    await request(owner,'/folders/root');
    await request(owner,'/search',{method:'POST',body:{query:'audit',mode:'hybrid'},expected:[503],timeout:15000});
  });
  await until(()=>request(owner,'/search',{method:'POST',body:{query:'audit',mode:'hybrid'},expected:[200,503]}),r=>Array.isArray(r.hits));
  const indexerIDs=containers('ai-indexer');stop(indexerIDs);
  const backlog=[];
  try { for(let i=0;i<4;i++) backlog.push(await upload(owner,Buffer.from(`AI backlog PostgreSQL ${randomUUID()}`),`backlog-${i}.txt`)); }
  finally { start(indexerIDs); }
  recovered=performance.now();
  for(const item of backlog) await until(()=>request(owner,`/files/${item.session.completed_entry_id}/ai`,{expected:[200,404]}),r=>r.status==='indexed');
  report.faults.push({name:'AI indexer backlog',files:backlog.length,recovery_ms:performance.now()-recovered});save();

  for (const count of [1000,10000,50000]) {
    const cap = await request(owner,'/folders',{method:'POST',body:{parent_id:owner.root,name:`capacity-${count}`}});
    sql(`INSERT INTO folders (id,owner_id,parent_id,name,name_normalized) SELECT gen_random_uuid(),${quote(owner.id)}::uuid,${quote(cap.id)}::uuid,'item-'||lpad(i::text,6,'0'),'item-'||lpad(i::text,6,'0') FROM generate_series(1,${count}) i;`);
    for (const concurrency of (count === 50000 ? [8,32,64,128] : [8,32])) {
      await benchmark(`directory_${count}_first_page`,concurrency,15,client=>request(owner,`/folders/${cap.id}/children?limit=50&sort=name_asc`,{endpoint:apis[client%2]}));
    }
    let cursor='',deepCursor='';const seen=new Set();const scanTimes=[];
    do {
      deepCursor=cursor;const begin=performance.now();
      const page=await request(owner,`/folders/${cap.id}/children?limit=100&sort=name_asc${cursor?`&cursor=${encodeURIComponent(cursor)}`:''}`);
      scanTimes.push(performance.now()-begin);
      for(const item of page.items){assert.ok(!seen.has(item.id),'duplicate cursor item');seen.add(item.id);}
      cursor=page.page.has_more?page.page.next_cursor:'';
    }while(cursor);
    assert.equal(seen.size,count,'cursor scan lost items');
    report.benchmarks.push({name:`directory_${count}_full_cursor_scan`,requests:scanTimes.length,items:seen.size,...stats(scanTimes)});
    if(count===50000) {
      await benchmark('directory_50000_deep_uncached_page',16,15,client=>request(owner,`/folders/${cap.id}/children?limit=50&sort=name_asc&cursor=${encodeURIComponent(deepCursor)}`,{endpoint:apis[client%2]}));
      await benchmark('directory_50000_soak',64,120,client=>request(owner,`/folders/${cap.id}/children?limit=50&sort=name_asc`,{endpoint:apis[client%2]}));
    }
    snapshot(`capacity-${count}`);
  }
  await benchmark('mixed_core_read_write',16,20,async(client,iteration)=>{
    if(iteration%10===0) await request(owner,'/folders',{method:'POST',body:{parent_id:owner.root,name:`mixed-${randomUUID()}`},endpoint:apis[client%2]});
    else await request(owner,iteration%2?'/users/me/quota':`/folders/${owner.root}/children?limit=50`,{endpoint:apis[client%2]});
  });
  for (const concurrency of [1,4,8]) {
    const samples=[];const started=performance.now();
    await Promise.all(Array.from({length:concurrency},async()=>{for(let i=0;i<10;i++){const r=await upload(owner,randomBytes(1024*1024));samples.push(r.elapsed_ms);}}));
    const seconds=(performance.now()-started)/1000;
    report.benchmarks.push({name:'upload_1MiB_including_download_hash',concurrency,files:samples.length,seconds,MiB_per_second:samples.length/seconds,...stats(samples)});save();
  }
  const large=await upload(owner,randomBytes(16*1024*1024));
  report.benchmarks.push({name:'multipart_16MiB_including_download_hash',files:1,elapsed_ms:large.elapsed_ms});
  const downloadData=randomBytes(1024*1024),downloadHash=hash(downloadData);
  const downloadable=await upload(owner,downloadData);
  const downloadURL=await request(owner,`/files/${downloadable.session.completed_entry_id}/download-url`);
  for(const concurrency of [8,32]) {
    const row=await benchmark('download_1MiB_SHA256',concurrency,15,async()=>{
      const response=await fetch(downloadURL.url,{signal:AbortSignal.timeout(30000)});
      assert.equal(response.status,200);
      assert.equal(hash(Buffer.from(await response.arrayBuffer())),downloadHash);
    });
    row.MiB_per_second=(row.requests-row.failures)/row.seconds;save();
  }
  for (const concurrency of [1,4,8]) await benchmark('hybrid_search_fake_provider',concurrency,15,()=>request(owner,'/search',{method:'POST',body:{query:'PostgreSQL permissions',mode:'hybrid',limit:10}}));
  await until(()=>Number(sql("SELECT count(*) FROM outbox_deliveries WHERE status <> 'published'")),n=>n===0,120000);
  await until(()=>Number(sql("SELECT count(*) FROM storage_operations WHERE state <> 'succeeded'")),n=>n===0,120000);
  report.integrity=sql(`SELECT json_build_object('reference_mismatches',(SELECT count(*) FROM file_objects o WHERE o.reference_count <> (SELECT count(*) FROM file_entries e WHERE e.object_id=o.id AND e.deleted_at IS NULL)), 'active_entries_in_deleted_folders',(SELECT count(*) FROM file_entries e JOIN folders f ON f.id=e.folder_id WHERE e.deleted_at IS NULL AND f.deleted_at IS NOT NULL), 'unfinished_storage',(SELECT count(*) FROM storage_operations WHERE state <> 'succeeded'), 'quota_mismatches',(SELECT count(*) FROM users u WHERE u.used_logical_bytes <> COALESCE((SELECT sum(o.size_bytes) FROM file_entries e JOIN file_objects o ON o.id=e.object_id WHERE e.owner_id=u.id AND e.deleted_at IS NULL),0)), 'outbox_pending',(SELECT count(*) FROM outbox_deliveries WHERE status <> 'published'));`);
  for (const [key,value] of Object.entries(JSON.parse(report.integrity))) assert.equal(value,0,`integrity ${key}`);
  assert.deepEqual(report.benchmarks.filter(row=>row.failures>0).map(row=>({name:row.name,failures:row.failures})),[],'benchmark requests failed');
  snapshot('final'); report.completed_at=new Date().toISOString(); report.result='PASS';log(`AUDIT_RUNTIME=PASS ${report.integrity}`);
} catch(err) { report.result='FAIL';report.error=err.stack;log(err.stack);process.exitCode=1; }
finally { if(stopped.size) start([...stopped]); monitor.kill('SIGTERM');monitorLines.close();save(); }
