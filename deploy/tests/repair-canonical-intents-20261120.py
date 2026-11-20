"""Repair only the three verified canonical intents from the 2026-11-20 acceptance.

Default is read-only. --apply settles metadata using existing failed receipts;
it never reopens tasks, recomputes output, changes quota, or deletes bytes.
"""
import argparse
import datetime
import json
import os
from pathlib import Path
import subprocess

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

TARGETS = [
    ('d36fc7ed-e860-5f2f-b8bd-c8025548b849', '01a1f62b-45cc-7258-b77e-a71c5b139665',
     '2f8d31f2-ba25-5e9a-b4b6-5e0dbdf10be0', '2326b506-0615-5298-bda4-82f045bb0391'),
    ('bd70a73d-b434-52be-bef9-7ddbb7d26109', '01a1f62b-45e7-758e-ab73-69944f35503d',
     '90de6208-649b-56ea-a7f6-2afda1adcf98', 'c856fc2f-0116-5752-998a-f1ef8ca2c679'),
    ('9720b253-a9a5-5a1d-b1d7-8c1bd0c4e601', '01a1f62b-4562-76c6-b2be-b5fcb454e2a0',
     '92bdac86-558c-58d3-a4e9-ac71c92d1eac', 'f78734f5-66ca-5910-9cc1-7aeeea321908'),
]
CODE = 'upload.commit_precondition_failed'


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def quote(value):
    return "'" + str(value).replace("'", "''") + "'"


def psql(role, query):
    return subprocess.check_output([
        'docker', 'exec', '-i', 'bx-yunpan-' + role + '-1', 'sh', '-c',
        'exec psql -XqAt -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"',
    ], input=query, text=True)


def rows(role, query):
    return json.loads(psql(role, "SELECT coalesce(json_agg(q),'[]'::json) FROM (" + query + ') q;'))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--output', required=True)
    args = parser.parse_args()
    os.umask(0o077)
    output = Path(args.output)
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    ids = ','.join(quote(t[0]) for t in TARGETS)
    business_query = f"""SELECT o.*,s.status AS session_status,s.error_code,
        s.reserved_bytes,s.user_id,r.result_id,r.input_digest,r.result_digest,
        r.outcome AS receipt_outcome,r.created_at AS receipt_created_at
        FROM storage_operations o JOIN upload_sessions s ON s.id=o.upload_session_id
        JOIN async_commit_receipts r ON r.semantic_key='canonical.ensure:'||o.id
        WHERE o.id IN ({ids}) ORDER BY o.id"""
    before = rows('postgres', business_query)
    control_query = f"""SELECT t.id,t.kind,t.state,t.outcome,t.protocol_version,t.input,
        t.semantic_key,t.input_digest,r.id AS result_id,r.result_digest,
        r.input_digest AS result_input_digest,r.committed_at,r.body
        FROM tasks t JOIN results r ON r.id=t.result_id AND r.task_id=t.id
        WHERE t.input->>'operation_id' IN ({ids}) ORDER BY t.id"""
    control = rows('async-postgres', control_query)
    require(len(before) == len(TARGETS) == len(control), 'expected exactly three intents, receipts and tasks')
    environment = dict(x.split('=', 1) for x in json.loads(subprocess.check_output([
        'docker', 'inspect', '--format', '{{json .Config.Env}}', 'bx-yunpan-api-1'])))
    client = boto3.client('s3', endpoint_url='http://127.0.0.1:9000',
        aws_access_key_id=environment['S3_ACCESS_KEY'], aws_secret_access_key=environment['S3_SECRET_KEY'],
        region_name=environment.get('S3_REGION', 'us-east-1'),
        config=Config(signature_version='s3v4', connect_timeout=5, read_timeout=15))
    expected, cleanups = [], []
    for operation, session, task_id, result_id in TARGETS:
        op = next(x for x in before if x['id'] == operation)
        task = next(x for x in control if x['id'] == task_id)
        require(op['upload_session_id'] == session and op['operation_type'] == 'canonical.ensure', 'intent identity mismatch')
        require(op['session_status'] == 'failed' and op['error_code'] == CODE and op['reserved_bytes'] == 0, 'session not safely terminal')
        require(op['state'] == 'pending' or (op['state'] == 'failed' and op['last_error'] == CODE and op['completed_at'] is not None), 'intent state changed')
        require(task['state'] == 'done' and task['outcome'] == 'failed' and task['committed_at'] and task['protocol_version'] == 2, 'control result not terminal')
        require(task['kind'] == 'canonical.ensure' and task['input']['operation_id'] == operation and task['input']['session_id'] == session and task['input']['owner_id'] == op['user_id'], 'task input mismatch')
        require(task['semantic_key'] == 'canonical.ensure:' + operation, 'semantic identity mismatch')
        require(op['result_id'] == task['result_id'] == result_id and op['receipt_outcome'] == 'failed', 'receipt identity mismatch')
        require(op['input_digest'] == task['input_digest'] == task['result_input_digest'] and op['result_digest'] == task['result_digest'], 'receipt digest mismatch')
        key, bucket = task['body']['key'], task['body']['bucket']
        require(key.startswith('objects/compute/' + operation + '/' + task_id + '/'), 'private output identity mismatch')
        cleanup = rows('postgres', f"SELECT id,state,operation_type FROM storage_operations WHERE source_bucket={quote(bucket)} AND source_key={quote(key)}")
        require(cleanup and all(c['state'] == 'succeeded' and c['operation_type'] == 'staging.delete' for c in cleanup), 'private cleanup not complete')
        try:
            client.head_object(Bucket=bucket, Key=key)
        except ClientError as error:
            require(error.response['ResponseMetadata']['HTTPStatusCode'] == 404, 'private output check inconclusive')
        else:
            raise RuntimeError('private output still exists')
        cleanups.append({'operation_id': operation, 'cleanup': cleanup, 'private_output_exists': False})
        expected.append({'id': operation, 'session_id': session, 'state': op['state'], 'semantic_key': task['semantic_key'],
            'result_id': result_id, 'input_digest': task['input_digest'], 'result_digest': task['result_digest'],
            'bucket': bucket, 'key': key})
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S%fZ')
    evidence = {'at': stamp, 'apply': args.apply, 'before': before, 'control': control, 'cleanups': cleanups}
    evidence_path = output / ('repair-' + stamp + '.json')
    evidence_path.write_text(json.dumps(evidence, indent=2) + '\n')
    changed = []
    if args.apply:
        changed = json.loads(psql('postgres', f"""
BEGIN;
SET LOCAL lock_timeout='2s'; SET LOCAL statement_timeout='10s';
CREATE TEMP TABLE intent_repair_expected ON COMMIT DROP AS
SELECT * FROM jsonb_to_recordset({quote(json.dumps(expected))}::jsonb) AS x(
 id uuid,session_id uuid,state text,semantic_key text,result_id uuid,input_digest text,result_digest text,bucket text,key text);
DO $$ BEGIN
 PERFORM o.id FROM storage_operations o JOIN intent_repair_expected e ON e.id=o.id
 JOIN upload_sessions s ON s.id=o.upload_session_id
 JOIN async_commit_receipts r ON r.semantic_key=e.semantic_key FOR UPDATE OF o,s,r;
 IF (SELECT count(*) FROM intent_repair_expected e JOIN storage_operations o ON o.id=e.id
 JOIN upload_sessions s ON s.id=e.session_id AND s.id=o.upload_session_id
 JOIN async_commit_receipts r ON r.semantic_key=e.semantic_key
 WHERE o.operation_type='canonical.ensure' AND o.state=e.state
 AND (o.state='pending' OR (o.state='failed' AND o.last_error={quote(CODE)} AND o.completed_at IS NOT NULL))
 AND s.status='failed' AND s.error_code={quote(CODE)} AND s.reserved_bytes=0
 AND r.result_id=e.result_id AND r.input_digest=e.input_digest AND r.result_digest=e.result_digest AND r.outcome='failed'
 AND EXISTS(SELECT 1 FROM storage_operations c WHERE c.source_bucket=e.bucket AND c.source_key=e.key AND c.state='succeeded' AND c.operation_type='staging.delete')
 AND NOT EXISTS(SELECT 1 FROM storage_operations c WHERE c.source_bucket=e.bucket AND c.source_key=e.key AND c.state<>'succeeded')) <> 3
 THEN RAISE EXCEPTION 'intent repair preconditions changed'; END IF;
END $$;
WITH changed AS (
 UPDATE storage_operations o SET state='failed',last_error={quote(CODE)},completed_at=r.created_at,
 updated_at=clock_timestamp(),lease_until=NULL,lease_token=NULL
 FROM intent_repair_expected e,async_commit_receipts r
 WHERE o.id=e.id AND o.state='pending' AND r.semantic_key=e.semantic_key
 RETURNING o.id,o.state,o.last_error,o.completed_at
) SELECT coalesce(json_agg(changed),'[]'::json) FROM changed;
COMMIT;
"""))
        require(len(changed) == sum(x['state'] == 'pending' for x in before), 'unexpected update count')
        after = rows('postgres', business_query)
        require(all(x['state'] == 'failed' and x['last_error'] == CODE and x['completed_at'] and x['lease_until'] is None and x['lease_token'] is None for x in after), 'post-repair state mismatch')
        require(rows('async-postgres', control_query) == control, 'control evidence changed')
        evidence.update(after=after, changed=changed)
    evidence['status'] = 'PASS'
    evidence_path.write_text(json.dumps(evidence, indent=2) + '\n')
    print(json.dumps({'status': 'PASS', 'apply': args.apply, 'verified': len(TARGETS), 'changed': len(changed), 'evidence': str(evidence_path)}))


if __name__ == '__main__':
    main()
