// smoke.js — sanity test. 1 VU, 30s, проверяет что все 4 эндпоинта работают.
// Запуск: BASE_URL=http://<vm>:8080 k6 run loadtest/smoke.js

import { sleep } from 'k6';
import { discoverRestaurants, readSearch, readRestaurant, createOrder, initiatePayment, getOrder } from './lib.js';

export const options = {
    vus: 1,
    duration: '30s',
    thresholds: {
        checks: ['rate>0.95'],
        http_req_failed: ['rate<0.05'],
        http_req_duration: ['p(95)<1500'],
    },
};

export function setup() {
    const ids = discoverRestaurants(20);
    if (ids.length === 0) throw new Error('seed не накатился? нет ресторанов');
    return { ids };
}

export default function (data) {
    readSearch();
    readRestaurant(data.ids);
    const orderResp = createOrder(data.ids);
    if (orderResp && orderResp.status >= 200 && orderResp.status < 300) {
        const orderID = orderResp.json('order_id');
        initiatePayment(orderID);
        getOrder(orderID);
    }
    sleep(1);
}
