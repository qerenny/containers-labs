import { check } from 'k6';
import http from 'k6/http';

const baseURL = __ENV.BASE_URL || 'http://127.0.0.1:18080';
const orderRate = Number(__ENV.ORDER_RATE || 20);
const testDuration = __ENV.TEST_DURATION || '4m';

export const options = {
  scenarios: {
    orders: {
      executor: 'constant-arrival-rate',
      rate: orderRate,
      timeUnit: '1s',
      duration: testDuration,
      preAllocatedVUs: 10,
      maxVUs: 100,
      gracefulStop: '10s',
    },
  },
  thresholds: {
    'http_req_duration{endpoint:order}': ['p(95)<300'],
    'http_req_failed{endpoint:order}': ['rate<0.01'],
    'checks{endpoint:order}': ['rate>0.99'],
  },
};

export default function () {
  const response = http.post(
    `${baseURL}/order`,
    JSON.stringify({
      source: 'k6',
      virtual_user: __VU,
      iteration: __ITER,
    }),
    {
      headers: { 'Content-Type': 'application/json' },
      tags: { endpoint: 'order' },
    },
  );

  check(
    response,
    { 'order created': (result) => result.status === 201 },
    { endpoint: 'order' },
  );
}
