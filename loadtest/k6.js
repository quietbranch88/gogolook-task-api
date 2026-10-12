import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

// Load test: token mode, 20 tenants, 3000 tasks each (docs/DESIGN.md#load-test,
// synthetic accumulation fixture). MODE=slo|ceiling; LIMIT="" (default page) or a page size; SKEW=1 sends
// half of all traffic to the first tenant.
const BASE = __ENV.BASE || 'http://127.0.0.1:8080';
const MODE = __ENV.MODE || 'slo';
const LIMIT = __ENV.LIMIT || '';
const SKEW = __ENV.SKEW === '1';
const TENANTS = JSON.parse(open(__ENV.TOKENS || './tokens.json'));

const listBytes = new Trend('list_bytes'); // bytes of each GET /tasks response
// k6 otherwise considers 3xx successful, contrary to the written response contract.
http.setResponseCallback(http.expectedStatuses({ min: 200, max: 299 }));

function pick() {
  if (SKEW && Math.random() < 0.5) return TENANTS[0];
  return TENANTS[Math.floor(Math.random() * TENANTS.length)];
}
function hdr(t, json) {
  const h = { Authorization: `Bearer ${t.token}`, 'X-Tenant-ID': String(t.tenant) };
  if (json) h['Content-Type'] = 'application/json';
  return { headers: h };
}
const listPath = LIMIT ? `/tasks?limit=${LIMIT}` : '/tasks';
const versions = {}; // Per-VU state; targets are never shared by different VUs.

export function setup() {
  // Distinct existing target per VU/tenant keeps latency measurements free of intentional conflicts.
  const ids = {};
  for (const t of TENANTS) {
    const targets = [];
    const needed = MODE === 'slo' ? 200 : 400;
    let after = 0;
    while (targets.length < needed) {
      const res = http.get(`${BASE}/tasks?limit=100&after=${after}`, hdr(t));
      if (res.status !== 200) throw new Error(`setup GET ${t.tenant}: ${res.status}`);
      const page = res.json();
      if (!Array.isArray(page) || page.length === 0 || page.some(x => !Number.isSafeInteger(x.id) || x.id <= after || !Number.isSafeInteger(x.version) || x.version < 1)) {
        throw new Error(`setup requires ${needed} versioned tasks per tenant`);
      }
      targets.push(...page.map(x => ({ id: x.id, version: x.version })));
      after = page[page.length - 1].id;
    }
    ids[t.tenant] = targets.slice(0, needed);
  }
  return ids;
}

// 80% GET : 6.7% POST : 13.23% PUT : 0.07% DELETE
export function mixed(ids) {
  const t = pick();
  const r = Math.random();
  if (r < 0.80) {
    const res = http.get(`${BASE}${listPath}`, hdr(t));
    listBytes.add(res.body ? res.body.length : 0);
    check(res, { 'GET 200': x => x.status === 200 });
    return;
  }
  if (r < 0.867) {
    const res = http.post(`${BASE}/tasks`, JSON.stringify({ name: `load ${__VU}-${__ITER}`, status: 0 }), hdr(t, true));
    check(res, { 'POST 201': x => x.status === 201 });
    return;
  }
  if (r < 0.9993) {
    const target = ids[t.tenant][__VU - 1];
    if (!target) throw new Error('No exclusive PUT target for this VU');
    const version = versions[t.tenant] || target.version;
    const res = http.put(`${BASE}/tasks/${target.id}`, JSON.stringify({ name: `upd ${__VU}-${__ITER}`, status: __ITER % 2, version }), hdr(t, true));
    if (check(res, { 'PUT 200': x => x.status === 200 })) {
      const next = res.json('version');
      if (check(next, { 'PUT version incremented': x => x === version + 1 })) versions[t.tenant] = next;
    }
    return;
  }
  const c = http.post(`${BASE}/tasks`, JSON.stringify({ name: 'tmp', status: 0 }), hdr(t, true));
  if (check(c, { 'DELETE setup POST 201': x => x.status === 201 })) {
    const res = http.del(`${BASE}/tasks/${c.json('id')}`, null, hdr(t));
    check(res, { 'DELETE 204': x => x.status === 204 });
  }
}

const scenarios = {
  // PERF-01/02/03: 50 iterations/s mixed for 60 s at 3000 rows/tenant; thresholds below are the gate
  slo: { s: { executor: 'constant-arrival-rate', rate: 50, timeUnit: '1s', duration: '60s', preAllocatedVUs: 20, maxVUs: 200, exec: 'mixed' } },
  // ceiling: open-model ramp 200 -> 2000 iterations/s
  ceiling: { s: { executor: 'ramping-arrival-rate', startRate: 200, timeUnit: '1s', preAllocatedVUs: 100, maxVUs: 400, exec: 'mixed',
                  stages: [ { target: 600, duration: '20s' }, { target: 1200, duration: '20s' }, { target: 2000, duration: '20s' } ] } },
};

export const options = {
  scenarios: scenarios[MODE],
  // Correctness/delivery gates apply to slo only; latency is reported without a threshold.
  // MODE=slo is retained for command compatibility; the ceiling ramp is exploratory.
  thresholds: MODE === 'slo'
    ? { checks: ['rate==1'], http_req_failed: ['rate==0'], dropped_iterations: ['count==0'] }
    : {},
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max'],
};
