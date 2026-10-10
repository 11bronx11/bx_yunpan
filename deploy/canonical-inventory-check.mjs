import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';

const project = process.env.AUDIT_PROJECT ?? 'bx-yunpan';
const bucket = process.env.AUDIT_BUCKET ?? 'yunpan';
const dir = process.env.AUDIT_ARTIFACT_DIR ?? 'artifacts/distributed-fixes/20260912';
assert.match(project, /^[a-z0-9][a-z0-9-]+$/);
assert.match(bucket, /^[a-z0-9][a-z0-9.-]+$/);
const docker = (...args) => {
  try {
    return execFileSync('docker', args, { encoding: 'utf8', timeout: 120000, maxBuffer: 64 * 1024 * 1024 });
  } catch {
    // Provider errors can contain credential-bearing URLs.
    throw new Error('Inventory command failed; no result can be certified');
  }
};
const query = `SELECT json_build_object(
  'objects', COALESCE((SELECT json_agg(r ORDER BY r.id) FROM
    (SELECT id, bucket, object_key, status, size_bytes FROM file_objects) r), '[]'),
  'variants', COALESCE((SELECT json_agg(r ORDER BY r.object_key) FROM
    (SELECT o.bucket, v.object_key, v.status FROM object_variants v JOIN file_objects o ON o.id=v.object_id) r), '[]'),
  'sessions', COALESCE((SELECT json_agg(r ORDER BY r.id) FROM
    (SELECT id, bucket, object_key, status FROM upload_sessions) r), '[]'),
  'operations', COALESCE((SELECT json_agg(r ORDER BY r.id) FROM
    (SELECT id, operation_type, target_bucket, target_key, state FROM storage_operations WHERE operation_type='canonical.ensure') r), '[]')
);`;
const metadata = () => JSON.parse(docker('exec', `${project}-postgres-1`, 'sh', '-c',
  'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"', 'audit', query));
const before = metadata();
// Credentials stay inside the existing MinIO container and are never written to an alias file.
const raw = docker('exec', `${project}-minio-1`, 'sh', '-c',
  'export MC_HOST_audit="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@127.0.0.1:9000"; exec mc --json ls --recursive "audit/$1"', 'audit', bucket);
const entries = raw.trim() ? raw.trim().split('\n').map(line => JSON.parse(line)) : [];
assert.ok(entries.every(row => row.status === 'success'), 'incomplete provider inventory');
const after = metadata();
const stable = JSON.stringify(before) === JSON.stringify(after);
const files = entries.filter(row => row.type === 'file');
const keys = new Map(files.map(row => [row.key, row]));
assert.equal(keys.size, files.length, 'duplicate inventory keys');
const objects = new Map(after.objects.filter(row => row.bucket === bucket).map(row => [row.object_key, row]));
const variants = new Map(after.variants.filter(row => row.bucket === bucket).map(row => [row.object_key, row]));
const sessions = new Map(after.sessions.filter(row => row.bucket === bucket).map(row => [row.object_key, row]));
const findings = [];
const counts = { physical_objects: files.length, canonical_ready: 0, variants_ready: 0, pending_gc: 0, staging: 0 };
for (const item of files) {
  const object = objects.get(item.key);
  const variant = variants.get(item.key);
  const session = sessions.get(item.key);
  if (object?.status === 'ready') {
    counts.canonical_ready++;
    if (Number(object.size_bytes) !== item.size) findings.push({ kind: 'size_mismatch', key: item.key });
  } else if (object?.status === 'deleting') counts.pending_gc++;
  else if (variant?.status === 'ready') counts.variants_ready++;
  else if (session && ['initializing', 'created', 'uploading', 'completing', 'verifying'].includes(session.status)) counts.staging++;
  else {
    const operations = after.operations.filter(op => op.target_bucket === bucket && op.target_key === item.key)
      .map(op => ({ id: op.id, state: op.state }));
    findings.push({ kind: object?.status === 'deleted' ? 'deleted_object_present' : 'untracked_or_terminal_object', key: item.key, size_bytes: item.size, operations });
  }
}
for (const object of objects.values()) {
  if (object.status === 'ready' && !keys.has(object.object_key)) findings.push({ kind: 'ready_canonical_missing', key: object.object_key });
}
for (const variant of variants.values()) {
  if (variant.status === 'ready' && !keys.has(variant.object_key)) findings.push({ kind: 'ready_variant_missing', key: variant.object_key });
}
const report = {
  checked_at: new Date().toISOString(), project, bucket, read_only: true,
  metadata_stable_during_scan: stable, counts, findings,
  result: !stable ? 'INCONCLUSIVE' : findings.length ? 'FINDINGS' : 'PASS',
  boundary: 'Current completed objects in one bucket; no historical versions, incomplete multipart uploads, content checksums, or distributed atomic snapshot.',
};
mkdirSync(dir, { recursive: true });
writeFileSync(`${dir}/canonical-inventory.json`, JSON.stringify(report, null, 2) + '\n');
console.log(JSON.stringify(report));
if (report.result !== 'PASS') process.exitCode = 1;
