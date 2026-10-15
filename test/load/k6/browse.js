// Browse load: RATE requests a second on /products across the shops in
// /results/catalog.json, for DURATION. Each virtual user is one shopper
// with its own address (X-Forwarded-For through a trusted proxy) and its
// own visitor cookie, so the per-visitor limits apply as in production.
//
// Run by scripts/load-test.sh.
import http from 'k6/http';
import { check } from 'k6';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';

const limited = new Counter('rate_limited');
const failures = new Counter('server_errors');

const catalog = JSON.parse(open('/results/catalog.json'));
const BASE = __ENV.BASE_URL || 'http://yc-gateway:8080';
const RATE = Number(__ENV.RATE || 500);
const WORDS = ['kettle', 'mug', 'lamp', 'chair', 'shirt', 'book'];

export const options = {
  scenarios: {
    browse: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: __ENV.DURATION || '60s',
      preAllocatedVUs: Number(__ENV.VUS || 400),
      maxVUs: Number(__ENV.MAX_VUS || 800),
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    'http_req_duration{kind:list}': ['p(95)<300', 'p(99)<800'],
    'http_req_duration{kind:search}': ['p(95)<400', 'p(99)<1000'],
    'http_req_duration{kind:detail}': ['p(95)<300', 'p(99)<800'],
    checks: ['rate>0.99'],
  },
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

function shopper() {
  const vu = exec.vu.idInTest;
  return {
    shop: catalog.shops[vu % catalog.shops.length],
    addr: `10.${(vu >> 16) & 255}.${(vu >> 8) & 255}.${vu & 255}`,
  };
}

export default function () {
  const { shop, addr } = shopper();
  const params = (kind) => ({ headers: { Host: shop.host, 'X-Forwarded-For': addr }, tags: { kind, shop: shop.host } });
  const roll = Math.random();
  let res;
  if (roll < 0.5) {
    res = http.get(`${BASE}/products`, params('list'));
  } else if (roll < 0.7) {
    res = http.get(`${BASE}/products?sort=price_asc&page_size=20`, params('list'));
  } else if (roll < 0.85) {
    res = http.get(`${BASE}/products?q=${WORDS[Math.floor(Math.random() * WORDS.length)]}`, params('search'));
  } else {
    const id = shop.products[Math.floor(Math.random() * shop.products.length)];
    res = http.get(`${BASE}/products/${id}`, params('detail'));
  }
  check(res, { 'status 200': (r) => r.status === 200 });
  if (res.status === 429) limited.add(1);
  if (res.status >= 500) failures.add(1);
}

export function handleSummary(data) {
  return { '/results/browse.json': JSON.stringify(data, null, 2), stdout: textSummary(data) };
}

function textSummary(data) {
  const m = data.metrics;
  const line = (name, key) => {
    const v = m[key] && m[key].values;
    return v ? `  ${name.padEnd(8)} p50 ${v.med.toFixed(1)} ms  p95 ${v['p(95)'].toFixed(1)} ms  p99 ${v['p(99)'].toFixed(1)} ms  max ${v.max.toFixed(1)} ms\n` : '';
  };
  const reqs = m.http_reqs.values;
  const failed = m.http_req_failed.values.rate * 100;
  const count = (key) => (m[key] ? m[key].values.count : 0);
  return `\nbrowse: ${reqs.count} requests, ${reqs.rate.toFixed(0)}/s, ${failed.toFixed(2)}% failed ` +
    `(${count('rate_limited')} rate limited, ${count('server_errors')} 5xx)\n` +
    line('list', 'http_req_duration{kind:list}') +
    line('search', 'http_req_duration{kind:search}') +
    line('detail', 'http_req_duration{kind:detail}') +
    `  thresholds ${Object.values(data.metrics).every((v) => !v.thresholds || Object.values(v.thresholds).every((t) => t.ok)) ? 'passed' : 'FAILED'}\n`;
}
