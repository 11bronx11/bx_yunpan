import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';

const apis = (process.env.HC_API_URLS ?? 'http://127.0.0.1:18081,http://127.0.0.1:18082').split(',');
assert.equal(apis.length, 2, 'two independent API instances are required');
for (const api of apis) {
  assert.equal(new URL(api).hostname, '127.0.0.1', 'smoke creates fixtures and must target the isolated local APIs');
}
let token;
async function request(api, path, method = 'GET', body, headers = {}) {
  const response = await fetch(`${api}/api/v1${path}`, {
    method, signal: AbortSignal.timeout(5000),
    headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}), ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  assert.ok(response.ok, `${method} ${path}: HTTP ${response.status}`);
  return response.status === 204 ? null : response.json();
}

const session = await request(apis[0], '/auth/register', 'POST', {
  username: `hc-${randomUUID().slice(0, 8)}`, password: `Hc-${randomUUID()}!`,
});
token = session.access_token;
assert.ok(token);
const root = await request(apis[0], '/folders/root');
const children = `/folders/${root.id}/children`;
await request(apis[1], children);
const folder = await request(apis[0], '/folders', 'POST', { parent_id: root.id, name: 'before' });
assert.equal((await request(apis[1], children)).items[0]?.name, 'before');
await request(apis[1], children);
const renamed = await request(apis[0], `/folders/${folder.id}`, 'PATCH', { name: 'after' }, { 'If-Match': String(folder.version) });
const reads = await Promise.all(Array.from({ length: 12 }, () => request(apis[1], children)));
assert.ok(reads.every(result => result.items.length === 1 && result.items[0].name === 'after'));
await request(apis[0], `/folders/${folder.id}`, 'DELETE', undefined, { 'If-Match': String(renamed.version) });
assert.equal((await request(apis[1], children)).items.length, 0);

const metrics = await fetch(`${apis[1]}/metrics`, { signal: AbortSignal.timeout(5000) }).then(r => r.text());
assert.match(metrics, /yunpan_cache_lookups_total\{result="l1_hit"\} [1-9]/);
assert.match(metrics, /yunpan_cache_source_loads_total\{result="success"\} [1-9]/);
assert.match(metrics, /go_sql_in_use_connections/);
console.log(JSON.stringify({
  result: 'PASS', replicas: apis.length, concurrent_reads: reads.length,
  checks: ['cross-replica create', 'cross-replica rename', 'cross-replica delete', 'cache metrics', 'database pool metrics'],
  scope: 'functional HTTP smoke, not a capacity measurement',
}));
console.log(metrics.split('\n').filter(line => /^(yunpan_cache_|go_sql_)/.test(line)).join('\n'));
