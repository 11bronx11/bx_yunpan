import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';

const base = __ENV.BASE_URL || 'http://127.0.0.1:8081';
const token = __ENV.TOKEN || '';
const endpointLatency = new Trend('product_endpoint_latency', true);
const endpointErrors = new Counter('product_endpoint_errors');

export const options = {
  scenarios: {
    core: { executor: 'constant-arrival-rate', rate: 40, timeUnit: '1s', duration: '15s', preAllocatedVUs: 20, maxVUs: 60 },
  },
  thresholds: { http_req_failed: ['rate<0.01'], product_endpoint_latency: ['p(95)<1000'] },
};

function get(path, headers) {
  const started = Date.now();
  const response = http.get(`${base}${path}`, { headers });
  endpointLatency.add(Date.now() - started, { endpoint: path.split('?')[0] });
  if (response.status !== 200) endpointErrors.add(1, { endpoint: path.split('?')[0] });
  return response;
}

export default function () {
  const headers = { Authorization: `Bearer ${token}` };
  const root = get('/api/v1/folders/root', headers);
  const id = root.status === 200 ? root.json('id') : '';
  const children = id ? get(`/api/v1/folders/${id}/children`, headers) : null;
  const quota = get('/api/v1/users/me/quota', headers);
  const uploads = get('/api/v1/uploads?status=active', headers);
  const shares = get('/api/v1/shares', headers);
  check(root, { 'root 200': r => r.status === 200 });
  if (children) check(children, { 'children 200': r => r.status === 200 });
  check(quota, { 'quota 200': r => r.status === 200 });
  check(uploads, { 'uploads 200': r => r.status === 200 });
  check(shares, { 'shares 200': r => r.status === 200 });
}
