import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import { Trend, Counter } from 'k6/metrics';

const base = __ENV.BASE_URL || 'http://127.0.0.1:8081';
const tokens = (__ENV.TOKENS || '').split(',').filter(Boolean);
const searchLatency = new Trend('ai_search_latency', true);
const askLatency = new Trend('ai_ask_latency', true);
const searchErrors = new Counter('ai_search_errors');
const askErrors = new Counter('ai_ask_errors');

export const options = {
  scenarios: {
    search: { executor: 'shared-iterations', vus: 3, iterations: 12, maxDuration: '90s', startTime: '0s' },
    ask: { executor: 'shared-iterations', vus: 3, iterations: 5, maxDuration: '180s', startTime: '2s' },
  },
  thresholds: {
    http_req_failed: ['rate<0.05'],
    ai_search_latency: ['p(95)<5000'],
    ai_ask_latency: ['p(95)<120000'],
  },
};

function token() { return tokens[(__VU - 1) % Math.max(tokens.length, 1)] || ''; }

export default function () {
  const headers = { Authorization: `Bearer ${token()}`, 'Content-Type': 'application/json' };
  if (exec.scenario.name === 'search') {
    const started = Date.now();
    const response = http.post(`${base}/api/v1/search`, JSON.stringify({ query: 'RAG 检索向量索引', mode: 'hybrid', limit: 8 }), { headers });
    searchLatency.add(Date.now() - started);
    if (response.status !== 200) searchErrors.add(1);
    check(response, { 'AI search HTTP 200': r => r.status === 200 });
    return;
  }
  const started = Date.now();
  const response = http.post(`${base}/api/v1/ai/ask`, JSON.stringify({ question: 'RAG 检索链路使用了什么索引和融合方式？' }), { headers, timeout: '150s' });
  askLatency.add(Date.now() - started);
  if (response.status !== 200) askErrors.add(1);
  check(response, { 'AI ask HTTP 200': r => r.status === 200 });
}
