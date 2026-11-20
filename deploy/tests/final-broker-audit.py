"""Inspect the deployed queue prefix without consuming or mutating messages."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import requests

os.umask(0o077)
out = Path(sys.argv[1])
def env(role):
    container = json.loads(subprocess.check_output(['docker', 'inspect', 'bx-yunpan-' + role + '-1']))[0]
    return dict(x.split('=', 1) for x in container['Config']['Env'])
broker, control = env('rabbitmq'), env('async-control')
prefix = control.get('ASYNC_BROKER_PREFIX', 'bx.async.v2')
session = requests.Session()
session.trust_env = False
report = {'prefix': prefix}
try:
    for attempt in range(6):
        response = session.get('http://127.0.0.1:15672/api/queues',
            auth=(broker['RABBITMQ_DEFAULT_USER'], broker['RABBITMQ_DEFAULT_PASS']), timeout=10)
        response.raise_for_status()
        queues = [{k: q.get(k) for k in ['name', 'vhost', 'messages_ready', 'messages_unacknowledged', 'consumers']}
            for q in response.json() if q['name'].startswith(prefix + '.')]
        if queues and all(q['messages_ready'] == 0 and q['messages_unacknowledged'] == 0 for q in queues):
            break
        time.sleep(2)
    report['queues'] = queues
    assert len(queues) >= 10, 'missing expected deployed queues'
    assert all(q['messages_ready'] == 0 and q['messages_unacknowledged'] == 0 for q in queues), 'queue backlog remains'
    assert all(q['consumers'] >= 1 for q in queues if '.work.' in q['name']), 'missing work consumer'
    report['status'] = 'PASS'
except Exception as error:
    report.update(status='FAIL', error=str(error))
finally:
    (out / 'broker-audit.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))
if report['status'] != 'PASS':
    sys.exit(1)
