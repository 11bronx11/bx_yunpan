import assert from 'node:assert/strict';
import {createHash, randomBytes, randomUUID} from 'node:crypto';
import {execFileSync} from 'node:child_process';
import {chmodSync, mkdirSync, readFileSync, writeFileSync} from 'node:fs';
import {setTimeout as sleep} from 'node:timers/promises';

const project = process.env.FULLCHAIN_PROJECT ?? 'bx-fullchain-20261106';
const base = process.env.FULLCHAIN_BASE ?? 'http://127.0.0.1:24000';
const output = process.env.FULLCHAIN_OUTPUT ?? 'artifacts/fullchain-20261106';
assert.match(project, /^bx-fullchain-/);
assert.equal(new URL(base).hostname, '127.0.0.1');
mkdirSync(output, {recursive: true});
const suite = process.argv[2] ?? 'core';
const report = {suite, project, base, started_at: new Date().toISOString(), groups: [], requests: []};
const save = () => writeFileSync(`${output}/${suite}-results.json`, JSON.stringify(report, null, 2) + '\n');
const hash = value => createHash('sha256').update(value).digest('hex');
const docker = (...args) => execFileSync('docker', args, {encoding: 'utf8', timeout: 90000}).trim();
const containers = role => docker('ps', '-q', '--filter', `label=com.docker.compose.project=${project}`, '--filter', `label=com.docker.compose.service=${role}`).split('\n').filter(Boolean);
const setting = (role, key) => JSON.parse(docker('inspect', '--format', '{{json .Config.Env}}', containers(role)[0])).find(v => v.startsWith(`${key}=`))?.slice(key.length + 1);
const sql = query => docker('exec', `${project}-postgres-1`, 'sh', '-c', 'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"', 'probe', query);
const quote = value => `'${String(value).replaceAll("'", "''")}'`;
const apiEndpoints = () => containers('api').map(id => `http://${docker('port', id, '8081/tcp')}`);
let apis = apiEndpoints();
assert.equal(apis.length, 2, 'two independent API replicas required');
let counter = 0;
async function request(user, path, {method = 'GET', body, headers = {}, expected = [200, 201, 202, 204], endpoint, cookie, quiet = false} = {}) {
  const started = Date.now();
  const target = endpoint ?? apis[counter++ % apis.length];
  const response = await fetch(`${target}/api/v1${path}`, {
    method, headers: {'Content-Type': 'application/json', ...(user ? {Authorization: `Bearer ${user.token}`} : {}), ...(cookie ? {Cookie: cookie} : {}), ...headers},
    body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(150000),
  });
  const raw = await response.text();
  let data; try { data = raw ? JSON.parse(raw) : null; } catch { data = {text: raw.slice(0, 120)}; }
  if (!quiet || !expected.includes(response.status)) report.requests.push({method, path, api: target, status: response.status, code: data?.error?.code, ms: Date.now() - started});
  assert.ok(expected.includes(response.status), `${method} ${path}: status=${response.status} code=${data?.error?.code ?? 'none'}`);
  return {data, status: response.status, cookie: response.headers.get('set-cookie')?.split(';')[0]};
}
async function group(name, fn) {
  const started = Date.now();
  try { const evidence = await fn(); report.groups.push({name, pass: true, ms: Date.now() - started, evidence}); console.log(`PASS ${name}`); }
  catch (error) { report.groups.push({name, pass: false, ms: Date.now() - started, error: error.message}); console.log(`FAIL ${name}: ${error.message}`); }
  save();
}
async function until(action, check, timeout = 120000) {
  const deadline = Date.now() + timeout; let last;
  do { last = await action(); if (check(last)) return last; await sleep(350); } while (Date.now() < deadline);
  throw new Error(`observable state timeout: ${JSON.stringify(last).slice(0, 200)}`);
}
async function register() {
  const username = `fc-${randomUUID().slice(0, 12)}`, password = `Fullchain-${randomUUID()}!`;
  const r = await request(null, '/auth/register', {method: 'POST', body: {username, password}});
  const user = {id: r.data.user.id, username, password, token: r.data.access_token, cookie: r.cookie};
  user.root = (await request(user, '/folders/root')).data.id;
  return user;
}
async function login(owner) {
  const r = await request(null, '/auth/login', {method: 'POST', body: {login: owner.username, password: owner.password}});
  return {...owner, token: r.data.access_token, cookie: r.cookie};
}
const createBody = (u, bytes, filename, mime = 'application/octet-stream') => ({folder_id: u.root, filename, sha256: hash(bytes), size_bytes: bytes.length, mime_type: mime});
const create = async (u, bytes, name, mime) => (await request(u, '/uploads', {method: 'POST', body: createBody(u, bytes, name, mime), headers: {'Idempotency-Key': randomUUID()}})).data;
async function sendParts(u, initial, bytes, devices = [u]) {
  const session = await until(async () => (await request(u, `/uploads/${initial.id}`, {quiet: true})).data, s => ['created', 'uploading', 'failed'].includes(s.status));
  assert.notEqual(session.status, 'failed', session.error_code);
  for (let n = 1; n <= session.part_count; n++) {
    const signed = (await request(u, `/uploads/${session.id}/parts/presign`, {method: 'POST', body: {part_numbers: [n]}})).data.parts[0];
    const part = bytes.subarray((n - 1) * session.part_size, n * session.part_size);
    const put = await fetch(signed.url, {method: 'PUT', body: part, signal: AbortSignal.timeout(60000)});
    assert.equal(put.status, 200); const etag = put.headers.get('etag').replaceAll('"', ''); await put.arrayBuffer();
    await Promise.all(devices.map(device => request(device, `/uploads/${session.id}/parts/confirm`, {method: 'POST', body: {parts: [{part_number: n, etag, size_bytes: part.length}]}})));
  }
  return session;
}
async function finish(u, initial, bytes, devices = [u]) {
  const session = await sendParts(u, initial, bytes, devices);
  await Promise.all(devices.map(device => request(device, `/uploads/${session.id}/complete`, {method: 'POST'})));
  const done = await until(async () => (await request(u, `/uploads/${session.id}`, {quiet: true})).data, s => ['completed', 'failed', 'aborted'].includes(s.status));
  assert.equal(done.status, 'completed', done.error_code);
  return done;
}
async function download(u, file, bytes) {
  const signed = (await request(u, `/files/${file}/download-url`)).data;
  const r = await fetch(signed.url, {signal: AbortSignal.timeout(60000)});
  assert.equal(r.status, 200); assert.equal(hash(Buffer.from(await r.arrayBuffer())), hash(bytes));
}
async function upload(u, bytes, name, mime) {
  const start = await create(u, bytes, name, mime);
  const file = start.mode === 'instant' ? start.file : await finish(u, start.upload, bytes);
  const result = {file: file.completed_entry_id ?? file.id, object: file.completed_object_id ?? file.object_id, bytes: bytes.length};
  await download(u, result.file, bytes); return result;
}
const statePath = `${output}/.env.fixtures.json`;
let fixture;
const persist = () => {writeFileSync(statePath, JSON.stringify(fixture), {mode: 0o600}); chmodSync(statePath, 0o600);};

if (suite === 'core') {
  const owner = await register(), stranger = await register();
  fixture = {owner, stranger, documents: []}; persist();
  let devices;
  await group('eight simultaneous device logins create independent refresh families', async () => {
    devices = await Promise.all(Array.from({length: 8}, () => login(owner)));
    assert.equal(new Set(devices.map(d => d.cookie)).size, 8);
    assert.ok((await Promise.all(devices.map(d => request(d, '/users/me')))).every(r => r.data.id === owner.id));
    fixture.devices = devices; persist(); return {devices: 8, apis: apis.length};
  });
  assert.ok(devices, 'device logins required');
  await group('same refresh token race has one winner; replay revokes only that device family', async () => {
    const results = await Promise.all(devices.map(() => request(null, '/auth/refresh', {method: 'POST', cookie: devices[0].cookie, expected: [200, 401]})));
    assert.equal(results.filter(r => r.status === 200).length, 1);
    const winner = results.find(r => r.status === 200);
    await request(null, '/auth/refresh', {method: 'POST', cookie: winner.cookie, expected: [401]});
    const independent = await request(null, '/auth/refresh', {method: 'POST', cookie: devices[1].cookie});
    await request({...devices[1], token: independent.data.access_token}, '/auth/logout', {method: 'POST', cookie: independent.cookie});
    await request(null, '/auth/refresh', {method: 'POST', cookie: independent.cookie, expected: [401]});
    const third = await request(null, '/auth/refresh', {method: 'POST', cookie: devices[2].cookie});
    devices[2].cookie = third.cookie;
    await request(null, '/auth/refresh', {method: 'POST', cookie: devices[3].cookie, headers: {Origin: 'https://attacker.invalid'}, expected: [403]});
    return {rotation_winners: 1, replay_rejected: 7, independent_device_survives: true, logout_scope: 'current refresh session; access JWT remains valid until expiry'};
  });
  await group('concurrent same-name folder creation and versioned rename have exactly one winner', async () => {
    const created = await Promise.all(devices.map(d => request(d, '/folders', {method: 'POST', body: {parent_id: owner.root, name: 'race-folder'}, expected: [201, 409]})));
    assert.equal(created.filter(r => r.status === 201).length, 1);
    const folder = created.find(r => r.status === 201).data;
    const renamed = await Promise.all(devices.map((d, i) => request(d, `/folders/${folder.id}`, {method: 'PATCH', body: {name: `winner-${i}`}, headers: {'If-Match': String(folder.version)}, expected: [200, 409]})));
    assert.equal(renamed.filter(r => r.status === 200).length, 1);
    return {create_winners: 1, rename_winners: 1, conflicts: 14};
  });
  await group('reciprocal folder moves serialize across devices without creating cycles', async () => {
    for (let i = 0; i < 6; i++) {
      const folders = await Promise.all(['a', 'b'].map(name => request(owner, '/folders', {method: 'POST', body: {parent_id: owner.root, name: `move-${i}-${name}`}})));
      const results = await Promise.all(folders.map((f, n) => request(devices[n], `/folders/${f.data.id}/move`, {method: 'POST', body: {target_folder_id: folders[1 - n].data.id}, headers: {'If-Match': String(f.data.version)}, expected: [200, 409, 422]})));
      assert.equal(results.filter(r => r.status === 200).length, 1);
    }
    return {races: 6, cycles: 0};
  });
  await group('upload create, part confirmation and complete replay are idempotent across eight devices', async () => {
    const bytes = randomBytes(64000), key = randomUUID(), body = createBody(owner, bytes, 'device-replay.bin');
    const starts = await Promise.all(devices.map(d => request(d, '/uploads', {method: 'POST', body, headers: {'Idempotency-Key': key}})));
    assert.equal(new Set(starts.map(r => r.data.upload.id)).size, 1);
    const id = starts[0].data.upload.id;
    await request(owner, '/uploads', {method: 'POST', body: {...body, filename: 'changed.bin'}, headers: {'Idempotency-Key': key}, expected: [409]});
    const done = await finish(owner, starts[0].data.upload, bytes, devices);
    await download(devices[7], done.completed_entry_id, bytes);
    const operations = sql(`SELECT operation_type, count(*) FROM storage_operations WHERE upload_session_id=${quote(id)} GROUP BY operation_type ORDER BY operation_type`);
    assert.match(operations, /multipart.create\|1/); assert.match(operations, /multipart.complete\|1/);
    return {session: id, operations, requests_per_action: devices.length};
  });
  await group('parallel same-content uploads converge to one canonical object with exact references', async () => {
    const bytes = randomBytes(96000);
    const starts = await Promise.all(devices.slice(0, 6).map((d, i) => create(d, bytes, `dedup-${i}.bin`)));
    assert.ok(starts.every(s => s.mode === 'multipart'));
    await Promise.all(starts.map((s, i) => sendParts(devices[i], s.upload, bytes)));
    await Promise.all(starts.map((s, i) => request(devices[i], `/uploads/${s.upload.id}/complete`, {method: 'POST'})));
    const done = await Promise.all(starts.map(s => until(async () => (await request(owner, `/uploads/${s.upload.id}`, {quiet: true})).data, r => ['completed', 'failed'].includes(r.status))));
    // Migration 7 deliberately permits only one active entry per owner/object,
    // even under different filenames. Losers release their reserved quota.
    assert.equal(done.filter(s => s.status === 'completed').length, 1);
    assert.ok(done.filter(s => s.status === 'failed').every(s => s.error_code === 'upload.file_exists'));
    const winner = done.find(s => s.status === 'completed'), object = winner.completed_object_id;
    assert.equal(Number(sql(`SELECT reference_count FROM file_objects WHERE id=${quote(object)}`)), 1);
    await request(owner, '/uploads', {method: 'POST', body: createBody(owner, bytes, 'same-owner-duplicate.bin'), headers: {'Idempotency-Key': randomUUID()}, expected: [409]});
    const peer = await upload(stranger, bytes, 'shared-canonical.bin');
    assert.equal(peer.object, object);
    await request(owner, `/files/${winner.completed_entry_id}`, {method: 'DELETE', headers: {'If-Match': '1'}});
    await download(stranger, peer.file, bytes);
    return {parallel_uploads: 6, winners: 1, controlled_duplicate_failures: 5, canonical_objects: 1, same_owner_duplicate_status: 409, shared_object_survives_delete: true};
  });
  await group('same filename with different keys admits one pending upload; abort releases quota once', async () => {
    const bytes = randomBytes(4000);
    const results = await Promise.all(devices.map(d => request(d, '/uploads', {method: 'POST', body: createBody(owner, bytes, 'same-name.bin'), headers: {'Idempotency-Key': randomUUID()}, expected: [201, 409]})));
    assert.equal(results.filter(r => r.status === 201).length, 1);
    const session = results.find(r => r.status === 201).data.upload;
    await Promise.all(devices.map(d => request(d, `/uploads/${session.id}`, {method: 'DELETE', expected: [204]})));
    assert.equal(Number(sql(`SELECT reserved_bytes FROM users WHERE id=${quote(owner.id)}`)), 0);
    return {create_winners: 1, abort_replays: 8, reserved_bytes: 0};
  });
  await group('upload complete versus abort race converges to a valid terminal state', async () => {
    const outcomes = [];
    for (let i = 0; i < 5; i++) {
      const bytes = randomBytes(2400), start = await create(owner, bytes, `complete-abort-${i}.bin`);
      await sendParts(owner, start.upload, bytes);
      await Promise.all([
        request(devices[4], `/uploads/${start.upload.id}/complete`, {method: 'POST', expected: [202, 409]}),
        request(devices[5], `/uploads/${start.upload.id}`, {method: 'DELETE', expected: [204, 409]}),
      ]);
      const terminal = await until(async () => (await request(owner, `/uploads/${start.upload.id}`, {quiet: true})).data, s => ['completed', 'aborted', 'failed'].includes(s.status));
      assert.ok(['completed', 'aborted'].includes(terminal.status), terminal.error_code);
      if (terminal.status === 'completed') await download(owner, terminal.completed_entry_id, bytes);
      outcomes.push(terminal.status);
    }
    assert.equal(Number(sql(`SELECT reserved_bytes FROM users WHERE id=${quote(owner.id)}`)), 0);
    return {races: 5, outcomes, reserved_bytes: 0};
  });
  await group('real multipart resume across devices preserves all parts and download hash', async () => {
    const bytes = randomBytes(11 * 1024 * 1024), start = await create(owner, bytes, 'resumed-multipart.bin');
    assert.ok(start.upload.part_count > 1);
    const done = await finish(devices[5], start.upload, bytes, [devices[4], devices[5]]);
    await download(devices[6], done.completed_entry_id, bytes);
    return {parts: start.upload.part_count, bytes: bytes.length};
  });
  await group('Chinese document upload and cross-account ownership isolation', async () => {
    const bytes = Buffer.from('澄海项目验收资料。恢复口令是 ORCHID731。PostgreSQL 保存元数据，MinIO 保存文件内容。中文搜索使用 Elasticsearch，语义检索使用 Qdrant。');
    const doc = await upload(owner, bytes, '中文验收.txt', 'text/plain');
    fixture.doc = doc; fixture.documents.push({...doc, name: '中文验收.txt', query: '澄海项目'}); persist();
    await request(stranger, `/files/${doc.file}/download-url`, {expected: [404]});
    await request(stranger, `/files/${doc.file}/ai`, {expected: [404]});
    return doc;
  });
  await group('concurrent share import creates one entry; revocation blocks new access', async () => {
    const share = (await request(owner, '/shares', {method: 'POST', body: {file_id: fixture.doc.file}})).data;
    const resolved = (await request(null, '/public/shares/resolve', {method: 'POST', body: {share_key: share.share_key}})).data;
    const peers = await Promise.all(Array.from({length: 8}, () => login(stranger)));
    const headers = {'Idempotency-Key': randomUUID(), 'X-Share-Token': resolved.share_access_token};
    const results = await Promise.all(peers.map(d => request(d, `/shares/${share.share.id}/import`, {method: 'POST', body: {target_folder_id: stranger.root}, headers})));
    assert.equal(new Set(results.map(r => r.data.imported_file.id)).size, 1);
    fixture.imported = results[0].data.imported_file.id; persist();
    await request(owner, `/shares/${share.share.id}`, {method: 'DELETE'});
    await request(null, '/public/shares/content', {headers, expected: [404]});
    await request(stranger, `/shares/${share.share.id}/import`, {method: 'POST', body: {target_folder_id: stranger.root}, headers: {...headers, 'Idempotency-Key': randomUUID()}, expected: [404]});
    return {imports: 8, entries: 1, revoked_access: 404};
  });
  await group('concurrent rename, move and delete on the same file obey If-Match', async () => {
    const bytes = randomBytes(500), doc = await upload(owner, bytes, 'mixed-race.bin');
    const target = (await request(owner, '/folders', {method: 'POST', body: {parent_id: owner.root, name: 'mixed-target'}})).data;
    const results = await Promise.all([
      request(devices[3], `/files/${doc.file}`, {method: 'PATCH', body: {name: 'renamed-race.bin'}, headers: {'If-Match': '1'}, expected: [200, 404, 409]}),
      request(devices[4], `/files/${doc.file}/move`, {method: 'POST', body: {target_folder_id: target.id}, headers: {'If-Match': '1'}, expected: [200, 404, 409]}),
      request(devices[5], `/files/${doc.file}`, {method: 'DELETE', headers: {'If-Match': '1'}, expected: [204, 404, 409]}),
    ]);
    assert.equal(results.filter(r => [200, 204].includes(r.status)).length, 1);
    return {statuses: results.map(r => r.status), winners: 1};
  });
  persist();
} else if (suite === 'sharing') {
  const owner = await register(), recipient = await register(), outsider = await register();
  const bytes = Buffer.from(`分享模块验收：${randomUUID()}，取消、过期和导入隔离。`);
  const source = await upload(owner, bytes, 'sharing-proof.txt', 'text/plain');
  let created, resolved, content, imported;
  const createShare = async (expires_at = null) => (await request(owner, '/shares', {method: 'POST', body: {file_id: source.file, expires_at}})).data;
  await group('create validation rejects foreign files, missing files and past dates', async () => {
    await request(recipient, '/shares', {method: 'POST', body: {file_id: source.file}, expected: [404]});
    await request(owner, '/shares', {method: 'POST', body: {file_id: randomUUID()}, expected: [404]});
    for (const expires_at of ['bad-date', new Date(Date.now() - 1000).toISOString()]) {
      await request(owner, '/shares', {method: 'POST', body: {file_id: source.file, expires_at}, expected: [422]});
    }
    created = await createShare(); assert.equal(created.share_key.length, 22);
    resolved = (await request(null, '/public/shares/resolve', {method: 'POST', body: {share_key: created.share_key}})).data;
    return {share_key_length: 22, invalid_inputs_rejected: 4};
  });
  await group('owner metadata, bounded pagination and cursor ownership are consistent across APIs', async () => {
    for (let i = 0; i < 5; i++) await createShare();
    const seen = new Set(); let cursor = '';
    do {
      const page = (await request(owner, '/shares?limit=2' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''))).data;
      assert.ok(page.items.length <= 2);
      for (const share of page.items) {
        assert.equal(share.file_name, 'sharing-proof.txt'); assert.equal(share.file_available, true);
        assert.equal(share.status, 'active'); assert.equal(share.share_key, undefined);
        assert.ok(!seen.has(share.id)); seen.add(share.id);
      }
      cursor = page.page.next_cursor;
      if (cursor) await request(outsider, '/shares?limit=2&cursor=' + encodeURIComponent(cursor), {expected: [422]});
    } while (cursor);
    assert.equal(seen.size, 6);
    for (const method of ['GET', 'DELETE', 'PATCH']) await request(outsider, '/shares/' + created.share.id, {method, body: method === 'PATCH' ? {version: 1, expires_at: null} : undefined, expected: [404]});
    for (const query of ['limit=0', 'limit=101', 'cursor=garbage']) await request(owner, '/shares?' + query, {expected: [422]});
    return {visited: seen.size, page_size: 2};
  });
  await group('public gated download, preview, HEAD and ranges return exact bytes with no-store', async () => {
    content = (await request(null, '/public/shares/content', {headers: {'X-Share-Token': resolved.share_access_token}})).data;
    assert.ok(content.preview.url.startsWith('/api/v1/public/shares/file?'));
    for (const path of [content.download.url, content.preview.url]) {
      const response = await fetch(base + path); assert.equal(response.status, 200);
      assert.equal(response.headers.get('cache-control'), 'no-store');
      assert.equal(hash(Buffer.from(await response.arrayBuffer())), hash(bytes));
    }
    const head = await fetch(base + content.download.url, {method: 'HEAD'}); assert.equal(head.status, 200); assert.equal(Number(head.headers.get('content-length')), bytes.length);
    const range = await fetch(base + content.download.url, {headers: {Range: 'bytes=0-9'}}); assert.equal(range.status, 206);
    assert.deepEqual(Buffer.from(await range.arrayBuffer()), bytes.subarray(0, 10));
    return {bytes: bytes.length, range_status: 206};
  });
  await group('quota rejection returns the storage error and leaves imports, usage and references unchanged', async () => {
    const userID = quote(recipient.id), shareID = quote(created.share.id), objectID = quote(source.object);
    const originalQuota = sql(`SELECT quota_bytes FROM users WHERE id=${userID}`);
    const refs = sql(`SELECT reference_count FROM file_objects WHERE id=${objectID}`);
    sql(`UPDATE users SET quota_bytes=0 WHERE id=${userID}`);
    try {
      const r = await request(recipient, '/shares/' + created.share.id + '/import', {method: 'POST',
        headers: {'X-Share-Token': resolved.share_access_token, 'Idempotency-Key': randomUUID()},
        body: {target_folder_id: recipient.root}, expected: [429]});
      assert.equal(r.data.error.code, 'quota.exceeded');
      assert.equal(Number(sql(`SELECT count(*) FROM share_imports WHERE user_id=${userID} AND share_id=${shareID}`)), 0);
      assert.equal(Number(sql(`SELECT used_logical_bytes FROM users WHERE id=${userID}`)), 0);
      assert.equal(sql(`SELECT reference_count FROM file_objects WHERE id=${objectID}`), refs);
    } finally { sql(`UPDATE users SET quota_bytes=${BigInt(originalQuota)} WHERE id=${userID}`); }
    return {status: 429, new_imports: 0, usage_delta: 0, reference_delta: 0};
  });
  await group('eight concurrent imports preserve one entry, one quota charge and target-bound replay', async () => {
    const peers = await Promise.all(Array.from({length: 8}, () => login(recipient)));
    const headers = {'X-Share-Token': resolved.share_access_token, 'Idempotency-Key': randomUUID()};
    const results = await Promise.all(peers.map(user => request(user, '/shares/' + created.share.id + '/import', {method: 'POST', headers, body: {target_folder_id: recipient.root}})));
    const ids = new Set(results.map(r => r.data.imported_file.id)); assert.equal(ids.size, 1); imported = [...ids][0];
    const other = (await request(recipient, '/folders', {method: 'POST', body: {parent_id: recipient.root, name: 'other'}})).data;
    await request(recipient, '/shares/' + created.share.id + '/import', {method: 'POST', headers, body: {target_folder_id: other.id}, expected: [409]});
    await download(recipient, imported, bytes);
    return {requests: 8, entries: 1};
  });
  await group('concurrent expiry edits admit one writer and stale versions return a specific 409', async () => {
    const expires_at = new Date(Date.now() + 3600000).toISOString();
    const edits = await Promise.all(Array.from({length: 8}, () => request(owner, '/shares/' + created.share.id, {method: 'PATCH', body: {version: 1, expires_at}, expected: [200, 409]})));
    assert.equal(edits.filter(r => r.status === 200).length, 1);
    assert.ok(edits.filter(r => r.status === 409).every(r => r.data.error.code === 'share.version_conflict'));
    return {writers: 1, controlled_conflicts: 7};
  });
  await group('share expiry update reaches Outbox, PostgreSQL and Elasticsearch ACL projections', async () => {
    const id = quote(created.share.id), sourceID = quote(source.file);
    await until(() => Number(sql(`SELECT count(*) FROM outbox_deliveries d JOIN outbox_events e ON e.id=d.event_id WHERE e.aggregate_id=${id} AND e.event_type='share.updated.v1' AND d.status='published' AND EXISTS (SELECT 1 FROM task_results r WHERE r.task_id=d.event_id::text || ':' || d.destination AND r.status='succeeded')`)), n => n >= 1);
    const expectedACL = Number(sql(`SELECT acl_epoch FROM owner_content_epochs WHERE owner_id=${quote(owner.id)}`));
    const target = setting('search-indexer', 'SEARCH_INDEX_EPOCH') || 'v1';
    await until(() => Number(sql(`SELECT COALESCE(max(acl_version),0) FROM search_file_projections WHERE file_entry_id=${sourceID}`)), n => n >= expectedACL);
    const endpoint = setting('search-indexer', 'SEARCH_ES_ENDPOINT'); assert.ok(endpoint);
    await until(async () => {
      const result = await fetch(`http://127.0.0.1:29200/files_${target}/_doc/${source.file}`);
      const body = await result.json(); return body._source?.acl_version || 0;
    }, n => n >= expectedACL);
    return {acl_epoch: expectedACL, update_delivery_consumed: true};
  });
  await group('concurrent revocation blocks original keys, existing access tokens and issued byte URLs', async () => {
    await Promise.all(Array.from({length: 8}, () => request(owner, '/shares/' + created.share.id, {method: 'DELETE', expected: [204]})));
    await request(null, '/public/shares/resolve', {method: 'POST', body: {share_key: created.share_key}, expected: [404]});
    await request(null, '/public/shares/content', {headers: {'X-Share-Token': resolved.share_access_token}, expected: [404]});
    for (const path of [content.preview.url, content.download.url]) {
      const response = await fetch(base + path, {headers: {Range: 'bytes=0-9'}}); assert.equal(response.status, 404); await response.arrayBuffer();
    }
    await request(recipient, '/shares/' + created.share.id + '/import', {method: 'POST', body: {target_folder_id: recipient.root}, headers: {'X-Share-Token': resolved.share_access_token, 'Idempotency-Key': randomUUID()}, expected: [404]});
    const current = (await request(owner, '/shares/' + created.share.id)).data; assert.equal(current.status, 'revoked');
    await request(owner, '/shares/' + created.share.id, {method: 'PATCH', body: {version: current.version, expires_at: null}, expected: [404]});
    await download(recipient, imported, bytes);
    const events = Number(sql(`SELECT count(*) FROM outbox_events WHERE aggregate_id=${quote(created.share.id)} AND event_type='share.revoked.v1'`)); assert.equal(events, 1);
    return {revokes: 8, revocation_events: events, imported_copy_survives: true};
  });
  await group('expiry shortening invalidates an existing token and URL; renewal keeps the key usable', async () => {
    const expiring = await createShare();
    const access = (await request(null, '/public/shares/resolve', {method: 'POST', body: {share_key: expiring.share_key}})).data;
    const before = (await request(null, '/public/shares/content', {headers: {'X-Share-Token': access.share_access_token}})).data;
    await request(owner, '/shares/' + expiring.share.id, {method: 'PATCH', body: {version: 1, expires_at: new Date(Date.now() + 1800).toISOString()}});
    await sleep(2000);
    const expired = (await request(owner, '/shares/' + expiring.share.id)).data; assert.equal(expired.status, 'expired');
    await request(null, '/public/shares/resolve', {method: 'POST', body: {share_key: expiring.share_key}, expected: [404]});
    const denied = await fetch(base + before.download.url); assert.equal(denied.status, 404); await denied.arrayBuffer();
    await request(owner, '/shares/' + expiring.share.id, {method: 'PATCH', body: {version: expired.version, expires_at: null}});
    await request(null, '/public/shares/resolve', {method: 'POST', body: {share_key: expiring.share_key}});
    return {expired_status: 404, renewed_same_key: true};
  });
  await group('source deletion invalidates all remaining shares while imported bytes survive', async () => {
    const file = (await request(owner, '/files/' + source.file)).data;
    await request(owner, '/files/' + source.file, {method: 'DELETE', headers: {'If-Match': String(file.version)}});
    const list = (await request(owner, '/shares')).data;
    assert.ok(list.items.every(s => s.status === 'revoked' || s.status === 'unavailable'));
    await request(owner, '/shares', {method: 'POST', body: {file_id: source.file}, expected: [404]});
    await download(recipient, imported, bytes);
    return {unavailable_shares: list.items.filter(s => s.status === 'unavailable').length};
  });
} else if (suite === 'ai') {
  assert.equal(setting('ai-query', 'SEARCH_EXTERNAL_FALLBACK'), 'false');
  assert.equal(setting('ai-query', 'SEARCH_READ_BACKEND'), 'es_qdrant');
  fixture = JSON.parse(readFileSync(statePath));
  fixture.owner = await login(fixture.owner);
  fixture.stranger = await login(fixture.stranger);
  fixture.devices = await Promise.all(fixture.devices.map(() => login(fixture.owner)));
  persist();
  const {owner, stranger, doc} = fixture;
  await group('real AI indexing through remote parser and model provider', async () => {
    const result = await until(async () => (await request(owner, `/files/${doc.file}/ai`, {expected: [200, 404], quiet: true})).data, d => ['indexed', 'failed', 'unsupported'].includes(d?.status), 240000);
    assert.equal(result.status, 'indexed', result.error_code);
    assert.ok(Number(sql(`SELECT count(*) FROM ai_chunks c JOIN ai_documents d ON d.id=c.document_id WHERE d.object_id=${quote(doc.object)} AND c.document_version_id=d.active_version_id`)) > 0);
    return {status: result.status, summary_present: Boolean(result.summary)};
  });
  await group('ES fulltext and Qdrant semantic/hybrid retrieve expected file with fallback disabled', async () => {
    const found = {};
    for (const mode of ['name', 'fulltext', 'semantic', 'hybrid']) {
      const r = await until(async () => (await request(owner, '/search', {method: 'POST', body: {query: mode === 'name' ? '中文验收' : '澄海项目', mode, file_ids: [doc.file], limit: 5}})).data, s => s.hits.some(h => h.file?.id === doc.file));
      found[mode] = r.hits.length;
    }
    return {found, fallback: false};
  });
  await group('real RAG answer cites the authorized document and its recovery code', async () => {
    const r = (await request(owner, '/ai/ask', {method: 'POST', body: {question: '澄海项目的恢复口令是什么？', file_ids: [doc.file]}})).data;
    assert.ok(r.answer.includes('ORCHID731'), 'answer must contain the source recovery code');
    assert.ok(r.citations.length > 0); assert.ok(r.citations.every(c => c.file_id === doc.file));
    writeFileSync(`${output}/rag-answer.json`, JSON.stringify(r, null, 2) + '\n');
    return {answer_contains_source_fact: true, citations: r.citations.length};
  });
  await group('AI reprocess replay shares one async task and publishes one active document version', async () => {
    const key = randomUUID();
    const jobs = await Promise.all([0, 1].map(n => request(fixture.devices[4 + n], `/files/${doc.file}/ai/reprocess`, {method: 'POST', headers: {'Idempotency-Key': key}})));
    const ids = jobs.map(r => r.data.id ?? r.data.task?.id); assert.ok(ids[0]); assert.equal(ids[0], ids[1]);
    await request(stranger, `/ai/jobs/${ids[0]}`, {expected: [404]});
    const terminal = await until(async () => (await request(owner, `/ai/jobs/${ids[0]}`, {quiet: true})).data, j => ['succeeded', 'failed', 'canceled'].includes(j.status), 240000);
    assert.equal(terminal.status, 'succeeded', terminal.error_code);
    return {requests: 2, task: ids[0], status: terminal.status};
  });
  await group('PDF DOCX GB18030 UTF16 remote parsing, indexing and Chinese/English retrieval', async () => {
    const specs = JSON.parse(readFileSync(`${output}/documents/manifest.json`));
    const results = [];
    for (const spec of specs) {
      const bytes = readFileSync(`${output}/documents/${spec.name}`);
      const item = await upload(owner, bytes, spec.name, spec.mime);
      fixture.documents.push({...item, ...spec}); persist();
      const state = await until(async () => (await request(owner, `/files/${item.file}/ai`, {expected: [200, 404], quiet: true})).data, d => ['indexed', 'failed', 'unsupported'].includes(d?.status), 240000);
      assert.equal(state.status, 'indexed', `${spec.name}: ${state.error_code}`);
      await until(async () => (await request(owner, '/search', {method: 'POST', body: {query: spec.query, mode: 'fulltext', file_ids: [item.file], limit: 5}})).data, r => r.hits.some(h => h.file?.id === item.file));
      results.push({name: spec.name, indexed: true, retrieved: true});
    }
    return results;
  });
  await group('image media worker produces thumbnail and AI vision indexes it', async () => {
    const item = await upload(owner, readFileSync('web/public/logo192.png'), 'vision-proof.png', 'image/png');
    fixture.image = item; persist();
    await until(() => Number(sql(`SELECT count(*) FROM object_variants WHERE object_id=${quote(item.object)} AND status='ready'`)), n => n > 0);
    const preview = (await request(owner, `/files/${item.file}/preview`)).data;
    const response = await fetch(preview.url); assert.equal(response.status, 200); assert.ok((await response.arrayBuffer()).byteLength > 0);
    const state = await until(async () => (await request(owner, `/files/${item.file}/ai`, {quiet: true})).data, d => ['indexed', 'failed', 'unsupported'].includes(d?.status), 240000);
    assert.equal(state.status, 'indexed', state.error_code);
    return {thumbnail: true, vision_status: state.status};
  });
  await group('cross-user search and ask cannot use files absent from that user account', async () => {
    const outsider = await register();
    for (const mode of ['fulltext', 'semantic', 'hybrid']) {
      const r = (await request(outsider, '/search', {method: 'POST', body: {query: '澄海项目', mode, file_ids: [doc.file], limit: 10}})).data;
      assert.equal(r.hits.length, 0);
    }
    const result = await request(outsider, '/ai/ask', {method: 'POST', body: {question: '恢复口令是什么？', file_ids: [doc.file]}, expected: [200, 404, 422]});
    assert.ok(!(result.data?.answer ?? '').includes('ORCHID731')); assert.equal(result.data?.citations?.length ?? 0, 0);
    return {search_modes: 3, leaked_hits: 0, leaked_citations: 0};
  });
} else if (suite === 'retrieval') {
  assert.equal(setting('ai-query', 'SEARCH_EXTERNAL_FALLBACK'), 'false');
  assert.equal(setting('ai-query', 'AI_RERANK_ENABLED'), 'true');
  fixture = JSON.parse(readFileSync(statePath));
  const owner = await login(fixture.owner), doc = fixture.doc;
  await group('three consecutive rounds retrieve the source through name, ES fulltext, Qdrant semantic and hybrid', async () => {
    const rounds = [];
    for (let n = 0; n < 3; n++) {
      const found = {};
      for (const mode of ['name', 'fulltext', 'semantic', 'hybrid']) {
        const r = (await request(owner, '/search', {method: 'POST', body: {query: mode === 'name' ? '中文验收' : '澄海项目', mode, file_ids: [doc.file], limit: 5}})).data;
        assert.ok(r.hits.some(h => h.file?.id === doc.file), `${mode} lost source document`);
        found[mode] = r.hits.length;
      }
      rounds.push(found);
    }
    return {rounds, fallback: false};
  });
  await group('multi-document RAG executes the real reranker and records a successful trace', async () => {
    const trace = randomBytes(16).toString('hex'), parent = randomBytes(8).toString('hex');
    const files = [...new Set(fixture.documents.map(d => d.file))];
    assert.ok(files.length >= 5);
    const answer = (await request(owner, '/ai/ask', {method: 'POST',
      headers: {traceparent: `00-${trace}-${parent}-01`},
      body: {question: '对比澄海项目与瀚海计划的验收资料，澄海项目的恢复口令是什么？', file_ids: files}})).data;
    assert.ok(answer.answer.includes('ORCHID731'));
    assert.ok(answer.citations.length > 0 && answer.citations.every(c => files.includes(c.file_id)));
    const span = await until(async () => {
      try {
        const r = await fetch(`http://127.0.0.1:26686/api/traces/${trace}`), body = await r.json();
        return body.data?.flatMap(t => t.spans).find(s => s.operationName === 'ai.rerank');
      } catch { return null; }
    }, s => Boolean(s), 60000);
    const tags = Object.fromEntries(span.tags.map(t => [t.key, t.value]));
    assert.notEqual(tags.error, true); assert.notEqual(tags['otel.status_code'], 'ERROR');
    assert.ok(tags['ai.candidate_count'] > 1);
    assert.ok(tags['ai.rerank_result_count'] > 0);
    return {trace_id: trace, candidate_count: tags['ai.candidate_count'], result_count: tags['ai.rerank_result_count'],
      model: tags['ai.rerank_model'], citations: answer.citations.length, error: false};
  });
} else if (suite === 'controls') {
  assert.equal(setting('ai-query', 'AI_RATE_LIMIT_ENABLED'), 'true');
  assert.equal(setting('ai-query', 'AI_RATE_LIMIT_REPROCESS_PER_MINUTE'), '3');
  assert.equal(setting('ai-query', 'AI_RATE_LIMIT_SEARCH_PER_MINUTE'), '30');
  const owner = await register();
  const devices = await Promise.all(Array.from({length: 8}, () => login(owner)));
  const doc = await upload(owner, Buffer.from(`并发限流验收：八个设备共享同一账户的处理预算。${randomUUID()}`), 'control-proof.txt', 'text/plain');
  await until(async () => (await request(owner, `/files/${doc.file}/ai`, {expected: [200, 404], quiet: true})).data,
              d => ['indexed', 'failed', 'unsupported'].includes(d?.status), 240000);
  // The limiter uses UTC minute buckets. Keep the burst away from a boundary.
  async function stableWindow() {
    const remaining = 60000 - Date.now() % 60000;
    if (remaining < 15000) await sleep(remaining + 100);
  }
  await group('eight devices share reprocess budget and accepted idempotent requests return one task', async () => {
    await stableWindow();
    const key = randomUUID();
    const jobs = await Promise.all(devices.map(d => request(d, `/files/${doc.file}/ai/reprocess`, {method: 'POST',
      headers: {'Idempotency-Key': key}, expected: [202, 429]})));
    const accepted = jobs.filter(r => r.status === 202);
    assert.equal(accepted.length, 3); assert.equal(jobs.filter(r => r.status === 429).length, 5);
    assert.ok(jobs.filter(r => r.status === 429).every(r => r.data.error.code === 'ai.rate_limited'));
    const ids = accepted.map(r => r.data.id); assert.ok(ids[0]); assert.equal(new Set(ids).size, 1);
    const task = await until(async () => (await request(owner, `/ai/jobs/${ids[0]}`, {quiet: true})).data,
                             j => ['succeeded', 'failed', 'canceled'].includes(j.status), 240000);
    assert.equal(task.status, 'succeeded', task.error_code);
    assert.equal(Number(sql(`SELECT count(*) FROM outbox_events WHERE aggregate_id=${quote(ids[0])}`)), 1);
    return {devices: 8, apis: 2, accepted: 3, throttled: 5, task_ids: 1, outbox_events: 1, task_status: task.status};
  });
  await group('search budget is shared across devices and both API replicas', async () => {
    await stableWindow();
    const replies = await Promise.all(Array.from({length: 36}, (_, n) => request(devices[n % devices.length], '/search', {
      method: 'POST', body: {query: 'no-match-control-proof', mode: 'name', limit: 5}, expected: [200, 429]})));
    assert.equal(replies.filter(r => r.status === 200).length, 30);
    assert.equal(replies.filter(r => r.status === 429).length, 6);
    assert.ok(replies.filter(r => r.status === 429).every(r => r.data.error.code === 'ai.rate_limited'));
    return {requests: 36, accepted: 30, throttled: 6, devices: 8, apis: 2};
  });
} else if (suite === 'faults') {
  assert.equal(setting('ai-query', 'SEARCH_EXTERNAL_FALLBACK'), 'false');
  fixture = JSON.parse(readFileSync(statePath));
  fixture.owner = await login(fixture.owner);
  fixture.devices = await Promise.all(fixture.devices.map(() => login(fixture.owner)));
  persist();
  const {owner, doc} = fixture;
  async function outage(roles, action) {
    const ids = roles.flatMap(containers); assert.ok(ids.length);
    docker('stop', '--time', '5', ...ids);
    try { return await action(); } finally { docker('start', ...ids); }
  }
  await group('broker outage preserves API writes in Outbox and drains after restart', async () => {
    let pending;
    await outage(['rabbitmq'], async () => {
      pending = await create(owner, randomBytes(1000), `broker-outage-${randomUUID()}.bin`);
      await request(owner, '/folders/root');
      assert.ok(Number(sql("SELECT count(*) FROM outbox_deliveries WHERE status <> 'published'")) > 0);
    });
    await until(async () => (await request(owner, `/uploads/${pending.upload.id}`, {quiet: true})).data, s => s.status === 'created');
    await request(owner, `/uploads/${pending.upload.id}`, {method: 'DELETE'});
    return {write_accepted_while_broker_down: true, recovered: true};
  });
  await group('both upload workers stopped: durable work resumes after restart', async () => {
    let pending;
    await outage(['upload-worker'], async () => {
      pending = await create(owner, randomBytes(1000), `upload-worker-outage-${randomUUID()}.bin`);
      const state = (await request(owner, `/uploads/${pending.upload.id}`)).data;
      assert.equal(state.status, 'initializing');
    });
    await until(async () => (await request(owner, `/uploads/${pending.upload.id}`, {quiet: true})).data, s => s.status === 'created');
    await request(owner, `/uploads/${pending.upload.id}`, {method: 'DELETE'});
    return {workers: 2, initializing_during_outage: true, recovered: true};
  });
  await group('one API replica restart preserves the other device session and file access', async () => {
    const id = containers('api')[0];
    docker('stop', '--time', '5', id);
    try {
      const survivor = containers('api')[0];
      const endpoint = `http://${docker('port', survivor, '8081/tcp')}`;
      await request(fixture.devices[6], '/folders/root', {endpoint});
      await request(fixture.devices[7], `/files/${doc.file}`, {endpoint});
    } finally {
      docker('start', id);
      apis = apiEndpoints();
      await until(async () => {
        try { return (await Promise.all(apis.map(endpoint => fetch(`${endpoint}/health/ready`)))).every(r => r.ok); }
        catch { return false; }
      }, ready => ready);
    }
    return {surviving_api_operational: true};
  });
  await group('AI query outage leaves drive available and returns controlled AI unavailability', async () => {
    let commandTask;
    await outage(['ai-query'], async () => {
      await request(owner, '/folders/root');
      await request(owner, `/files/${doc.file}/download-url`);
      await request(owner, '/search', {method: 'POST', body: {query: '澄海项目', mode: 'fulltext', limit: 5}, expected: [503]});
      const command = await request(owner, `/files/${doc.file}/ai/reprocess`, {method: 'POST', headers: {'Idempotency-Key': randomUUID()}});
      commandTask = command.data.id;
      assert.ok(commandTask);
      const done = await until(async () => (await request(owner, `/ai/jobs/${commandTask}`, {quiet: true})).data,
        j => ['succeeded', 'failed', 'canceled'].includes(j.status), 240000);
      assert.equal(done.status, 'succeeded', done.error_code);
    });
    await until(async () => (await request(owner, '/search', {method: 'POST', body: {query: '澄海项目', mode: 'fulltext', file_ids: [doc.file], limit: 5}, expected: [200, 503]})), r => r.status === 200);
    return {drive_available: true, ai_outage_status: 503, local_reprocess_task: commandTask, local_command_succeeded: true, recovered: true};
  });
  await group('external index outage is observable with fallback disabled, then recovers', async () => {
    // Keep the failure probe away from a minute boundary; recovery below
    // explicitly respects throttling while the indexes start.
    const remaining = 60000 - Date.now() % 60000;
    if (remaining < 15000) await sleep(remaining + 100);
    await outage(['elasticsearch', 'qdrant'], async () => {
      await request(owner, '/folders/root');
      await request(owner, '/search', {method: 'POST', body: {query: '澄海项目', mode: 'hybrid', file_ids: [doc.file], limit: 5}, expected: [503]});
    });
    await until(async () => {
      const r = await request(owner, '/search', {method: 'POST', body: {query: '澄海项目', mode: 'hybrid', file_ids: [doc.file], limit: 5}, expected: [200, 429, 503]});
      if (r.status === 429) await sleep(60000 - Date.now() % 60000 + 100);
      else if (r.status !== 200) await sleep(2000);
      return r;
    }, r => r.status === 200 && r.data.hits.some(h => h.file?.id === doc.file), 180000);
    return {fallback: false, outage_status: 503, recovered: true};
  });
} else if (suite === 'fallback') {
  assert.equal(setting('ai-query', 'SEARCH_EXTERNAL_FALLBACK'), 'true');
  assert.equal(setting('ai-query', 'SEARCH_READ_BACKEND'), 'es_qdrant');
  fixture = JSON.parse(readFileSync(statePath));
  const owner = await login(fixture.owner), outsider = await register();
  await group('external indexes offline: PostgreSQL fallback preserves authorized search and grounded answers', async () => {
    const ids = ['elasticsearch', 'qdrant'].flatMap(containers);
    assert.equal(ids.length, 2);
    docker('stop', '--time', '5', ...ids);
    try {
      for (const mode of ['fulltext', 'semantic', 'hybrid']) {
        const r = (await request(owner, '/search', {method: 'POST', body: {query: '澄海项目', mode, file_ids: [fixture.doc.file], limit: 5}})).data;
        assert.ok(r.hits.some(h => h.file?.id === fixture.doc.file), `${mode} fallback lost the authorized file`);
        const denied = (await request(outsider, '/search', {method: 'POST', body: {query: '澄海项目', mode, file_ids: [fixture.doc.file], limit: 5}})).data;
        assert.equal(denied.hits.length, 0);
      }
      const answer = (await request(owner, '/ai/ask', {method: 'POST', body: {question: '澄海项目的恢复口令是什么？', file_ids: [fixture.doc.file]}})).data;
      assert.ok(answer.answer.includes('ORCHID731'));
      assert.ok(answer.citations.length > 0 && answer.citations.every(c => c.file_id === fixture.doc.file));
      return {search_modes: 3, leaked_hits: 0, grounded_answer: true, fallback: true};
    } finally { docker('start', ...ids); }
  });
  await group('external indexes recover while fallback remains enabled', async () => {
    await until(async () => {
      try {
        const r = await fetch('http://127.0.0.1:29200/_cluster/health');
        const s = await r.json();
        const q = await fetch('http://127.0.0.1:29333/healthz');
        return r.ok && !s.unassigned_primary_shards && q.ok;
      } catch { return false; }
    }, ready => ready, 180000);
    const r = (await request(owner, '/search', {method: 'POST', body: {query: '澄海项目', mode: 'hybrid', file_ids: [fixture.doc.file], limit: 5}})).data;
    assert.ok(r.hits.some(h => h.file?.id === fixture.doc.file));
    return {elasticsearch_available: true, qdrant_available: true, search_successful: true};
  });
} else if (suite === 'audit') {
  await group('database references, logical usage and reservations match authoritative rows', async () => {
    const refs = Number(sql('SELECT count(*) FROM file_objects o WHERE o.reference_count <> (SELECT count(*) FROM file_entries f WHERE f.object_id=o.id AND f.deleted_at IS NULL)'));
    const quota = Number(sql('SELECT count(*) FROM users u WHERE u.used_logical_bytes <> COALESCE((SELECT sum(o.size_bytes) FROM file_entries f JOIN file_objects o ON o.id=f.object_id WHERE f.owner_id=u.id AND f.deleted_at IS NULL),0) OR u.reserved_bytes <> COALESCE((SELECT sum(s.size_bytes) FROM upload_sessions s WHERE s.user_id=u.id AND s.status IN (\'initializing\',\'created\',\'uploading\',\'completing\',\'verifying\')),0)'));
    assert.equal(refs, 0); assert.equal(quota, 0); return {reference_mismatches: refs, quota_mismatches: quota};
  });
  await group('outbox delivery, worker receipts and all six queues converge without terminal failures', async () => {
    await until(() => Number(sql("SELECT count(*) FROM outbox_deliveries WHERE transport='rabbitmq' AND status<>'published'")), n => n === 0, 180000);
    await until(() => Number(sql("SELECT count(*) FROM outbox_deliveries d WHERE d.transport='rabbitmq' AND d.status='published' AND NOT EXISTS (SELECT 1 FROM task_results r WHERE r.task_id=d.event_id::text || ':' || d.destination AND r.status='succeeded')")), n => n === 0, 180000);
    assert.equal(Number(sql('SELECT count(*) FROM task_failures')), 0);
    const queues = await until(() => JSON.parse(docker('exec', `${project}-rabbitmq-1`, 'rabbitmqctl', '-q', 'list_queues', '--formatter=json', 'name', 'messages', 'consumers')), rows => rows.every(r => r.messages === 0));
    for (const name of ['upload', 'object', 'media', 'maintenance', 'ai', 'search']) assert.ok(queues.some(q => q.name === name && q.consumers > 0), `missing consumer ${name}`);
    return {queues: queues.length, main_queues: 6, messages: 0, terminal_failures: 0};
  });
} else {
  throw new Error(`unknown suite ${suite}`);
}
report.finished_at = new Date().toISOString();
report.result = report.groups.every(g => g.pass) ? 'PASS' : 'FAIL';
save(); console.log(`${suite}: ${report.result} (${report.groups.filter(g => g.pass).length}/${report.groups.length})`);
process.exitCode = report.result === 'PASS' ? 0 : 1;
