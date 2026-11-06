"""Run real-dependency regression in a separate database on the fullchain stack."""
import collections
import json
import os
from pathlib import Path
import subprocess
import sys
import time

root = Path(__file__).resolve().parent.parent
os.chdir(root)
project = os.environ.get('FULLCHAIN_PROJECT', 'bx-fullchain-20261106')
if not project.startswith('bx-fullchain-'):
    raise SystemExit('Dedicated test project required')
env_file = Path(os.environ.get('FULLCHAIN_ENV_FILE', 'deploy/.env.fullchain-20261106'))
values = dict(line.split('=', 1) for line in env_file.read_text().splitlines()
              if line and not line.startswith('#') and '=' in line)
output = root / os.environ.get('FULLCHAIN_OUTPUT', 'artifacts/fullchain-20261106')
output.mkdir(exist_ok=True, parents=True)

def docker(*args, **kwargs):
    return subprocess.run(['docker', *args], check=True, **kwargs)

dsn = ''
env = os.environ.copy()
venv = env.get('FULLCHAIN_PYTHON_BIN', '/tmp/bx-ai-chain-gapk1mlx/venv/bin')
env.update({
    'PATH': venv + ':' + env['PATH'], 'GOMAXPROCS': '2',
    'PYTHONDONTWRITEBYTECODE': '1',
    'TEST_POSTGRES_DSN': dsn.replace('@postgres:5432/', '@127.0.0.1:26432/'),
    'TEST_MINIO_ENDPOINT': '127.0.0.1:29100',
    'TEST_MINIO_ACCESS_KEY': values['MINIO_ROOT_USER'],
    'TEST_MINIO_SECRET_KEY': values['MINIO_ROOT_PASSWORD'],
    'TEST_RABBITMQ_URL': f"amqp://{values['RABBITMQ_USER']}:{values['RABBITMQ_PASSWORD']}@127.0.0.1:26672/",
    'TEST_RABBITMQ_MANAGEMENT_URL': 'http://127.0.0.1:26673',
    'TEST_RABBITMQ_CONTAINER': project + '-rabbitmq-1',
    'TEST_STABILITY_DURATION': '5s',
    'TEST_SEARCH_ES_ENDPOINT': 'http://127.0.0.1:29200',
    'TEST_SEARCH_QDRANT_ENDPOINT': 'http://127.0.0.1:29333',
    'TEST_CHINESE_PARSER_WORKER': '1',
    'TEST_REDIS_ADDR': '127.0.0.1:27379',
    'TEST_REDIS_FAULTS': '1',
})
go = env.get('FULLCHAIN_GO_BIN', '/home/bronx/.local/go/bin/go')
jobs = [
    ('backend-tests', [go, 'test', '-json', '-p=1', '-count=1', '-timeout=15m', './...']),
    ('backend-vet', [go, 'vet', '-p=1', './...']),
    ('backend-race', [go, 'test', '-race', '-json', '-p=1', '-count=1', '-timeout=15m',
                      './internal/identity', './internal/upload', './internal/drive',
                      './internal/storageops', './internal/media', './internal/outbox',
                      './internal/taskqueue', './internal/search/...', './internal/ai/...']),
]
jobs.append(('backend-sharing-race', [go, 'test', '-race', '-json', '-p=1', '-count=3', '-timeout=5m', './internal/sharing']))
selected = sys.argv[1:]
if 'backend-recheck' in selected:
    jobs.append(('backend-recheck', [go, 'test', '-json', '-p=1', '-count=3', '-timeout=5m',
                                   '-run=^TestDispatcherRealRabbitCrashBeforeACK$', './internal/outbox']))
if 'backend-race-recheck' in selected:
    jobs.append(('backend-race-recheck', [go, 'test', '-race', '-json', '-p=1', '-count=1', '-timeout=15m',
                                        '-run=^Test(ExternalContainers|ExternalLiveRebuildAndDocumentRetirement|ChineseParserToSearchPipeline)$',
                                        './internal/search', './internal/ai']))
jobs = [job for job in jobs if not selected or job[0] in selected]
report_path = output / 'go-verification.json'
report = json.loads(report_path.read_text()) if report_path.exists() else {}
for name, command in jobs:
    print('Running ' + name, flush=True)
    database = None
    if name != 'backend-vet':
        # Clone a clean migrated database per package. Some fixtures retain
        # diagnostic operations which global maintenance scans would see.
        database = 'regression_' + name.removeprefix('backend-') + '_' + str(time.time_ns())
        docker('exec', project + '-postgres-1', 'sh', '-c',
               'exec createdb -U "$POSTGRES_USER" "$1"', 'probe', database)
        dsn = f"postgres://{values['POSTGRES_USER']}:{values['POSTGRES_PASSWORD']}@postgres:5432/{database}?sslmode=disable"
        with (output / (name + '-migration.log')).open('w') as log:
            docker('run', '--rm', '--network', project + '_default', '--env-file', str(env_file),
                   '-e', 'POSTGRES_PRIMARY_DSN=' + dsn, '-e', 'POSTGRES_DSN=' + dsn,
                   'bx-yunpan-migrate:' + values['APP_IMAGE_TAG'], 'up', stdout=log, stderr=subprocess.STDOUT)
    path = output / (name + ('.jsonl' if '-json' in command else '.log'))
    package_results = []
    with path.open('w') as log:
        if name == 'backend-vet':
            result = subprocess.run(command, cwd=root / 'backend', env=env, stdout=log, stderr=subprocess.STDOUT)
            exit_code = result.returncode
        else:
            selector_index = next(i for i, arg in enumerate(command) if arg.startswith('./'))
            packages = subprocess.check_output([go, 'list', '-f', '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}', *command[selector_index:]],
                                               cwd=root / 'backend', env=env, text=True).splitlines()
            packages = [p for p in packages if p]
            for index, package in enumerate(packages):
                package_db = database + '_' + str(index)
                docker('exec', project + '-postgres-1', 'sh', '-c',
                       'exec createdb -U "$POSTGRES_USER" -T "$1" "$2"', 'probe', database, package_db)
                package_dsn = dsn.replace('/' + database + '?', '/' + package_db + '?')
                env['TEST_POSTGRES_DSN'] = package_dsn.replace('@postgres:5432/', '@127.0.0.1:26432/')
                print('  ' + package, flush=True)
                result = subprocess.run(command[:selector_index] + [package], cwd=root / 'backend', env=env, stdout=log, stderr=subprocess.STDOUT)
                evidence = {'package': package, 'database': package_db, 'exit_code': result.returncode}
                if result.returncode == 0:
                    # Retain diagnostics compactly without accumulating live
                    # databases on the small test host. Failed fixtures stay live.
                    archive_dir = output / 'temporary-db-archives'
                    archive_dir.mkdir(exist_ok=True, mode=0o700)
                    archive = archive_dir / (package_db + '.dump')
                    with archive.open('wb') as dump:
                        archive.chmod(0o600)
                        docker('exec', project + '-postgres-1', 'sh', '-c',
                               'exec pg_dump -Fc -U "$POSTGRES_USER" -d "$1"', 'archive', package_db, stdout=dump)
                    docker('exec', project + '-postgres-1', 'sh', '-c',
                           'exec dropdb -U "$POSTGRES_USER" "$1"', 'archive', package_db)
                    evidence['archive'] = str(archive)
                    evidence['database_archived'] = True
                package_results.append(evidence)
            exit_code = max((p['exit_code'] for p in package_results), default=0)
    counts = collections.Counter()
    failures, skipped = [], []
    if '-json' in command:
        for line in path.read_text().splitlines():
            try:
                item = json.loads(line)
            except ValueError:
                continue
            if item.get('Test') and item.get('Action') in ('pass', 'fail', 'skip'):
                counts[item['Action']] += 1
                if item['Action'] == 'fail':
                    failures.append(item['Package'] + ':' + item['Test'])
                if item['Action'] == 'skip':
                    skipped.append(item['Package'] + ':' + item['Test'])
    report[name] = {'exit_code': exit_code, 'database_template': database, 'packages': package_results, 'counts_including_subtests': dict(counts), 'failures': failures, 'skipped': skipped}
    report_path.write_text(json.dumps(report, indent=2) + '\n')
    print(name + ': ' + json.dumps(report[name]), flush=True)
sys.exit(0 if all(report[name]['exit_code'] == 0 for name, _ in jobs) else 1)
