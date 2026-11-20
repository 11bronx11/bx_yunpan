"""Expose terminal uploads whose legacy intent still advertises pending work."""
import json
import os
from pathlib import Path
import subprocess
import sys
import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

os.umask(0o077)
out = Path(sys.argv[1])
fixture = json.loads((out / 'private-fixture.json').read_text())
owners = ','.join("'" + u['id'] + "'" for u in fixture['users'])
def sql(role, query):
    wrapped = "SELECT coalesce(json_agg(q),'[]'::json) FROM (" + query + ') q'
    return json.loads(subprocess.check_output(['docker', 'exec', 'bx-yunpan-' + role + '-1',
        'sh', '-c', 'exec psql -XAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "$1"', 'probe', wrapped]))
intents = sql('postgres', f"SELECT o.id,o.upload_session_id,o.operation_type,o.state,o.available_at,s.status AS upload_status,s.error_code,s.reserved_bytes,s.filename FROM storage_operations o JOIN upload_sessions s ON s.id=o.upload_session_id WHERE s.user_id IN ({owners}) AND s.status IN ('completed','failed','aborted','expired') AND o.operation_type='canonical.ensure' AND o.state='pending'")
env = dict(x.split('=', 1) for x in json.loads(subprocess.check_output(['docker', 'inspect', '--format', '{{json .Config.Env}}', 'bx-yunpan-api-1'])))
client = boto3.client('s3', endpoint_url='http://127.0.0.1:9000', aws_access_key_id=env['S3_ACCESS_KEY'],
    aws_secret_access_key=env['S3_SECRET_KEY'], region_name=env.get('S3_REGION', 'us-east-1'),
    config=Config(signature_version='s3v4', connect_timeout=5, read_timeout=20))
for intent in intents:
    intent['tasks'] = sql('async-postgres', f"SELECT t.id,t.kind,t.state,t.outcome,r.id AS result_id,r.body FROM tasks t LEFT JOIN results r ON r.id=t.result_id WHERE t.kind='canonical.ensure' AND t.input->>'operation_id'='{intent['id']}'")
    for task in intent['tasks']:
        body = task.pop('body') or {}
        key, bucket = body.get('key'), body.get('bucket', env['S3_BUCKET'])
        if key:
            task['private_output_key'] = key
            try:
                client.head_object(Bucket=bucket, Key=key)
                task['private_output_exists'] = True
            except ClientError as error:
                if error.response['ResponseMetadata']['HTTPStatusCode'] != 404:
                    raise
                task['private_output_exists'] = False
report = {'status': 'FAIL' if intents else 'PASS', 'pending_terminal_intents': intents}
(out / 'intent-audit.json').write_text(json.dumps(report, indent=2) + '\n')
print(json.dumps(report))
if intents:
    sys.exit(1)
