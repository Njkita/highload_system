// stress.js — постепенный рост нагрузки до отказа. По 2 минуты на ступеньку,
// чтобы p99 был представительным (за 30 сек — это шум, не p99).
//
// Запуск: BASE_URL=http://<vm>:8080 k6 run loadtest/stress.js

import { Trend, Rate } from 'k6/metrics';
import { discoverRestaurants, readSearch, readRestaurant, createOrder, initiatePayment } from './lib.js';

const readDuration = new Trend('read_duration', true);
const writeDuration = new Trend('write_duration', true);
const errors = new Rate('errors');

export const options = {
    scenarios: {
        reads: {
            executor: 'ramping-arrival-rate',
            timeUnit: '1s',
            preAllocatedVUs: 50,
            maxVUs: 400,
            startRate: 50,
            stages: [
                { target: 100, duration: '2m' },
                { target: 200, duration: '2m' },
                { target: 400, duration: '2m' },
                { target: 600, duration: '2m' },
                { target: 800, duration: '2m' },
            ],
            exec: 'reads',
        },
        writes: {
            executor: 'ramping-arrival-rate',
            timeUnit: '1s',
            preAllocatedVUs: 20,
            maxVUs: 100,
            startRate: 10,
            stages: [
                { target: 20,  duration: '2m' },
                { target: 40,  duration: '2m' },
                { target: 80,  duration: '2m' },
                { target: 120, duration: '2m' },
                { target: 160, duration: '2m' },
            ],
            exec: 'writes',
        },
    },
    thresholds: {
        'http_req_failed': ['rate<0.05'],
    },
};

export function setup() {
    const ids = discoverRestaurants(30);
    return { ids };
}

export function reads(data) {
    const r = Math.random();
    let resp;
    if (r < 0.5)      resp = readSearch();
    else              resp = readRestaurant(data.ids);
    if (resp) {
        readDuration.add(resp.timings.duration);
        errors.add(resp.status >= 500);
    }
}

export function writes(data) {
    const orderResp = createOrder(data.ids);
    if (!orderResp) { errors.add(true); return; }
    writeDuration.add(orderResp.timings.duration);
    errors.add(orderResp.status >= 400);
    if (orderResp.status === 201 || orderResp.status === 200) {
        const orderID = orderResp.json('order_id');
        const pay = initiatePayment(orderID);
        if (pay) writeDuration.add(pay.timings.duration);
    }
}
