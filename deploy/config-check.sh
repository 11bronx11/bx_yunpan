#!/usr/bin/env bash
set -euo pipefail
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "$deploy_dir/common.sh"
yunpan_assert_compose_owner "$deploy_dir"
[[ -f "$deploy_dir/.env" ]] || { echo 'Missing deploy/.env' >&2; exit 1; }
# Compare the rendered role-specific model, including all defaults. Never print
# either expected or actual values: some are credentials.
python3 - "$deploy_dir" <<'PY'
import json,os,subprocess,sys
root=sys.argv[1]
base=['docker','compose','--env-file',root+'/.env','-f',root+'/compose.yaml','--profile','*']
rows=subprocess.check_output(base+['ps','--format','json']).decode().strip()
containers=[]
if rows:
    try:
        parsed=json.loads(rows); containers=parsed if isinstance(parsed,list) else [parsed]
    except json.JSONDecodeError: containers=[json.loads(line) for line in rows.splitlines()]
running={r['Service'] for r in containers if r.get('State')=='running'}
env=os.environ.copy()
if 'ai-query' in running: env['AISVC_GRPC_TARGET']='ai-query:8082'
if 'redis-coordination' in running:env.update(REDIS_COORDINATION_ADDR='redis-coordination:6379',REDIS_CACHE_ADDR='redis-cache:6379')
if 'search-indexer' in running:env['SEARCH_INDEX_ENABLED']='true'
if 'elasticsearch' in running:env.update(SEARCH_WRITE_BACKENDS='es_qdrant',SEARCH_ES_ENDPOINT=env.get('SEARCH_ES_ENDPOINT') or 'http://elasticsearch:9200',SEARCH_QDRANT_ENDPOINT=env.get('SEARCH_QDRANT_ENDPOINT') or 'http://qdrant:6333')
model=json.loads(subprocess.check_output(base+['config','--format','json'],env=env))
required={'api','async-control','committer','upload-worker','object-worker','media-worker','gc-worker'}
appenv=model['services']['api']['environment']
if appenv.get('AI_ENABLED','true')=='true':
    required.update({'ai-query','ai-indexer'} if 'ai-query' in running else {'aisvc'})
    if appenv.get('PARSER_REMOTE_ENABLED','true')=='true': required.add('parser-svc')
if appenv.get('SEARCH_INDEX_ENABLED')=='true': required.add('search-indexer')
errors=[]
for service in sorted(required-running): errors.append(service+': not running')
for old in ('worker','outbox-relay','maintenance-worker'):
    if old in running:errors.append(old+': legacy alias running beside new core roles')
for r in containers:
    service=r['Service']
    if service not in required or r.get('State')!='running':continue
    inspected=json.loads(subprocess.check_output(['docker','inspect',r['ID']]))[0]
    actual=dict(x.split('=',1) for x in inspected['Config']['Env'] if '=' in x)
    expected=model['services'][service].get('environment',{})
    for key,value in expected.items():
        if actual.get(key)!=str(value if value is not None else ''):errors.append(r['Name']+': '+key+' differs from rendered configuration')
for error in errors:print(error,file=sys.stderr)
if errors:sys.exit(1)
print('Running role configuration matches deploy/.env and Compose defaults.')
PY
