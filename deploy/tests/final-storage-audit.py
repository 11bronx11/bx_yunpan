"""Read-only audit of bytes and multipart uploads owned by final acceptance users."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import boto3
from botocore.config import Config

os.umask(0o077)
out = Path(sys.argv[1])
fixture = json.loads((out / 'private-fixture.json').read_text())
owners = [u['id'] for u in fixture['users']]
quoted = ','.join("'" + x.replace("'", "''") + "'" for x in owners)
env = dict(x.split('=', 1) for x in json.loads(subprocess.check_output([
    'docker', 'inspect', '--format', '{{json .Config.Env}}', 'bx-yunpan-api-1'])))
client = boto3.client('s3', endpoint_url='http://127.0.0.1:9000',
    aws_access_key_id=env['S3_ACCESS_KEY'], aws_secret_access_key=env['S3_SECRET_KEY'],
    region_name=env.get('S3_REGION', 'us-east-1'),
    config=Config(signature_version='s3v4', connect_timeout=5, read_timeout=30))

def sql(query):
    wrapped = "SELECT coalesce(json_agg(q),'[]'::json) FROM (" + query + ') q'
    return json.loads(subprocess.check_output(['docker', 'exec', 'bx-yunpan-postgres-1',
        'sh', '-c', 'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"', 'probe', wrapped]))

report = {'at': datetime.datetime.now(datetime.timezone.utc).isoformat(), 'objects': [],
          'staging': [], 'multipart': [], 'historical_reference_mismatches': []}
try:
    objects = sql(f"SELECT DISTINCT o.id,o.bucket,o.object_key,o.sha256,o.size_bytes,o.status,o.reference_count,o.gc_eligible_at FROM file_objects o JOIN file_entries f ON f.object_id=o.id WHERE f.owner_id IN ({quoted})")
    for obj in objects:
        if obj['status'] == 'deleted':
            continue
        response = client.get_object(Bucket=obj['bucket'], Key=obj['object_key'])
        digest, size = hashlib.sha256(), 0
        for block in response['Body'].iter_chunks(chunk_size=1024 * 1024):
            digest.update(block)
            size += len(block)
        response['Body'].close()
        assert size == obj['size_bytes'], obj['id'] + ' size mismatch'
        assert digest.hexdigest() == obj['sha256'], obj['id'] + ' SHA256 mismatch'
        report['objects'].append(dict(id=obj['id'], status=obj['status'], bytes=size,
            sha256=digest.hexdigest(), references=obj['reference_count'], gc_eligible_at=obj['gc_eligible_at']))
    bucket = env['S3_BUCKET']
    for owner in owners:
        prefix = 'staging/' + owner + '/'
        for page in client.get_paginator('list_objects_v2').paginate(Bucket=bucket, Prefix=prefix):
            report['staging'].extend(dict(key=x['Key'], bytes=x['Size']) for x in page.get('Contents', []))
        for page in client.get_paginator('list_multipart_uploads').paginate(Bucket=bucket, Prefix=prefix):
            report['multipart'].extend(dict(key=x['Key'], initiated=x['Initiated'].isoformat()) for x in page.get('Uploads', []))
    assert not report['staging'], 'owned staging objects did not converge'
    assert not report['multipart'], 'owned multipart uploads did not converge'
    report['historical_reference_mismatches'] = sql("SELECT id,bucket,status,reference_count,created_at,(SELECT count(*) FROM file_entries f WHERE f.object_id=o.id AND f.deleted_at IS NULL) AS actual FROM file_objects o WHERE reference_count<>(SELECT count(*) FROM file_entries f WHERE f.object_id=o.id AND f.deleted_at IS NULL)")
    report['status'] = 'PASS'
except Exception as error:
    report.update(status='FAIL', error=str(error))
finally:
    (out / 'storage-audit.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps({'status': report['status'], 'objects': len(report['objects']),
        'bytes': sum(o['bytes'] for o in report['objects']), 'staging': len(report['staging']),
        'multipart': len(report['multipart']), 'error': report.get('error')}, ensure_ascii=False))
if report['status'] != 'PASS':
    sys.exit(1)
