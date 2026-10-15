// Checkout load: VUS concurrent shoppers (10 by default) each check out
// again and again for DURATION as a new visitor: add a stock-tracked item,
// start a checkout, choose shipping, confirm and pay with the fake Stripe.
// Every confirmation must complete the saga.
//
// Run by scripts/load-test.sh, and by scripts/chaos-test.sh with SHOP (one
// shop by name) and SUMMARY (the summary's file name) while it breaks
// things.
import http from 'k6/http';
import { check, fail, sleep } from 'k6';
import exec from 'k6/execution';
import { Counter, Trend } from 'k6/metrics';

const catalog = JSON.parse(open('/results/catalog.json'));
const shops = catalog.shops.filter((s) => !__ENV.SHOP || s.name === __ENV.SHOP);
const BASE = __ENV.BASE_URL || 'http://yc-gateway:8080';

const limited = new Counter('rate_limited');
const completed = new Counter('checkouts_completed');
const notCompleted = new Counter('checkouts_not_completed');
const flow = new Trend('checkout_flow_duration', true);
const confirm = new Trend('checkout_confirm_duration', true);

export const options = {
  scenarios: {
    checkout: {
      executor: 'constant-vus',
      vus: Number(__ENV.VUS || 10),
      duration: __ENV.DURATION || '60s',
    },
  },
  thresholds: {
    checkouts_not_completed: ['count==0'],
    checkout_confirm_duration: ['p(95)<2000'],
    http_req_failed: ['rate<0.01'],
  },
  summaryTrendStats: ['avg', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

const ADDRESS = {
  country_code: 'ZA',
  lines: ['1 Jan Smuts Ave', 'Johannesburg 2196'],
  fields: { street: '1 Jan Smuts Ave', city: 'Johannesburg', province: 'Gauteng', postal_code: '2196' },
};

function uuid() {
  return crypto.randomUUID();
}

export default function () {
  const vu = exec.vu.idInTest;
  const iter = exec.vu.iterationInScenario;
  const shop = shops[vu % shops.length];
  const jar = new http.CookieJar();
  // Every checkout is a new customer with their own address.
  const addr = `10.${100 + (vu & 127)}.${(iter >> 8) & 255}.${iter & 255}`;
  const headers = { Host: shop.host, 'X-Forwarded-For': addr, 'Content-Type': 'application/json' };
  const call = (method, path, body, extra = {}) => {
    for (;;) {
      const res = http.request(method, `${BASE}${path}`, body === undefined ? null : JSON.stringify(body), {
        jar,
        headers: Object.assign({}, headers, extra),
        tags: { name: path.replace(/[0-9a-f-]{36}/g, '{id}') },
      });
      if (res.status !== 429) return res;
      limited.add(1);
      sleep(Number(res.headers['Retry-After'] || 1));
    }
  };
  const must = (res, status, what) => {
    if (!check(res, { [`${what} ${status}`]: (r) => r.status === status })) {
      notCompleted.add(1, { step: what });
      sleep(1); // don't spin while something is down
      fail(`${what}: HTTP ${res.status} ${res.body && res.body.slice(0, 300)}`);
    }
    return res.json();
  };

  const start = Date.now();
  must(call('GET', '/me'), 200, 'visitor');
  const sku = shop.skus[Math.floor(Math.random() * shop.skus.length)];
  must(call('POST', '/carts/shopping/items', { sku_id: sku, quantity: 1 }), 200, 'add to cart');
  const session = must(call('POST', '/checkout', {}), 201, 'start checkout').session_id;
  must(call('POST', `/checkout/${session}/shipping`, { delivery_address: ADDRESS }), 200, 'shipping');
  const t0 = Date.now();
  const done = must(call('POST', `/checkout/${session}/confirm`, {
    payment_gateway_token: 'pm_card_visa',
    guest_contact: { email: `load+${vu}-${exec.vu.iterationInScenario}@load.test`, phone_e164: '+27831234567' },
  }, { 'Idempotency-Key': uuid() }), 200, 'confirm');
  confirm.add(Date.now() - t0);
  if (done.state !== 'COMPLETED') {
    notCompleted.add(1, { step: 'confirm', state: done.state });
    fail(`confirm: state ${done.state}`);
  }
  completed.add(1);
  flow.add(Date.now() - start);
}

export function handleSummary(data) {
  const m = data.metrics;
  const v = (key) => m[key] && m[key].values;
  const ms = (t) => (t ? `p50 ${t.med.toFixed(0)} ms  p95 ${t['p(95)'].toFixed(0)} ms  p99 ${t['p(99)'].toFixed(0)} ms  max ${t.max.toFixed(0)} ms` : '-');
  const count = (key) => (v(key) ? v(key).count : 0);
  const text = `\ncheckout: ${count('checkouts_completed')} completed, ${count('checkouts_not_completed')} not completed, ` +
    `${count('rate_limited')} rate limited` +
    ` (${(count('checkouts_completed') / (data.state.testRunDurationMs / 1000)).toFixed(1)}/s)\n` +
    `  confirm  ${ms(v('checkout_confirm_duration'))}\n` +
    `  flow     ${ms(v('checkout_flow_duration'))}\n` +
    `  http     ${ms(v('http_req_duration'))}, ${(v('http_req_failed').rate * 100).toFixed(2)}% failed\n`;
  return { [`/results/${__ENV.SUMMARY || 'checkout'}.json`]: JSON.stringify(data, null, 2), stdout: text };
}
