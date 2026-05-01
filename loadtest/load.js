// load.js — устойчивая нагрузка на 5 минут.
// Цель: проверить минимальную планку из задания — read ≥ 100 RPS p99 < 500ms,
// write ≥ 30 RPS p99 < 1s, error rate < 1%.
//
// Профиль read-heavy 80/20:
//   80% — search + restaurant card + get order
//   20% — create order + initiate payment
//
// Параметры:
//   TARGET_READ_RPS  (default 120)  целевой RPS на read-сценарий
//   TARGET_WRITE_RPS (default 30)
//   DURATION         (default 5m)
//
// Запуск: BASE_URL=http://<vm>:8080 k6 run loadtest/load.js
// Посмотреть p99 на конкретный URL: k6 run --summary-trend-stats="avg,p(50),p(95),p(99),max" loadtest/load.js

import { sleep } from 'k6';
import { Trend, Rate } from 'k6/metrics';
import { discoverRestaurants, readSearch, readRestaurant, createOrder, initiatePayment, getOrder } from './lib.js';

const readDuration = new Trend('read_duration', true);
const writeDuration = new Trend('write_duration', true);
const errors = new Rate('errors');

const READ_RPS = parseInt(__ENV.TARGET_READ_RPS || '120');
const WRITE_RPS = parseInt(__ENV.TARGET_WRITE_RPS || '30');
const DURATION = __ENV.DURATION || '5m';

export const options = {
    scenarios: {
        reads: {
            executor: 'constant-arrival-rate',
            rate: READ_RPS,
            timeUnit: '1s',
            duration: DURATION,
            preAllocatedVUs: 50,
            maxVUs: 200,
            exec: 'reads',
        },
        writes: {
            executor: 'constant-arrival-rate',
            rate: WRITE_RPS,
            timeUnit: '1s',
            duration: DURATION,
            preAllocatedVUs: 20,
            maxVUs: 60,
            exec: 'writes',
        },
    },
    thresholds: {
        'http_req_failed': ['rate<0.01'],
        'read_duration': ['p(99)<500'],
        'write_duration': ['p(99)<1000'],
    },
};

export function setup() {
    const ids = discoverRestaurants(30);
    if (ids.length < 5) throw new Error('seed не накатился');
    return { ids };
}

export function reads(data) {
    // три read-операции примерно равными долями
    const r = Math.random();
    let resp;
    if (r < 0.5)      resp = readSearch();
    else if (r < 0.9) resp = readRestaurant(data.ids);
    else              resp = readSearch();
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
        if (pay) {
            writeDuration.add(pay.timings.duration);
            errors.add(pay.status >= 500);
        }
    }
}
