// Real deployed HTTP acceptance. Creates only labelled test users/data; SQL is read-only.
// FINAL_OUTPUT=artifacts/<run> node deploy/tests/final-acceptance.mjs
import assert from 'node:assert/strict';
import {createHash, randomBytes, randomUUID} from 'node:crypto';
import {execFile} from 'node:child_process';
import {promisify} from 'node:util';
import {mkdirSync, writeFileSync, chmodSync} from 'node:fs';
import {setTimeout as sleep} from 'node:timers/promises';
import {deflateSync} from 'node:zlib';

const exec = promisify(execFile);
const base = process.env.FINAL_BASE || 'http://127.0.0.1:3000';
assert.equal(new URL(base).hostname, '127.0.0.1');
const stamp = new Date().toISOString().replace(/[-:]/g, '').slice(0, 15);
const output = process.env.FINAL_OUTPUT || `artifacts/final-acceptance-${stamp}`;
mkdirSync(output, {recursive: true, mode: 0o700}); chmodSync(output, 0o700);
const report = {base, started_at: new Date().toISOString(), groups: [], requests: [], samples: [], external_failures: []};
const fixture = {base_url: base, users: [], files: [], sessions: []};
const hash = bytes => createHash('sha256').update(bytes).digest('hex');
const quote = value => "'" + String(value).replaceAll("'", "''") + "'";
const save = () => writeFileSync(`${output}/${process.env.FINAL_RESULT_NAME || (process.env.FINAL_HELPER ? 'supplement-' : '') + 'results.json'}`, JSON.stringify(report, null, 2) + '\n', {mode: 0o600});
const persist = () => writeFileSync(`${output}/private-${process.env.FINAL_HELPER ? 'supplement-' : ''}fixture.json`, JSON.stringify(fixture, null, 2), {mode: 0o600});
const sql = async (role, query) => {
  assert.match(query.trim(), /^(SELECT|WITH)\b/i);
  const {stdout} = await exec('docker', ['exec', `bx-yunpan-${role}-1`, 'sh', '-c',
    'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"', 'probe', query], {timeout: 20000, maxBuffer: 8 * 1024 * 1024});
  return stdout.trim();
};
const rows = async (role, query) => JSON.parse(await sql(role, `SELECT coalesce(json_agg(q),'[]'::json) FROM (${query}) q`));
async function request(user, path, {method = 'GET', body, headers = {}, expected = [200], timeout = 75000} = {}) {
  const started = performance.now();
  const entry = {at: new Date().toISOString(), method, path, expected};
  try {
    const res = await fetch(base + '/api/v1' + path, {method,
      headers: {'Content-Type': 'application/json', ...(user ? {Authorization: `Bearer ${user.token}`} : {}), ...headers},
      body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(timeout)});
    const raw = await res.text(); let data;
    try { data = raw ? JSON.parse(raw) : null; } catch { data = {text: raw.slice(0, 120)}; }
    Object.assign(entry, {status: res.status, code: data?.error?.code, ms: Math.round((performance.now() - started) * 100) / 100});
    report.requests.push(entry);
    assert.ok(expected.includes(res.status), `${method} ${path}: ${res.status} ${data?.error?.code || raw.slice(0, 100)}`);
    return {data, status: res.status, cookie: res.headers.get('set-cookie')?.split(';')[0], retry_after: res.headers.get('retry-after')};
  } catch (error) { if (!entry.status) report.requests.push({...entry, ms: Math.round(performance.now() - started), error: error.message}); throw error; }
}
async function group(name, action) {
  const start = Date.now();
  try { const evidence = await action(); report.groups.push({name, status: 'PASS', ms: Date.now() - start, evidence}); console.log('PASS ' + name); }
  catch (error) { report.groups.push({name, status: 'FAIL', ms: Date.now() - start, error: error.stack}); console.log('FAIL ' + name + ': ' + error.message); }
  save();
}
async function until(action, check, timeout = 150000) {
  const end = Date.now() + timeout; let value;
  do { value = await action(); if (check(value)) return value; await sleep(500); } while (Date.now() < end);
  throw Error('observable state timeout: ' + JSON.stringify(value).slice(0, 500));
}
async function register(label) {
  const username = `final_${stamp.slice(4)}_${label}`, password = randomUUID() + '!Aa1';
  const {data, cookie} = await request(null, '/auth/register', {method: 'POST', body: {username, password}, expected: [201]});
  const user = {id: data.user.id, username, password, token: data.access_token, cookie};
  user.root = (await request(user, '/folders/root')).data.id;
  fixture.users.push(user); persist(); return user;
}
const createBody = (u, bytes, filename, mime = 'application/octet-stream') => ({folder_id: u.root, filename, sha256: hash(bytes), size_bytes: bytes.length, mime_type: mime});
async function create(u, bytes, filename, mime, extra = {}) {
  const {data} = await request(u, '/uploads', {method: 'POST', body: {...createBody(u, bytes, filename, mime), ...extra}, headers: {'Idempotency-Key': randomUUID()}, expected: [201]});
  if (data.upload) {fixture.sessions.push({id: data.upload.id, owner: u.id, filename}); persist();}
  return data;
}
async function parts(u, initial, bytes) {
  const session = await until(async () => (await request(u, `/uploads/${initial.id}`)).data, s => ['created', 'uploading', 'failed', 'aborted'].includes(s.status));
  assert.ok(['created', 'uploading'].includes(session.status), JSON.stringify(session));
  const numbers = Array.from({length: session.part_count}, (_, i) => i + 1).reverse();
  const signed = (await request(u, `/uploads/${session.id}/parts/presign`, {method: 'POST', body: {part_numbers: numbers}})).data.parts;
  const confirmed = await Promise.all(signed.map(async p => {
    const bytesPart = bytes.subarray((p.part_number - 1) * session.part_size, p.part_number * session.part_size);
    const res = await fetch(p.url, {method: 'PUT', body: bytesPart, signal: AbortSignal.timeout(60000)});
    report.requests.push({at: new Date().toISOString(), method: 'PUT', path: '/storage/<presigned-part>', status: res.status, expected: [200]});
    assert.equal(res.status, 200); await res.arrayBuffer();
    return {part_number: p.part_number, etag: res.headers.get('etag').replaceAll('"', ''), size_bytes: bytesPart.length};
  }));
  return {session, confirmed};
}
async function finish(u, initial, bytes) {
  const {confirmed} = await parts(u, initial, bytes);
  await request(u, `/uploads/${initial.id}/parts/confirm?view=delta`, {method: 'POST', body: {parts: confirmed}});
  await request(u, `/uploads/${initial.id}/complete`, {method: 'POST', expected: [202]});
  return until(async () => (await request(u, `/uploads/${initial.id}`)).data, s => ['completed', 'failed', 'aborted'].includes(s.status));
}
async function download(u, file, bytes) {
  const signed = (await request(u, `/files/${file}/download-url`)).data;
  const res = await fetch(signed.url, {signal: AbortSignal.timeout(60000)});
  assert.equal(res.status, 200); assert.equal(hash(Buffer.from(await res.arrayBuffer())), hash(bytes));
}
async function upload(u, bytes, name, mime) {
  const started = Date.now(), initial = await create(u, bytes, name, mime);
  assert.equal(initial.mode, 'multipart');
  const done = await finish(u, initial.upload, bytes); assert.equal(done.status, 'completed', done.error_code);
  const file = {id: done.completed_entry_id, object: done.completed_object_id, owner: u.id, name, bytes: bytes.length, sha256: hash(bytes), e2e_ms: Date.now() - started};
  fixture.files.push(file); persist(); await download(u, file.id, bytes); return file;
}
function png() {
  const crc = bytes => {let c = 0xffffffff; for (const b of bytes) {c ^= b; for (let n = 0; n < 8; n++) c = (c >>> 1) ^ ((c & 1) ? 0xedb88320 : 0);} return (c ^ 0xffffffff) >>> 0;};
  const chunk = (type, data) => {const t = Buffer.from(type), n = Buffer.alloc(4), c = Buffer.alloc(4); n.writeUInt32BE(data.length); c.writeUInt32BE(crc(Buffer.concat([t, data]))); return Buffer.concat([n, t, data, c]);};
  const h = Buffer.alloc(13); h.writeUInt32BE(32); h.writeUInt32BE(32, 4); h[8] = 8; h[9] = 2;
  return Buffer.concat([Buffer.from([137,80,78,71,13,10,26,10]), chunk('IHDR', h), chunk('IDAT', deflateSync(Buffer.concat(Array.from({length: 32}, () => Buffer.concat([Buffer.from([0]), randomBytes(96)]))))), chunk('IEND', Buffer.alloc(0))]);
}
async function snapshot(label) {
  const ownerList = fixture.users.length ? fixture.users.map(x => quote(x.id)).join(',') : "'00000000-0000-0000-0000-000000000000'";
  const domain = await rows('postgres', `SELECT id FROM upload_sessions WHERE user_id IN (${ownerList}) UNION SELECT id FROM file_entries WHERE owner_id IN (${ownerList}) UNION SELECT object_id AS id FROM file_entries WHERE owner_id IN (${ownerList})`);
  const ids = [...new Set([...fixture.users.map(x => x.id), ...fixture.sessions.map(x => x.id), ...fixture.files.flatMap(x => [x.id, x.object]), ...domain.map(x => x.id)])];
  const list = ids.length ? ids.map(quote).join(',') : "'00000000-0000-0000-0000-000000000000'";
  const owners = fixture.users.length ? fixture.users.map(x => quote(x.id)).join(',') : list;
  const objects = `SELECT DISTINCT object_id FROM file_entries WHERE owner_id IN (${owners})`;
  const snapshot = {at: new Date().toISOString(), label};
  snapshot.migrations = {};
  for (const role of ['postgres', 'async-postgres', 'ai-postgres']) snapshot.migrations[role] = await rows(role, 'SELECT * FROM schema_migrations');
  snapshot.global = await rows('postgres', `SELECT 'reference_mismatch' AS check_name,count(*) AS violations FROM file_objects o WHERE reference_count<>(SELECT count(*) FROM file_entries f WHERE f.object_id=o.id AND f.deleted_at IS NULL) UNION ALL SELECT 'active_file_not_ready',count(*) FROM file_entries f JOIN file_objects o ON o.id=f.object_id WHERE f.deleted_at IS NULL AND o.status<>'ready'`);
  snapshot.quota = await rows('postgres', `SELECT u.id,u.username,u.used_logical_bytes,u.reserved_bytes,q.occupied_bytes,coalesce((SELECT sum(o.size_bytes) FROM file_entries f JOIN file_objects o ON o.id=f.object_id WHERE f.owner_id=u.id AND f.deleted_at IS NULL),0) AS actual_logical_bytes,coalesce((SELECT sum(s.reserved_bytes) FROM upload_sessions s WHERE s.user_id=u.id),0) AS session_reserved_bytes FROM users u JOIN user_quota_usage q ON q.owner_id=u.id WHERE u.id IN (${owners})`);
  snapshot.sessions = await rows('postgres', `SELECT status,error_code,count(*) AS count,sum(reserved_bytes) AS reserved_bytes FROM upload_sessions WHERE user_id IN (${owners}) GROUP BY status,error_code ORDER BY status,error_code`);
  snapshot.business_checks = await rows('postgres', `SELECT 'reference_mismatch' AS check_name,count(*) AS violations FROM file_objects o WHERE o.id IN (${objects}) AND o.reference_count<>(SELECT count(*) FROM file_entries f WHERE f.object_id=o.id AND f.deleted_at IS NULL) UNION ALL SELECT 'terminal_upload_reservation',count(*) FROM upload_sessions WHERE user_id IN (${owners}) AND status IN ('completed','failed','aborted','expired') AND reserved_bytes<>0 UNION ALL SELECT 'completed_identity_mismatch',count(*) FROM upload_sessions s LEFT JOIN file_entries f ON f.id=s.completed_entry_id WHERE s.user_id IN (${owners}) AND s.status='completed' AND (f.id IS NULL OR f.object_id<>s.completed_object_id OR f.owner_id<>s.user_id) UNION ALL SELECT 'active_file_not_ready',count(*) FROM file_entries f JOIN file_objects o ON o.id=f.object_id WHERE f.owner_id IN (${owners}) AND f.deleted_at IS NULL AND o.status<>'ready'`);
  snapshot.operations = await rows('postgres', `SELECT operation_type,state,count(*) AS count FROM storage_operations WHERE upload_session_id IN (SELECT id FROM upload_sessions WHERE user_id IN (${owners})) OR object_id IN (${objects}) GROUP BY operation_type,state ORDER BY 1,2`);
  const events = await rows('postgres', `SELECT id,seq,event_type,aggregate_id FROM outbox_events WHERE aggregate_id IN (${list}) OR payload::text LIKE ANY(ARRAY[${ids.length ? ids.map(x => quote('%'+x+'%')).join(',') : "'__none__'"}])`);
  snapshot.events = {total: events.length, max_seq: Math.max(0, ...events.map(x => Number(x.seq)))};
  snapshot.missing_inbox = events.length ? await rows('async-postgres', `SELECT unnest(ARRAY[${events.map(x => quote(x.id)).join(',')}]::uuid[]) AS id EXCEPT SELECT id FROM event_inbox`) : [];
  snapshot.relay = await rows('async-postgres', 'SELECT * FROM relay_positions');
  const taskPredicate = `owner_key IN (${list}) OR resource_key IN (${list}) OR input::text LIKE ANY(ARRAY[${ids.length ? ids.map(x => quote('%'+x+'%')).join(',') : "'__none__'"}])`;
  snapshot.tasks = await rows('async-postgres', `SELECT kind,state,outcome,phase,count(*) AS count,count(*) FILTER(WHERE state='pending' AND not_before>now()) AS scheduled_future,count(*) FILTER(WHERE state IN ('pending','running') AND not_before<=now()) AS actionable FROM tasks WHERE ${taskPredicate} GROUP BY 1,2,3,4 ORDER BY 1,2`);
  snapshot.task_errors = await rows('async-postgres', `SELECT id,kind,state,outcome,last_error,attempts,max_attempts,not_before,deadline FROM tasks WHERE (${taskPredicate}) AND (state='stopped' OR (state IN ('pending','running') AND not_before<=now())) ORDER BY created_at LIMIT 100`);
  snapshot.future_tasks = await rows('async-postgres', `SELECT kind,count(*) AS count,min(not_before) AS earliest,max(not_before) AS latest FROM tasks WHERE (${taskPredicate}) AND state='pending' AND not_before>now() GROUP BY kind ORDER BY kind`);
  const results = await rows('async-postgres', `SELECT r.id,r.effect_target,r.input_digest,r.result_digest,r.outcome,t.semantic_key FROM results r JOIN tasks t ON t.id=r.task_id WHERE r.committed_at IS NOT NULL AND (${taskPredicate.split('input::text').join('t.input::text')})`);
  const coreResults = results.filter(r => r.effect_target === 'core');
  const aiResults = results.filter(r => r.effect_target === 'ai');
  const coreReceipts = coreResults.length ? await rows('postgres', `SELECT semantic_key,input_digest,result_digest,result_id,outcome FROM async_commit_receipts WHERE semantic_key IN (${coreResults.map(r=>quote(r.semantic_key)).join(',')})`) : [];
  const aiReceipts = aiResults.length ? await rows('ai-postgres', `SELECT result_id,input_digest,result_digest,outcome FROM ai_commit_receipts WHERE result_id IN (${aiResults.map(r=>quote(r.id)).join(',')})`) : [];
  snapshot.receipts = {core_results:coreResults.length, ai_results:aiResults.length, missing_or_conflicting:results.filter(r=>!(r.effect_target==='ai'?aiReceipts:coreReceipts).some(p=>p.result_id===r.id&&p.input_digest===r.input_digest&&p.result_digest===r.result_digest&&p.outcome===r.outcome)).map(r=>({id:r.id,effect_target:r.effect_target,outcome:r.outcome}))};
  snapshot.global_tasks = await rows('async-postgres', 'SELECT kind,state,count(*) AS count FROM tasks GROUP BY 1,2 ORDER BY 1,2');
  const objectIds = await rows('postgres', objects);
  const aiList = objectIds.length ? objectIds.map(x => quote(x.object_id)).join(',') : list;
  snapshot.ai = await rows('ai-postgres', `SELECT object_id,content_generation,tombstoned_at,active_source_id,active_content_version_id,active_embedding_set_id,active_summary_id FROM ai_objects WHERE object_id IN (${aiList})`);
  snapshot.capabilities = await rows('ai-postgres', `SELECT object_id,capability,latest_build_state,error_code FROM ai_capability_status WHERE object_id IN (${aiList}) ORDER BY 1,2`);
  snapshot.projections = await rows('ai-postgres', `SELECT backend,state,sum(expected_count) AS expected_count,sum(applied_count) AS applied_count,count(*) AS count FROM ai_projection_states WHERE object_id IN (${aiList}) GROUP BY 1,2`);
  snapshot.ai_checks = await rows('ai-postgres', `SELECT 'active_source_identity' AS check_name,count(*) AS violations FROM ai_objects o LEFT JOIN ai_sources s ON s.id=o.active_source_id WHERE o.object_id IN (${aiList}) AND o.active_source_id IS NOT NULL AND (s.id IS NULL OR s.object_id<>o.object_id OR s.content_generation<>o.content_generation OR s.publication_state<>'published') UNION ALL SELECT 'active_content_identity',count(*) FROM ai_objects o LEFT JOIN ai_content_versions c ON c.id=o.active_content_version_id WHERE o.object_id IN (${aiList}) AND o.active_content_version_id IS NOT NULL AND (c.id IS NULL OR c.object_id<>o.object_id OR c.source_id<>o.active_source_id OR c.publication_state<>'published') UNION ALL SELECT 'active_vector_identity',count(*) FROM ai_objects o LEFT JOIN ai_embedding_sets e ON e.id=o.active_embedding_set_id WHERE o.object_id IN (${aiList}) AND o.active_embedding_set_id IS NOT NULL AND (e.id IS NULL OR e.content_version_id<>o.active_content_version_id OR e.publication_state<>'published') UNION ALL SELECT 'active_summary_identity',count(*) FROM ai_objects o LEFT JOIN ai_summaries s ON s.id=o.active_summary_id WHERE o.object_id IN (${aiList}) AND o.active_summary_id IS NOT NULL AND (s.id IS NULL OR s.source_id<>o.active_source_id OR s.publication_state<>'published') UNION ALL SELECT 'complete_projection_count',count(*) FROM ai_projection_states WHERE object_id IN (${aiList}) AND state='complete' AND expected_count<>applied_count`);
  snapshot.ai_checks.push(...await rows('ai-postgres', `SELECT 'parent_child_counts' AS check_name,count(*) AS violations FROM ai_content_versions c WHERE c.object_id IN (${aiList}) AND (c.child_count<>(SELECT count(*) FROM ai_child_chunks x WHERE x.content_version_id=c.id) OR c.parent_count<>(SELECT count(*) FROM ai_parent_chunks p WHERE p.content_version_id=c.id)) UNION ALL SELECT 'vector_counts',count(*) FROM ai_embedding_sets e WHERE e.object_id IN (${aiList}) AND e.vector_count<>(SELECT count(*) FROM ai_chunk_vectors v WHERE v.embedding_set_id=e.id)`));
  snapshot.containers = JSON.parse((await exec('docker', ['inspect', ...(await exec('docker', ['ps', '-q', '--filter', 'label=com.docker.compose.project=bx-yunpan'])).stdout.trim().split('\n')], {maxBuffer: 5*1024*1024})).stdout).map(c => ({name: c.Name, image: c.Config.Image, image_id: c.Image, started_at: c.State.StartedAt, restart_count: c.RestartCount, oom: c.State.OOMKilled, health: c.State.Health?.Status}));
  writeFileSync(`${output}/db-${label}.json`, JSON.stringify(snapshot, null, 2), {mode: 0o600});
  return snapshot;
}
async function sample(phase) {
  const {stdout} = await exec('docker', ['stats', '--no-stream', '--format', '{{json .}}'], {timeout: 15000});
  report.samples.push({at: new Date().toISOString(), phase, containers: stdout.trim().split('\n').filter(Boolean).map(x => JSON.parse(x))}); save();
}
function external(data, context) {
  const code = data?.error?.code || '';
  assert.match(code, /^ai\.(quota_exhausted|provider|model|upstream|timeout|unavailable|rate_limit|embedding|ask_failed|internal)/, `unclassified external AI error: ${code}`);
  report.external_failures.push({context, code, message: data?.error?.message});
}
let owner, peer, outsider, doc, docBytes;
if (!process.env.FINAL_HELPER) {
try {
  report.baseline = await snapshot('baseline');
  await group('register-login-root-quota-and-unauthenticated-contract', async () => {
    owner = await register('owner'); peer = await register('peer'); outsider = await register('outsider');
    for (const path of ['/users/me', '/folders/root', '/uploads', '/ai/files']) await request(null, path, {expected: [401]});
    const login = await request(null, '/auth/login', {method: 'POST', body: {login: owner.username, password: owner.password}});
    assert.equal(login.data.user.id, owner.id); await request(owner, '/users/me/quota');
    return {users: fixture.users.map(x => ({id:x.id, username:x.username}))};
  });
  assert.ok(owner && peer && outsider, 'test users required');
  await group('folder-create-race-and-versioned-rename', async () => {
    const results = await Promise.all(Array.from({length: 8}, () => request(owner, '/folders', {method: 'POST', body: {parent_id: owner.root, name: 'final-race'}, expected: [201,409]})));
    assert.equal(results.filter(x => x.status===201).length, 1);
    const folder = results.find(x => x.status===201).data;
    const renamed = await Promise.all(Array.from({length: 8}, (_,i) => request(owner, `/folders/${folder.id}`, {method:'PATCH', body:{name:'race-'+i}, headers:{'If-Match':String(folder.version)}, expected:[200,409]})));
    assert.equal(renamed.filter(x => x.status===200).length, 1);
    return {create_statuses:results.map(x=>x.status), rename_statuses:renamed.map(x=>x.status)};
  });
  await group('multipart-resume-missing-parts-conflict-replay-and-download-sha256', async () => {
    const bytes = randomBytes(20*1024*1024+123), key=randomUUID(), body=createBody(owner,bytes,'multipart-final.bin');
    const starts=await Promise.all(Array.from({length:4},()=>request(owner,'/uploads',{method:'POST',body,headers:{'Idempotency-Key':key},expected:[201]})));
    assert.equal(new Set(starts.map(x=>x.data.upload.id)).size,1); const initial=starts[0].data.upload;
    fixture.sessions.push({id:initial.id,owner:owner.id,filename:body.filename});persist();
    await request(owner,'/uploads',{method:'POST',body:{...body,filename:'changed.bin'},headers:{'Idempotency-Key':key},expected:[409]});
    const {session,confirmed}=await parts(owner,initial,bytes);assert.ok(session.part_count>=3);
    await request(owner,`/uploads/${initial.id}/parts/confirm?view=delta`,{method:'POST',body:{parts:confirmed.slice(0,1)}});
    await request(owner,`/uploads/${initial.id}/complete`,{method:'POST',expected:[409]});
    const resumed=(await request(owner,`/uploads/${initial.id}`)).data;assert.equal(resumed.confirmed_parts.length,1);
    await request(owner,`/uploads/${initial.id}/parts/confirm?view=delta`,{method:'POST',body:{parts:confirmed}});
    await request(owner,`/uploads/${initial.id}/parts/confirm?view=delta`,{method:'POST',body:{parts:confirmed.slice(0,1)}});
    await request(owner,`/uploads/${initial.id}/parts/confirm?view=delta`,{method:'POST',body:{parts:[{...confirmed[0],etag:'conflict'}]},expected:[409]});
    await Promise.all(Array.from({length:4},()=>request(owner,`/uploads/${initial.id}/complete`,{method:'POST',expected:[202]})));
    const done=await until(async()=>(await request(owner,`/uploads/${initial.id}`)).data,s=>['completed','failed'].includes(s.status));assert.equal(done.status,'completed',done.error_code);
    fixture.files.push({id:done.completed_entry_id,object:done.completed_object_id,owner:owner.id,name:body.filename,bytes:bytes.length,sha256:hash(bytes)});persist();
    await download(owner,done.completed_entry_id,bytes);return {parts:session.part_count,bytes:bytes.length,sha256:hash(bytes),session:initial.id};
  });
  await group('hash-mismatch-and-abort-release-reservations', async () => {
    const bytes=randomBytes(8192), initial=await create(owner,bytes,'bad-hash.bin',undefined,{sha256:hash(randomBytes(8192))});
    const failed=await finish(owner,initial.upload,bytes);assert.equal(failed.status,'failed');assert.match(failed.error_code,/hash|sha|checksum/);
    const cancel=await create(owner,randomBytes(1024),'abort.bin');
    await Promise.all(Array.from({length:4},()=>request(owner,`/uploads/${cancel.upload.id}`,{method:'DELETE',expected:[204]})));
    await until(async()=>(await request(owner,`/uploads/${cancel.upload.id}`)).data,s=>s.status==='aborted');
    return {hash_error:failed.error_code,abort_replays:4};
  });
  await group('text-csv-and-image-upload-parser-and-thumbnail', async () => {
    docBytes=Buffer.from(`最终验收 ${randomUUID()}。回滚口令是 ORCHID731。PostgreSQL 保存事实，MinIO 保存字节。回滚时先核对版本，再恢复快照。\n`.repeat(12));
    doc=await upload(owner,docBytes,'final-policy.txt','text/plain');
    const csv=await upload(owner,Buffer.from(`item,value\nacceptance,${randomUUID()}\nrollback,ORCHID731\n`),'final-table.csv','text/csv');
    const image=await upload(owner,png(),'final-image.png','image/png');
    await until(async()=>(await request(owner,`/files/${image.id}/preview`)).data,p=>p.kind==='image');
    for(const file of [doc,csv]) await until(async()=>(await request(owner,`/files/${file.id}/ai`)).data,x=>x.capabilities?.askable===true,180000);
    return {text:doc.id,csv:csv.id,image:image.id};
  });
  await group('sharing-gated-range-import-idempotency-revoke-and-isolation', async () => {
    assert.ok(doc);await request(peer,`/files/${doc.id}/download-url`,{expected:[404]});await request(peer,`/files/${doc.id}/ai`,{expected:[404]});
    const share=(await request(owner,'/shares',{method:'POST',body:{file_id:doc.id},expected:[201]})).data;
    const resolved=(await request(null,'/public/shares/resolve',{method:'POST',body:{share_key:share.share_key}})).data;
    const headers={'X-Share-Token':resolved.share_access_token};
    const content=(await request(null,'/public/shares/content',{headers})).data;
    const url=new URL(content.download.url,base);
    const full=await fetch(url);assert.equal(full.status,200);assert.equal(hash(Buffer.from(await full.arrayBuffer())),hash(docBytes));
    const range=await fetch(url,{headers:{Range:'bytes=0-9'}});assert.equal(range.status,206);assert.deepEqual(Buffer.from(await range.arrayBuffer()),docBytes.subarray(0,10));
    const imported=await Promise.all(Array.from({length:8},()=>request(peer,`/shares/${share.share.id}/import`,{method:'POST',body:{target_folder_id:peer.root},headers:{...headers,'Idempotency-Key':share.share.id},expected:[201]})));
    assert.equal(new Set(imported.map(x=>x.data.imported_file.id)).size,1);const file=imported[0].data.imported_file;
    fixture.files.push({id:file.id,object:doc.object,owner:peer.id,name:file.name,bytes:docBytes.length,sha256:hash(docBytes)});persist();
    await request(outsider,`/shares/${share.share.id}`,{expected:[404]});
    await request(owner,`/shares/${share.share.id}`,{method:'DELETE',expected:[204]});
    await request(null,'/public/shares/content',{headers,expected:[404]});
    await request(null,'/public/shares/resolve',{method:'POST',body:{share_key:share.share_key},expected:[404]});
    await request(peer,`/shares/${share.share.id}/import`,{method:'POST',body:{target_folder_id:peer.root},headers:{...headers,'Idempotency-Key':randomUUID()},expected:[404]});
    await download(peer,file.id,docBytes);return {imports:8,imported_entry:file.id,revoked_status:404,range_status:206};
  });
  await group('cross-owner-canonical-dedup-and-single-reference-delete', async () => {
    const bytes=randomBytes(12345), first=await upload(owner,bytes,'dedup-owner.bin'), second=await upload(peer,bytes,'dedup-peer.bin');
    assert.equal(first.object,second.object);
    await request(owner,'/uploads',{method:'POST',body:createBody(owner,bytes,'duplicate.bin'),headers:{'Idempotency-Key':randomUUID()},expected:[409]});
    const meta=(await request(owner,`/files/${first.id}`)).data;
    await request(owner,`/files/${first.id}`,{method:'DELETE',headers:{'If-Match':String(meta.version)},expected:[204]});
    await request(owner,`/files/${first.id}`,{expected:[404]});await download(peer,second.id,bytes);
    return {canonical:first.object,refs_expected:1};
  });
  await group('AI-fulltext-name-semantic-hybrid-and-scoped-ask', async () => {
    assert.ok(doc);
    for(const mode of ['name','fulltext','semantic','hybrid']) {
      const r=await request(owner,'/search',{method:'POST',body:{query:mode==='name'?'final-policy':'ORCHID731',mode},expected:[200,429,502,503,504]});
      if(r.status===200) {if(['name','fulltext'].includes(mode)) assert.ok(JSON.stringify(r.data).includes(doc.id),`${mode} missing document`);}
      else {assert.ok(!['name','fulltext'].includes(mode),'model-free search failed');external(r.data,'search-'+mode);}
    }
    for(const scope of [{mode:'selected',file_ids:[doc.id]},{mode:'all'}]) {
      const r=await request(owner,'/ai/ask',{method:'POST',body:{question:'回滚口令是什么？请给出引用。',scope},expected:[200,429,502,503,504]});
      if(r.status===200) {assert.ok(r.data.answer);assert.ok(r.data.citations?.length);for(const c of r.data.citations) {assert.ok(c.source_version_id&&c.content_version_id&&c.parent_id&&c.child_id);if(scope.mode==='selected')assert.equal(c.file_id,doc.id);}}
      else external(r.data,'ask-'+scope.mode);
    }
    await request(outsider,'/ai/ask',{method:'POST',body:{question:'回滚口令',scope:{mode:'selected',file_ids:[doc.id]}},expected:[409]});
    await request(owner,'/ai/ask',{method:'POST',body:{question:'回滚口令',scope:{mode:'selected',file_ids:[]}},expected:[400]});
    return {external_failures:report.external_failures};
  });
  await group('AI-reprocess-job-terminal-and-job-owner-isolation', async () => {
    const job=(await request(owner,`/files/${doc.id}/ai/reprocess`,{method:'POST',body:{targets:['summary']},headers:{'Idempotency-Key':randomUUID()},expected:[202]})).data;
    fixture.job=job;persist();await request(outsider,`/ai/jobs/${job.job_id}`,{expected:[404]});
    const terminal=await until(async()=>(await request(owner,`/ai/jobs/${job.job_id}`)).data,j=>['succeeded','failed','partial','stopped','partially_succeeded'].includes(j.status),180000);
    if(terminal.status!=='succeeded') report.external_failures.push({context:'summary-job',terminal});
    return terminal;
  });
  await group('spike-32-users-320-requests-and-8-parallel-real-uploads', async () => {
    const started=Date.now(), offset=report.requests.length;let controlled=0;
    await Promise.all(Array.from({length:32},async(_,actor)=>{
      for(let n=0;n<10;n++) {
        const paths=['/users/me',`/folders/${owner.root}/children?limit=10`,'/users/me/quota','/uploads?limit=10'];
        const r=await request(owner,paths[(actor+n)%paths.length],{expected:[200,503]});
        if(r.status===503){assert.equal(r.data.error.code,'service.overloaded');assert.equal(r.retry_after,'1');controlled++;}
      }
    }));
    await sleep(1100);for(let n=0;n<10;n++)await request(owner,`/folders/${owner.root}/children?limit=10`);
    const files=await Promise.all(Array.from({length:8},(_,i)=>upload(owner,randomBytes(32768),`spike-${i}.bin`)));
    const requests=report.requests.slice(offset).filter(x=>x.ms!==undefined).map(x=>x.ms).sort((a,b)=>a-b);
    return {actors:32,read_requests:320,controlled_503:controlled,uploads:files.length,elapsed_ms:Date.now()-started,p95_ms:requests[Math.floor(requests.length*.95)],p99_ms:requests[Math.floor(requests.length*.99)],max_upload_e2e_ms:Math.max(...files.map(x=>x.e2e_ms))};
  });
  await group('soak-600-seconds-mixed-reads-writes-and-upload-download', async () => {
    const started=Date.now(), end=started+600000, offset=report.requests.length;let iterations=0, uploads=0;
    report.soak={started_at:new Date().toISOString(),duration_required_ms:600000,reader_actors:6};save();
    const reads=Array.from({length:6},async(_,actor)=>{
      while(Date.now()<end) {
        const paths=[`/folders/${owner.root}/children?limit=10`,'/users/me/quota','/uploads?limit=10','/ai/files?limit=10',`/files/${doc.id}`,`/files/${doc.id}/ai`];
        const r=await request(owner,paths[(iterations+++actor)%paths.length],{expected:[200,409,503]});
        if(r.status===409)assert.equal(r.data.error.code,'ai.scope_changed');
        if(r.status===503){assert.equal(r.data.error.code,'service.overloaded');assert.equal(r.retry_after,'1');}
        await sleep(150);
      }
    });
    const writes=(async()=>{let n=0;while(Date.now()<end){
      const folder=(await request(owner,'/folders',{method:'POST',body:{parent_id:owner.root,name:`soak-${n}-${randomUUID().slice(0,6)}`},expected:[201]})).data;
      const renamed=(await request(owner,`/folders/${folder.id}`,{method:'PATCH',body:{name:`soak-renamed-${n}`},headers:{'If-Match':String(folder.version)}})).data;
      await request(owner,`/folders/${folder.id}`,{method:'DELETE',headers:{'If-Match':String(renamed.version)},expected:[204]});
      await upload(owner,randomBytes(16384),`soak-${n++}.bin`);uploads++;
      await sleep(Math.min(30000,Math.max(0,end-Date.now())));
    }})();
    const monitor=(async()=>{while(Date.now()<end){await sample('soak');console.log(`SOAK ${Math.round((Date.now()-started)/1000)}s reads=${iterations} uploads=${uploads}`);await sleep(Math.min(30000,Math.max(0,end-Date.now())));}})();
    const outcomes=await Promise.allSettled([...reads,writes,monitor]);
    report.soak.finished_at=new Date().toISOString();report.soak.elapsed_ms=Date.now()-started;
    const requests=report.requests.slice(offset).filter(x=>x.ms!==undefined), latencies=requests.map(x=>x.ms).sort((a,b)=>a-b);
    Object.assign(report.soak,{read_iterations:iterations,uploads,requests:requests.length,rps:requests.length/((Date.now()-started)/1000),p95_ms:latencies[Math.floor(latencies.length*.95)],p99_ms:latencies[Math.floor(latencies.length*.99)],errors:outcomes.filter(x=>x.status==='rejected').map(x=>x.reason.message)});
    assert.deepEqual(report.soak.errors,[]);assert.ok(report.soak.elapsed_ms>=600000);return report.soak;
  });
  await group('database-final-convergence', async () => {
    let current;
    await until(async()=>{current=await snapshot('settling');console.log('SETTLE actionable='+current.tasks.reduce((s,x)=>s+Number(x.actionable),0)+' missing_inbox='+current.missing_inbox.length);return current;},s=>s.missing_inbox.length===0&&s.tasks.every(x=>Number(x.actionable)===0),180000);
    report.final=await snapshot('final');
    for(const row of [...report.final.business_checks,...report.final.ai_checks])assert.equal(Number(row.violations),0,row.check_name);
    for(const row of report.final.quota){assert.equal(Number(row.occupied_bytes),Number(row.actual_logical_bytes)+Number(row.session_reserved_bytes));assert.equal(Number(row.session_reserved_bytes),0);}
    assert.ok(report.final.sessions.every(x=>['completed','failed','aborted','expired'].includes(x.status)));
    assert.ok(report.final.containers.every(x=>!x.oom&&x.health==='healthy'));
    for(const c of report.final.containers){const old=report.baseline.containers.find(x=>x.name===c.name);assert.equal(c.restart_count,old?.restart_count,c.name+' restart');assert.equal(c.started_at,old?.started_at,c.name+' recreated');}
    return {quota:report.final.quota,checks:report.final.business_checks,ai_checks:report.final.ai_checks,tasks:report.final.tasks};
  });
} catch(error) {report.fatal=error.stack;console.error(error.message);}
finally {
  report.finished_at=new Date().toISOString();persist();
  report.summary={pass:report.groups.filter(x=>x.status==='PASS').length,fail:report.groups.filter(x=>x.status==='FAIL').length,requests:report.requests.length,external_failures:report.external_failures.length};
  report.status=report.summary.fail||report.fatal?'FAIL':'PASS';save();console.log(JSON.stringify({output,status:report.status,...report.summary}));
}
if(report.status!=='PASS')process.exitCode=1;
}
export {fixture,report,request,group,until,create,parts,finish,upload,download,snapshot,sample,sql,rows,quote,save,output};
