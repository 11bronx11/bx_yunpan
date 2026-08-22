import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';

const base = __ENV.BASE_URL || 'http://127.0.0.1:8081';
const token = __ENV.TOKEN || '';
const folderLatency = new Trend('folder_latency', true);
const folderErrors = new Counter('folder_errors');

export const options = {
  scenarios: {
    health: { executor: 'constant-arrival-rate', rate: 100, timeUnit: '1s', duration: '10s', preAllocatedVUs: 20, maxVUs: 40 },
    folders: { executor: 'constant-arrival-rate', rate: 60, timeUnit: '1s', duration: '10s', preAllocatedVUs: 20, maxVUs: 40, startTime: '1s' },
  },
  thresholds: { http_req_failed: ['rate<0.01'], folder_latency: ['p(95)<1000'] },
};

export default function () {
  if (exec.scenario.name === 'health') {
    const response = http.get(`${base}/health/live`);
    check(response, { 'health HTTP 200': r => r.status === 200 });
    return;
  }
  const headers = { Authorization: `Bearer ${token}` };
  const started = Date.now();
  const root = http.get(`${base}/api/v1/folders/root`, { headers });
  folderLatency.add(Date.now() - started);
  if (root.status !== 200) folderErrors.add(1);
  check(root, { 'root HTTP 200': r => r.status === 200 });
  if (root.status === 200) {
    const id = root.json('id');
    const children = http.get(`${base}/api/v1/folders/${id}/children`, { headers });
    if (children.status !== 200) folderErrors.add(1);
    check(children, { 'children HTTP 200': r => r.status === 200 });
  }
}
