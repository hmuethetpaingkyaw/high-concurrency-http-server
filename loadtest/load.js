// Open-model load test: k6 sends exactly RATE req/s whether or not the
// server keeps up, the way real users do.
//
//   MODE=constant  hold RATE for DURATION        (pass/fail at a given load)
//   MODE=ramp      climb from 10 to RATE         (find the breaking point)
//   MODE=spike     hold RATE far above capacity  (overload: fast 503s are a pass)
import http from 'k6/http';
import { Rate } from 'k6/metrics';

const TARGET = __ENV.TARGET || 'http://app:8080';
const ENDPOINT = __ENV.ENDPOINT || '/io';
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || '1m';
const MODE = __ENV.MODE || 'constant';
const METHOD = __ENV.METHOD || 'GET';

// Level 3 placeholders in ENDPOINT: {id} becomes a product id, {cat} a category.
// Real traffic is skewed: HOT_RATIO of requests go to the HOT_KEYS most
// popular products, the rest are spread over all MAX_ID.
const MAX_ID = Number(__ENV.MAX_ID || 100000);
const HOT_KEYS = Number(__ENV.HOT_KEYS || 1000);
const HOT_RATIO = Number(__ENV.HOT_RATIO || 0.8);
const CATEGORIES = 50;

// VUs needed = RATE x latency (Little's Law). Start with enough for 200ms
// latency and let k6 add more on demand, up to 2s, before it drops iterations.
// Pre-allocating thousands of VUs made k6 itself the bottleneck above 5k RPS.
const maxVUs = Math.max(50, RATE * 2);
const preAllocatedVUs = Math.max(20, Math.ceil(RATE / 5));

// Load shedding answers 503 on purpose, so spike mode judges 503s separately
// and only counts anything else (other codes, timeouts, resets) as a failure.
const shed = new Rate('shed');
const unexpected = new Rate('unexpected');
if (MODE === 'spike') {
  http.setResponseCallback(http.expectedStatuses(200, 503));
}

const scenarios = {
  constant: {
    executor: 'constant-arrival-rate',
    rate: RATE,
    timeUnit: '1s',
    duration: DURATION,
    preAllocatedVUs,
    maxVUs,
  },
  spike: {
    executor: 'constant-arrival-rate',
    rate: RATE,
    timeUnit: '1s',
    duration: DURATION,
    preAllocatedVUs,
    maxVUs,
  },
  ramp: {
    executor: 'ramping-arrival-rate',
    startRate: 10,
    timeUnit: '1s',
    stages: [{ target: RATE, duration: DURATION }],
    preAllocatedVUs,
    maxVUs,
  },
};

export const options = {
  discardResponseBodies: true,
  scenarios: { load: scenarios[MODE] },
  // The SLO. Breaking any of these means "this level failed at this RATE".
  thresholds: MODE === 'spike' ? {
    'http_req_duration{status:200}': ['p(99)<200'], // accepted users still get fast answers
    // Rejections must be as fast as the normal SLO. On a CPU-quota box the
    // kernel pauses the whole process when the quota runs out, 503s included,
    // so near-instant (<20ms) isn't achievable there.
    'http_req_duration{status:503}': ['p(99)<200'],
    unexpected: ['rate<0.001'],
    dropped_iterations: ['count<1'],
  } : {
    http_req_failed: ['rate<0.001'],
    http_req_duration: ['p(99)<200'],
    dropped_iterations: ['count<1'],
  },
};

function pickId() {
  const n = Math.random() < HOT_RATIO ? HOT_KEYS : MAX_ID;
  return 1 + Math.floor(Math.random() * n);
}

export default function () {
  const path = ENDPOINT
    .replace('{id}', pickId())
    .replace('{cat}', `cat-${1 + Math.floor(Math.random() * CATEGORIES)}`);
  // Tag every request with the endpoint template, not its unique URL, or k6
  // creates a new time series per request and its memory grows without bound.
  const params = { timeout: '5s', headers: { 'Content-Type': 'application/json' }, tags: { name: ENDPOINT } };
  let res;
  if (METHOD === 'POST') {
    // POST /orders: buy one unit of a (possibly hot) product.
    res = http.post(`${TARGET}${path}`, JSON.stringify({ product_id: pickId(), qty: 1 }), params);
  } else {
    // A unique query suffix so no two /cpu requests hash the same input.
    const sep = path.includes('?') ? '&' : '?';
    res = http.get(`${TARGET}${path}${sep}r=${__VU}-${__ITER}`, params);
  }
  shed.add(res.status === 503);
  unexpected.add(res.status !== 200 && res.status !== 503);
}
