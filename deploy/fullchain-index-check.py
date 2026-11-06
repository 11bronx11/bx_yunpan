"""Compare actual ES/Qdrant records against PostgreSQL authoritative rows."""
import json
import math
import os
from pathlib import Path
import subprocess
import time
import urllib.request

project = os.environ.get('FULLCHAIN_PROJECT', 'bx-fullchain-20261106')
assert project.startswith('bx-fullchain-')
output = Path(os.environ.get('FULLCHAIN_OUTPUT', 'artifacts/fullchain-20261106'))
target = os.environ.get('FULLCHAIN_INDEX_EPOCH', 'fc20261106')

def sql(query):
    return json.loads(subprocess.check_output([
        'docker', 'exec', project + '-postgres-1', 'sh', '-c',
        'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"',
        'probe', query,
    ], text=True))

def http(url, body=None):
    request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                     headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.load(response)

def compare():
    chunks = sql("""SELECT coalesce(json_agg(row_to_json(x)), '[]') FROM (
      SELECT c.id::text AS chunk_id, c.document_version_id::text, d.object_id::text,
             c.content, c.embedding::text AS vector
      FROM ai_chunks c JOIN ai_documents d ON d.id=c.document_id
      JOIN file_objects o ON o.id=d.object_id
      WHERE c.document_version_id=d.active_version_id AND d.status='indexed'
        AND o.status='ready' AND o.reference_count > 0) x""")
    files = sql("""SELECT coalesce(json_agg(row_to_json(x)), '[]') FROM (
      SELECT f.id::text AS file_entry_id, f.name, f.version AS file_version,
             f.object_id::text, f.owner_id::text, f.folder_id::text
      FROM file_entries f JOIN file_objects o ON o.id=f.object_id
      WHERE f.deleted_at IS NULL AND o.status='ready') x""")
    expected_chunks = {r['chunk_id']: r for r in chunks}
    expected_files = {r['file_entry_id']: r for r in files}
    query = {'size': 10000, 'query': {'term': {'tombstone': False}}}
    es_chunks_result = http(f'http://127.0.0.1:29200/chunks_{target}/_search', query)
    assert not es_chunks_result.get('timed_out') and es_chunks_result['_shards']['failed'] == 0
    es_chunks = {h['_source']['chunk_id']: h['_source'] for h in es_chunks_result['hits']['hits']}
    es_file_result = http(f'http://127.0.0.1:29200/files_{target}/_search', query)
    assert not es_file_result.get('timed_out') and es_file_result['_shards']['failed'] == 0
    es_files = {h['_source']['file_entry_id']: h['_source'] for h in es_file_result['hits']['hits']}
    points, offset = [], None
    while True:
        body = {'limit': 256, 'with_payload': True, 'with_vector': True,
                'filter': {'must': [{'key': 'tombstone', 'match': {'value': False}}]}}
        if offset is not None:
            body['offset'] = offset
        result = http(f'http://127.0.0.1:29333/collections/chunks_{target}/points/scroll?consistency=all', body)['result']
        points.extend(result['points'])
        offset = result.get('next_page_offset')
        if offset is None:
            break
    qdrant = {p['payload']['chunk_id']: p for p in points}
    differences = {
        'es_missing_chunks': sorted(expected_chunks.keys() - es_chunks.keys()),
        'es_extra_chunks': sorted(es_chunks.keys() - expected_chunks.keys()),
        'qdrant_missing_chunks': sorted(expected_chunks.keys() - qdrant.keys()),
        'qdrant_extra_chunks': sorted(qdrant.keys() - expected_chunks.keys()),
        'es_missing_files': sorted(expected_files.keys() - es_files.keys()),
        'es_extra_files': sorted(es_files.keys() - expected_files.keys()),
        'content_or_version_errors': [], 'vector_errors': [], 'file_metadata_errors': [],
    }
    for cid, expected in expected_chunks.items():
        if cid in es_chunks:
            actual = es_chunks[cid]
            if any(actual[k] != expected[k] for k in ['object_id', 'document_version_id', 'content']):
                differences['content_or_version_errors'].append('es:' + cid)
        if cid in qdrant:
            actual = qdrant[cid]
            if any(actual['payload'][k] != expected[k] for k in ['object_id', 'document_version_id']):
                differences['content_or_version_errors'].append('qdrant:' + cid)
            a, b = json.loads(expected['vector']), actual['vector']
            cosine = sum(x*y for x, y in zip(a, b)) / (math.sqrt(sum(x*x for x in a)) * math.sqrt(sum(x*x for x in b)))
            if len(a) != 1024 or len(b) != 1024 or cosine < 0.999999:
                differences['vector_errors'].append(cid)
    for fid, expected in expected_files.items():
        if fid in es_files and any(es_files[fid].get(k) != value for k, value in expected.items()):
            differences['file_metadata_errors'].append(fid)
    if len(points) != len(qdrant):
        differences['vector_errors'].append('duplicate live chunk IDs')
    return {'result': 'PASS' if not any(differences.values()) else 'FAIL',
            'target': target, 'pg_active_chunks': len(chunks), 'es_active_chunks': len(es_chunks),
            'qdrant_active_chunks': len(points), 'pg_active_files': len(files), 'es_active_files': len(es_files),
            'vector_dimension': 1024, 'differences': differences}

deadline = time.monotonic() + 120
while True:
    report = compare()
    if report['result'] == 'PASS' or time.monotonic() >= deadline:
        break
    time.sleep(1)
output.mkdir(exist_ok=True, parents=True)
(output / 'index-coverage.json').write_text(json.dumps(report, indent=2) + '\n')
print(json.dumps(report, indent=2))
raise SystemExit(0 if report['result'] == 'PASS' else 1)
