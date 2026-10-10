import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { performance } from 'node:perf_hooks';
import { setTimeout as sleep } from 'node:timers/promises';

const database = process.env.AUDIT_MIGRATION_DATABASE ?? `lock22_${Date.now()}`;
assert.match(database, /^lock22_[0-9]+$/);
const container = 'bx-yunpan-hc-postgres-1';
const dir = process.env.AUDIT_ARTIFACT_DIR ?? 'artifacts/distributed-fixes/20260912';
mkdirSync(dir, { recursive: true });
const args = db => ['exec', '-i', container, 'psql', '-XAt', '-v', 'ON_ERROR_STOP=1', '-U', 'hc', '-d', db];
const sql = (text, db = database) => execFileSync('docker', args(db), { input: text, encoding: 'utf8', timeout: 120000 });
const asyncSQL = text => {
  const child = spawn('docker', args(database), { stdio: ['pipe', 'pipe', 'pipe'] });
  let output = '';
  child.stdout.on('data', chunk => { output += chunk; });
  child.stderr.on('data', chunk => { output += chunk; });
  child.stdin.end(text);
  const done = new Promise(resolve => child.on('close', code => resolve({ code, output })));
  return { child, done };
};
if (!process.env.AUDIT_MIGRATION_DATABASE) sql(`CREATE DATABASE ${database};`, 'hc');
const dsn = `postgres://hc:hc-local-only@127.0.0.1:15432/${database}?sslmode=disable`;
for (const command of (process.env.AUDIT_MIGRATION_DATABASE ? [] : ['up', 'down-one'])) {
  execFileSync('./bin/hc-migrate', [command], { cwd: 'backend', env: { ...process.env, POSTGRES_PRIMARY_DSN: dsn }, stdio: 'pipe' });
}
sql(`
INSERT INTO users(id, username, username_normalized, password_hash) VALUES (md5('lock22-user')::uuid, 'lock22-user', 'lock22-user', 'test') ON CONFLICT DO NOTHING;
INSERT INTO folders(id, owner_id, name, name_normalized) VALUES (md5('lock22-folder')::uuid, md5('lock22-user')::uuid, '/', '/') ON CONFLICT DO NOTHING;
INSERT INTO file_objects(id, sha256, size_bytes, mime_type, bucket, object_key, status, reference_count)
 SELECT md5('obj-'||i)::uuid, md5(i::text)||md5(i::text), 1, 'text/plain', 'fixture', i::text, 'ready', 1 FROM generate_series(1,100000) i ON CONFLICT DO NOTHING;
INSERT INTO file_entries(id, owner_id, folder_id, object_id, name, name_normalized)
 SELECT md5('file-'||i)::uuid, md5('lock22-user')::uuid, md5('lock22-folder')::uuid, md5('obj-'||i)::uuid, 'document-'||i, 'document-'||i FROM generate_series(1,100000) i ON CONFLICT DO NOTHING;
INSERT INTO outbox_events(id, aggregate_type, aggregate_id, event_type, event_version, payload, status)
 SELECT md5('event-'||i)::uuid, 'file_entry', md5('file-'||i)::uuid, 'file.created.v1', 1, '{}', 'published' FROM generate_series(1,100000) i ON CONFLICT DO NOTHING;
INSERT INTO outbox_deliveries(event_id, transport, destination, status)
 SELECT id, 'asynq', 'file.created.v1@maintenance', 'published' FROM outbox_events ON CONFLICT DO NOTHING;
ANALYZE;
`);
const migration = readFileSync('backend/migrations/000022_hot_path_indexes.up.sql', 'utf8');
const report = { database, rows_per_table: 100000, started_at: new Date().toISOString(), unchanged_migration: '000022_hot_path_indexes.up.sql' };
const running = asyncSQL(`SET application_name='audit_migration_22'; ${migration}`);
let migrationDone = false;
running.done.then(() => { migrationDone = true; });
const started = performance.now();
const writes = [];
const locks = [];
while (!migrationDone) {
  const found = sql("SELECT mode FROM pg_locks WHERE relation='outbox_deliveries'::regclass AND granted AND pid IN (SELECT pid FROM pg_stat_activity WHERE application_name='audit_migration_22');").trim();
  if (found) locks.push(found);
  const before = performance.now();
  const result = await asyncSQL("SET statement_timeout='500ms'; UPDATE outbox_deliveries SET attempt=attempt+1 WHERE event_id=md5('event-1')::uuid;").done;
  writes.push({ elapsed_ms: performance.now() - before, code: result.code, output: result.output });
  if (performance.now() - started > 60000) { running.child.kill('SIGTERM'); throw new Error('migration exceeded observation budget'); }
  await sleep(10);
}
const result = await running.done;
assert.equal(result.code, 0, result.output);
report.migration_observation_ms = performance.now() - started;
report.write_probes = writes;
report.observed_locks = [...new Set(locks)];
report.indexes_valid = sql("SELECT bool_and(indisvalid) FROM pg_index WHERE indexrelid IN (SELECT oid FROM pg_class WHERE relname IN ('idx_file_entries_name_trgm','idx_outbox_deliveries_ready','idx_outbox_deliveries_lease'));").trim();
assert.equal(report.indexes_valid, 't');
report.conclusion = writes.some(row => row.code !== 0) ? 'Actual migration blocked a concurrent write beyond the 500ms statement timeout. Use a maintenance window; this is not an online-migration pass.' : 'Migration completed in this sample; measured write latency and locks do not establish a production online-migration bound.';
writeFileSync(`${dir}/migration22-lock-results.json`, JSON.stringify(report, null, 2)+'\n');
console.log(JSON.stringify(report));
