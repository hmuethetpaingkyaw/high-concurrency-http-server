// Open-model load test: k6 sends exactly RATE req/s whether or not the
// server keeps up, the way real users do.
//
//   MODE=constant  hold RATE for DURATION        (pass/fail at a given load)
//   MODE=ramp      climb from 10 to RATE         (find the breaking point)
import http from 'k6/http';

const TARGET = __ENV.TARGET || 'http://app:8080';
const ENDPOINT = __ENV.ENDPOINT || '/io';
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || '1m';
const MODE = __ENV.MODE || 'constant';

// VUs needed = RATE x latency (Little's Law). Allow up to 2s latency before
// k6 runs out of VUs and starts dropping iterations.
const maxVUs = Math.max(50, RATE * 2);

const scenarios = {
  constant: {
    executor: 'constant-arrival-rate',
    rate: RATE,
    timeUnit: '1s',
    duration: DURATION,
    preAllocatedVUs: Math.ceil(maxVUs / 4),
    maxVUs,
  },
  ramp: {
    executor: 'ramping-arrival-rate',
    startRate: 10,
    timeUnit: '1s',
    stages: [{ target: RATE, duration: DURATION }],
    preAllocatedVUs: Math.ceil(maxVUs / 4),
    maxVUs,
  },
};

export const options = {
  discardResponseBodies: true,
  scenarios: { load: scenarios[MODE] },
  // The SLO. Breaking any of these means "this level failed at this RATE".
  thresholds: {
    http_req_failed: ['rate<0.001'],
    http_req_duration: ['p(99)<200'],
    dropped_iterations: ['count<1'],
  },
};

export default function () {
  http.get(`${TARGET}${ENDPOINT}?${__VU}-${__ITER}`, { timeout: '5s' });
}
