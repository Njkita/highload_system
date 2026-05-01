// spike.js — 2x спайк по сценарию из задания: error rate < 5%, нет OOM/крашей,
// восстановление за минуту после пика.
//
// Профиль: 1 минута базы → 30 сек спайк (×2) → 1 минута recovery.
// Запуск: BASE_URL=http://<vm>:8080 k6 run loadtest/spike.js

import { Rate, Trend } from 'k6/metrics';
import { discoverRestaurants, readSearch, readRestaurant, createOrder } from './lib.js';

const errors = new Rate('errors');
const readDuration = new Trend('read_duration', true);

const BASE_RPS = parseInt(__ENV.BASE_RPS || '100');

export const options = {
    scenarios: {
        reads: {
            executor: 'ramping-arrival-rate',
            timeUnit: '1s',
            preAllocatedVUs: 50,
            maxVUs: 400,
            startRate: BASE_RPS,
            stages: [
                { target: BASE_RPS,     duration: '1m' },   // baseline
                { target: BASE_RPS * 2, duration: '15s' },  // спайк
                { target: BASE_RPS * 2, duration: '15s' },  // держим
                { target: BASE_RPS,     duration: '1m' },   // recovery
            ],
            exec: 'reads',
        },
        writes: {
            executor: 'ramping-arrival-rate',
            timeUnit: '1s',
            preAllocatedVUs: 20,
            maxVUs: 100,
            startRate: 30,
            stages: [
                { target: 30, duration: '1m' },
                { target: 60, duration: '15s' },
                { target: 60, duration: '15s' },
                { target: 30, duration: '1m' },
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
    const resp = Math.random() < 0.5 ? readSearch() : readRestaurant(data.ids);
    if (resp) {
        readDuration.add(resp.timings.duration);
        errors.add(resp.status >= 500);
    }
}

export function writes(data) {
    const r = createOrder(data.ids);
    if (r) errors.add(r.status >= 500);
}
