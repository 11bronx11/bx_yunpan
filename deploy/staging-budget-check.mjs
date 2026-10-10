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
  '--profile', 'search', '--profile', 'redis-split', 'config', '--format', 'json',
], { encoding: 'utf8', env: { PATH: process.env.PATH, HOME: process.env.HOME } });
const config = JSON.parse(raw);
const integer = (value, minimum = 1) => {
  const parsed = Number(value);
  assert(Number.isSafeInteger(parsed) && parsed >= minimum, 'invalid budget integer');
  return parsed;
};
const postgresSetting = (key) => integer(config.services.postgres.command
  .find((entry) => entry.startsWith(`${key}=`))?.split('=')[1]);
const roles = ['api', 'outbox-relay', 'object-worker', 'media-worker',
  'maintenance-worker', 'ai-query', 'ai-indexer', 'search-indexer'];
const consumers = new Set(['object-worker', 'media-worker', 'maintenance-worker', 'ai-indexer', 'search-indexer']);
const rows = [];
for (const role of roles) {
  const service = config.services[role];
  const env = service.environment;
  const replicas = integer(service.deploy.replicas);
  const open = integer(env.POSTGRES_MAX_OPEN_CONNS);
  const idle = integer(env.POSTGRES_MAX_IDLE_CONNS, 0);
  assert(idle <= open, `${role}: idle exceeds open connections`);
  const dsn = new URL(env.POSTGRES_PRIMARY_DSN);
  assert(dsn.hostname === 'postgres' && dsn.pathname === '/hc_staging', `${role}: unexpected database`);
  assert.equal(env.POSTGRES_DSN, env.POSTGRES_PRIMARY_DSN, `${role}: DSN aliases differ`);
  assert.equal(env.AI_PROVIDER, 'fake', `${role}: local staging uses the fake provider`);
  assert(!env.DASHSCOPE_API_KEY, `${role}: unexpected external AI credential`);
  const tasks = consumers.has(role) ? integer(env.WORKER_CONCURRENCY) : null;
  rows.push({ role, replicas, max_open: open, max_idle: idle,
    task_concurrency: tasks, connection_total: replicas * open });
}
for (const role of ['worker', 'aisvc']) {
  assert.equal(config.services[role]?.deploy?.replicas ?? 0, 0, `${role}: combined role enabled`);
}
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
}, relay: { batch_size: integer(config.services['outbox-relay'].environment.OUTBOX_BATCH_SIZE),
  leader_lock: true }, runtime_verified: false }, null, 2));
