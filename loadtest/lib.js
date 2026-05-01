// Общие утилиты для k6-сценариев. Все сценарии тянут BASE_URL из env (default — localhost:8080).
// Профиль трафика — read-heavy: 80% read (search + restaurant card + order status),
// 20% write (create order + initiate payment).
//
// Setup() прогревает кэш каталога и возвращает ~30 реальных restaurant_id, которые
// дальше переиспользуются виртуальными пользователями. Без этого тестировался бы
// один и тот же ресторан, и Redis-кэш дал бы 100% hit rate, что нечестно показывает
// эффект iter-2.

import http from 'k6/http';
import { check } from 'k6';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

export const BASE = __ENV.BASE_URL || 'http://127.0.0.1:8080';

const MOSCOW_LAT = 55.75;
const MOSCOW_LON = 37.62;

// Возвращает массив restaurant_id для использования в сценариях.
// Параллельно прогревает кэш каталога: первый запрос — MISS, последующие — HIT.
export function discoverRestaurants(target = 30) {
    const ids = new Set();
    let page = 1;
    while (ids.size < target && page < 6) {
        const r = http.get(`${BASE}/api/v1/restaurants?lat=${MOSCOW_LAT}&lon=${MOSCOW_LON}&radius=20000&per_page=20&page=${page}`);
        check(r, { 'discover 200': (resp) => resp.status === 200 });
        if (r.status !== 200) break;
        const items = (r.json('items') || []);
        for (const it of items) ids.add(it.restaurant_id);
        if (items.length < 20) break;
        page++;
    }
    return [...ids];
}

export function readSearch() {
    // jitter в координатах, чтобы кэш-ключ варьировался у разных VU
    const lat = MOSCOW_LAT + (Math.random() - 0.5) * 0.2;
    const lon = MOSCOW_LON + (Math.random() - 0.5) * 0.3;
    const radius = [3000, 5000, 10000][Math.floor(Math.random() * 3)];
    const r = http.get(`${BASE}/api/v1/restaurants?lat=${lat.toFixed(3)}&lon=${lon.toFixed(3)}&radius=${radius}`);
    check(r, { 'search 2xx': (resp) => resp.status === 200 });
    return r;
}

export function readRestaurant(restaurantIDs) {
    const id = restaurantIDs[Math.floor(Math.random() * restaurantIDs.length)];
    const r = http.get(`${BASE}/api/v1/restaurants/${id}`);
    check(r, { 'restaurant 2xx': (resp) => resp.status === 200 });
    return r;
}

export function createOrder(restaurantIDs) {
    const id = restaurantIDs[Math.floor(Math.random() * restaurantIDs.length)];
    const card = http.get(`${BASE}/api/v1/restaurants/${id}`);
    if (card.status !== 200) return null;
    const menu = card.json('menu') || [];
    if (menu.length === 0) return null;
    const item = menu[Math.floor(Math.random() * menu.length)];

    const idemKey = uuidv4();
    const body = JSON.stringify({
        restaurant_id: id,
        items: [{ menu_item_id: item.menu_item_id, quantity: 1 + Math.floor(Math.random() * 2) }],
        delivery_address: {
            city: 'Москва',
            street_line: 'Тверская, 1',
            lat: MOSCOW_LAT,
            lon: MOSCOW_LON,
        },
    });
    const r = http.post(`${BASE}/api/v1/orders`, body, {
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': idemKey },
    });
    check(r, { 'create order 2xx': (resp) => resp.status === 201 || resp.status === 200 });
    return r;
}

export function initiatePayment(orderID) {
    const idemKey = uuidv4();
    const body = JSON.stringify({ payment_method: 'card', return_url: 'https://app.example.com/done' });
    const r = http.post(`${BASE}/api/v1/orders/${orderID}/payment`, body, {
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': idemKey },
    });
    check(r, { 'pay 2xx': (resp) => resp.status === 200 || resp.status === 202 });
    return r;
}

export function getOrder(orderID) {
    const r = http.get(`${BASE}/api/v1/orders/${orderID}`);
    check(r, { 'get order 2xx': (resp) => resp.status === 200 });
    return r;
}
