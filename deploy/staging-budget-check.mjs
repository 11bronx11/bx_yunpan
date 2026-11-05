import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const deploy = fileURLToPath(new URL('.', import.meta.url));
// An explicit environment prevents the development .env or shell overrides
// from redirecting this local staging allocation to another database.
const raw = execFileSync('docker', [
  'compose', '--project-name', 'bx-yunpan-staging',
  '--env-file', `${deploy}staging.env.example`,
  '-f', `${deploy}compose.yaml`, '-f', `${deploy}compose.staging.yaml`,
  '--profile', 'app', '--profile', 'distributed', '--profile', 'ai-split',
  '--profile', 'parser', '--profile', 'search', '--profile', 'redis-split', 'config', '--format', 'json',
], { encoding: 'utf8', env: { PATH: process.env.PATH, HOME: process.env.HOME } });
const config = JSON.parse(raw);
const integer = (value, minimum = 1) => {
  const parsed = Number(value);
  assert(Number.isSafeInteger(parsed) && parsed >= minimum, 'invalid budget integer');
  return parsed;
};
const postgresSetting = (key) => integer(config.services.postgres.command
  .find((entry) => entry.startsWith(`${key}=`))?.split('=')[1]);
const roles = ['api', 'worker', 'outbox-relay', 'object-worker', 'media-worker',
  'maintenance-worker', 'upload-worker', 'ai-query', 'ai-indexer', 'search-indexer'];
const consumers = new Set(['object-worker', 'media-worker', 'maintenance-worker', 'upload-worker', 'ai-indexer', 'search-indexer']);
const dispatcherRoles = new Set(['worker', 'outbox-relay']);
const rows = [];
for (const role of roles) {
  const service = config.services[role];
  const env = service.environment;
  const replicas = integer(service.deploy?.replicas ?? 0, 0);
  const open = integer(env.POSTGRES_MAX_OPEN_CONNS);
  const idle = integer(env.POSTGRES_MAX_IDLE_CONNS, 0);
  assert(idle <= open, `${role}: idle exceeds open connections`);
  const dsn = new URL(env.POSTGRES_PRIMARY_DSN);
  assert(dsn.hostname === 'postgres' && dsn.pathname === '/hc_staging', `${role}: unexpected database`);
  assert.equal(env.POSTGRES_DSN, env.POSTGRES_PRIMARY_DSN, `${role}: DSN aliases differ`);
  assert.equal(env.AI_PROVIDER, 'fake', `${role}: local staging uses the fake provider`);
  assert(!env.DASHSCOPE_API_KEY, `${role}: unexpected external AI credential`);
  const tasks = consumers.has(role) ? integer(env.WORKER_CONCURRENCY) : null;
  const listenerConnections = dispatcherRoles.has(role) && env.OUTBOX_NOTIFY_ENABLED !== 'false' ? replicas : 0;
  rows.push({ role, replicas, max_open: open, max_idle: idle,
    task_concurrency: tasks, listener_connections: listenerConnections,
    connection_total: replicas * open + listenerConnections });
}
for (const role of ['worker', 'aisvc']) {
  assert.equal(config.services[role]?.deploy?.replicas ?? 0, 0, `${role}: combined role enabled`);
}
const parser = config.services['parser-svc'];
assert(parser, 'parser-svc: parser profile is missing');
assert(integer(parser.deploy?.replicas ?? 0) > 0, 'parser-svc: no replicas allocated');
assert.equal(parser.environment.PARSER_REMOTE_ENABLED, 'true', 'parser-svc: remote parsing disabled');
assert.equal(parser.environment.PARSER_ETCD_SERVICE_PREFIX, '/services/document-parser');
assert.equal(config.services['outbox-relay'].environment.OUTBOX_LEADER_LOCK_ENABLED, 'true');
const app = rows.reduce((sum, row) => sum + row.connection_total, 0);
const max = postgresSetting('max_connections');
const reserved = postgresSetting('superuser_reserved_connections');
const operations = integer(config['x-staging-connection-budget'].operations);
const migration = integer(config['x-staging-connection-budget'].migration);
const spare = max - reserved - operations - migration - app;
assert(spare >= 0, `connection budget exceeded by ${-spare}`);
console.log(JSON.stringify({ project: config.name, roles: rows, connections: {
  postgres_max: max, application: app, migration, operations,
  superuser_reserved: reserved, unallocated: spare,
}, parser: { replicas: integer(parser.deploy.replicas), max_concurrent: integer(parser.environment.PARSER_MAX_CONCURRENT) },
relay: { batch_size: integer(config.services['outbox-relay'].environment.OUTBOX_BATCH_SIZE),
  leader_lock: true,
  notify_enabled: config.services['outbox-relay'].environment.OUTBOX_NOTIFY_ENABLED !== 'false',
  listener_connections: rows
    .filter((row) => dispatcherRoles.has(row.role))
    .reduce((sum, row) => sum + row.listener_connections, 0) }, runtime_verified: false }, null, 2));
