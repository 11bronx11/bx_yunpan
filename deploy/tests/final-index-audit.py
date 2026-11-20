"""Read-only comparison of owned active AI versions with real ES/Qdrant data."""
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import requests

os.umask(0o077)
out = Path(sys.argv[1])
fixture = json.loads((out / 'private-fixture.json').read_text())

def sql(role, query):
    query = "SELECT coalesce(json_agg(q),'[]'::json) FROM (" + query + ') q'
    return json.loads(subprocess.check_output(['docker', 'exec', 'bx-yunpan-' + role + '-1',
        'sh', '-c', 'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"', 'probe', query]))

owners = ','.join("'" + u['id'] + "'" for u in fixture['users'])
objects = sql('postgres', f'SELECT DISTINCT object_id FROM file_entries WHERE owner_id IN ({owners})')
ids = ','.join("'" + o['object_id'] + "'" for o in objects)
config = json.loads(subprocess.check_output(['docker', 'inspect', 'bx-yunpan-ai-text-projector-1']))[0]
env = dict(x.split('=', 1) for x in config['Config']['Env'])
session = requests.Session()
session.trust_env = False

def endpoint(role, port):
    container = json.loads(subprocess.check_output(['docker', 'inspect', 'bx-yunpan-' + role + '-1']))[0]
    address = next(iter(container['NetworkSettings']['Networks'].values()))['IPAddress']
    return f'http://{address}:{port}'

es, qd = endpoint('elasticsearch', 9200), endpoint('qdrant', 6333)

def query(backend, path, body):
    headers = {'api-key': env['SEARCH_QDRANT_API_KEY']} if backend == 'qd' and env.get('SEARCH_QDRANT_API_KEY') else {}
    auth = (env['SEARCH_ES_USERNAME'], env.get('SEARCH_ES_PASSWORD', '')) if backend == 'es' and env.get('SEARCH_ES_USERNAME') else None
    response = session.post((es if backend == 'es' else qd) + path,
        json=body, headers=headers, auth=auth, timeout=20)
    response.raise_for_status()
    return response.json()

report = {'es': [], 'qdrant': []}
try:
    active = sql('ai-postgres', f"SELECT o.object_id,o.active_content_version_id,o.active_embedding_set_id,p.backend,p.target_version FROM ai_objects o CROSS JOIN ai_projection_targets p WHERE o.object_id IN ({ids}) AND o.active_content_version_id IS NOT NULL AND p.state='active'")
    for obj in active:
        cid, sid, target = obj['active_content_version_id'], obj['active_embedding_set_id'], obj['target_version']
        children = sql('ai-postgres', f"SELECT id,parent_id,content FROM ai_child_chunks WHERE content_version_id='{cid}'")
        if obj['backend'] == 'es':
            data = query('es', '/' + target + '/_search', {'size': 1000, 'query': {'term': {'content_id': cid}}})
            hits = data['hits']['hits']
            assert len(hits) == len(children), 'ES active child count'
            indexed = {h['_source']['child_id']: h['_source'] for h in hits}
            for child in children:
                hit = indexed[child['id']]
                assert hit['parent_id'] == child['parent_id'] and hit['content_id'] == cid and hit['content'] == child['content'], 'ES active child identity/content'
            report['es'].append(dict(object_id=obj['object_id'], children=len(hits), content_id=cid))
        elif sid:
            data = query('qd', '/collections/' + target + '/points/scroll', {'limit': 1000, 'with_payload': True, 'with_vector': True,
                'filter': {'must': [{'key': 'set_id', 'match': {'value': sid}}]}})
            points = data['result']['points']
            vectors = sql('ai-postgres', f"SELECT child_id,embedding::text AS embedding FROM ai_chunk_vectors WHERE embedding_set_id='{sid}'")
            assert len(points) == len(vectors), 'Qdrant active vector count'
            indexed = {p['payload']['child_id']: p for p in points}
            minimum_cosine = 1.0
            for vector in vectors:
                point = indexed[vector['child_id']]
                assert point['payload']['content_id'] == cid and point['payload']['set_id'] == sid
                expected, actual = json.loads(vector['embedding']), point['vector']
                assert len(expected) == len(actual) == 1024, 'Qdrant dimension'
                dot = sum(a*b for a, b in zip(expected, actual))
                cosine = dot / math.sqrt(sum(a*a for a in expected) * sum(b*b for b in actual))
                minimum_cosine = min(minimum_cosine, cosine)
                assert cosine >= 0.999999, 'Qdrant vector values'
            report['qdrant'].append(dict(object_id=obj['object_id'], vectors=len(points), set_id=sid, minimum_cosine=minimum_cosine))
    report['status'] = 'PASS'
except Exception as error:
    report.update(status='FAIL', error=str(error))
finally:
    (out / 'index-audit.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))
if report['status'] != 'PASS':
    sys.exit(1)
