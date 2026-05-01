# highload_system

Проект по курсу MIPT Highload Systems 2026 — сервис доставки еды.

Грузин Никита, Б13-303.

## Структура

```
docs/
├── requirements.md            # ДЗ 1 — требования и capacity
├── architecture.md            # ДЗ 2 — архитектура, C4, API, модель данных
├── diagrams/                  # C4 L1/L2 + 3 sequence (.mmd + .svg + .png)
├── adr/                       # ДЗ 2 — Architecture Decision Records
│   ├── 001-architecture-style.md
│   ├── 002-data-storage.md
│   └── 003-payment-reliability.md
├── poc.md                     # ДЗ 3 — описание PoC, паттерны, что в скоупе
└── optimization-log.md        # ДЗ 3 — iter-0..iter-3, USE+RED метрики

openapi/
└── v1.yaml                    # OpenAPI 3.1

deploy/postgres/               # init-скрипты Postgres (схема + seed + индексы)
services/                      # ДЗ 3 — Go-сервисы PoC
├── catalog/  order/  payment/  psp-mock/
nginx/                         # ДЗ 3 — конфиги gateway (default + scaled)
loadtest/                      # ДЗ 3 — k6-сценарии (smoke / load / stress / spike)
docker-compose.yml             # ДЗ 3 — основной стек
docker-compose.scaled.yml      # ДЗ 3 — overlay для iter-3 (горизонтальное масштабирование)
```

## Навигация по ДЗ 2

- Архитектурный стиль — [architecture.md §1](docs/architecture.md#1-архитектурный-стиль) + [ADR-001](docs/adr/001-architecture-style.md).
- C4 L1 / L2 — [architecture.md §2](docs/architecture.md#2-компоненты); исходники в [docs/diagrams/](docs/diagrams/).
- Sequence (happy / error / async) — [architecture.md §3](docs/architecture.md#3-sequence-diagrams).
- API — [architecture.md §4](docs/architecture.md#4-api), контракт в [openapi/v1.yaml](openapi/v1.yaml).
- БД и модель данных — [architecture.md §5](docs/architecture.md#5-выбор-бд-и-модель-данных) + [ADR-002](docs/adr/002-data-storage.md).

# ДЗ 3 — PoC

PoC покрывает критичный путь из ДЗ 2: каталог → создание заказа → инициация оплаты, плюс инвариант ADR-003 (transactional outbox + идемпотентный webhook). Полный архитектурный стек из шести сервисов на VM 2 vCPU / 8 GB не уместился бы — поэтому из шести в коде реализованы три домена: `catalog`, `order`, `payment`. Tracking и Notification отрезаны: для проверки гипотез по нагрузке они не критичны, а съедают CPU и RAM, нужные основным сервисам. PSP заменён на отдельный сервис `psp-mock` с настраиваемой latency и error rate — это даёт возможность проверить circuit breaker не на «теоретически OPEN», а на реальной деградации внешнего вызова.

Замеры делались на bare-metal сервере 64 vCPU / 377 GB / SSD, но resource limits в `docker-compose.yml` стянуты под VM 2 vCPU / ~3.5 GB — это даёт честные числа «что выдержит сервис под VM-ограничениями», а не «что вытянет 64 ядра». Подробно о методике — в [docs/optimization-log.md](docs/optimization-log.md).

Профиль трафика — **read-heavy**: 80% read (search + restaurant card + get order) / 20% write (create order + initiate payment). Соотношение взято из [requirements §4.3](docs/requirements.md#43-средний-и-пиковый-rps): на сессию приходится ~9 read-операций и ~0.3 write.

## Как запустить

Из корня репо:

```bash
docker compose up -d --build
```

Дождаться, пока gateway станет healthy:

```bash
docker compose ps
docker compose logs -f gateway
```

Готовность одной командой:

```bash
curl -f http://127.0.0.1:8080/health    # ожидаем 200 ok
```

Если поднимаете на удалённой VM — в `curl` подставьте её IP, в k6-командах ниже — `BASE_URL=http://<vm-ip>:8080`.

Полная остановка:

```bash
docker compose down          # удалит контейнеры, том БД останется
docker compose down -v       # с уничтожением тома (если правили seed)
```

## Как проверить

```bash
# 1) поиск ресторанов в радиусе 5 км от центра Москвы
curl -sS "http://127.0.0.1:8080/api/v1/restaurants?lat=55.75&lon=37.62&radius=5000&per_page=5" | jq

# 2) карточка одного ресторана + его меню
RID=$(curl -sS "http://127.0.0.1:8080/api/v1/restaurants?lat=55.75&lon=37.62&radius=5000&per_page=1" | jq -r '.items[0].restaurant_id')
curl -sS "http://127.0.0.1:8080/api/v1/restaurants/${RID}" | jq '.menu | length'

# 3) создание заказа (Idempotency-Key обязателен — повтор тем же ключом отдаст тот же order_id)
MENU_ID=$(curl -sS "http://127.0.0.1:8080/api/v1/restaurants/${RID}" | jq -r '.menu[0].menu_item_id')
ORDER=$(curl -sS -X POST http://127.0.0.1:8080/api/v1/orders \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d "{\"restaurant_id\":\"${RID}\",\"items\":[{\"menu_item_id\":\"${MENU_ID}\",\"quantity\":2}],\"delivery_address\":{\"city\":\"Москва\",\"street_line\":\"Тверская, 1\",\"lat\":55.75,\"lon\":37.62}}")
echo "$ORDER" | jq
OID=$(echo "$ORDER" | jq -r '.order_id')

# 4) состояние заказа
curl -sS "http://127.0.0.1:8080/api/v1/orders/${OID}" | jq

# 5) инициация оплаты (PSP-mock симулирует ответ за ~120-200 мс)
curl -sS -X POST "http://127.0.0.1:8080/api/v1/orders/${OID}/payment" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"payment_method":"card","return_url":"https://app.example.com/done"}' | jq

# 6) после успеха PSP пришлёт webhook -> заказ переходит в paid
# имитируем это вручную (берём intent_id из ответа выше):
INTENT_ID=$(curl -sS -X POST "http://127.0.0.1:8080/api/v1/orders/${OID}/payment" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"payment_method":"card"}' | jq -r '.redirect_url' | sed 's|.*/||')
curl -sS -X POST "http://127.0.0.1:8080/webhooks/psp" \
  -H "Content-Type: application/json" \
  -d "{\"intent_id\":\"${INTENT_ID}\",\"status\":\"succeeded\"}"
curl -sS "http://127.0.0.1:8080/api/v1/orders/${OID}" | jq '.status'   # paid
```

## Как запустить load-тест

k6 запускается **с ноутбука или другой машины**, не с VM (load-генератор не должен бить за ресурсы с приложением).

```bash
# smoke — 1 VU, 30s — нужен до load.js, чтобы убедиться что всё поднялось
BASE_URL=http://<vm-ip>:8080 k6 run loadtest/smoke.js

# load — 5 минут устойчивой нагрузки на целевой RPS, для замера p99
BASE_URL=http://<vm-ip>:8080 k6 run loadtest/load.js

# stress — постепенный рост, чтобы найти точку отказа
BASE_URL=http://<vm-ip>:8080 k6 run loadtest/stress.js

# spike — 2x резкий пик и recovery
BASE_URL=http://<vm-ip>:8080 k6 run loadtest/spike.js
```

USE-метрики снимаются на VM параллельно k6:

```bash
ssh <vm> "docker stats --no-stream"
ssh <vm> "iostat -x 1 10"
ssh <vm> "vmstat 1 10"
ssh <vm> "docker exec highload_system-postgres-1 psql -U food fooddelivery -c 'SELECT * FROM pg_stat_activity'"
```

## Паттерны

Минимум по заданию — 2 design + 2 resilience. В реализации применены восемь.

| Паттерн | Тип | Зачем здесь | Где в коде |
|---|---|---|---|
| **API Gateway** | design | Единая точка входа, маршрутизация по префиксам, единый health | [nginx/nginx.conf](nginx/nginx.conf) |
| **Transactional Outbox** | design | Гарантия публикации события: orders/payments + outbox_events в одной транзакции, отдельный relay-воркер пушит в Redpanda | [services/order/cmd/main.go](services/order/cmd/main.go) `createOrder`, [services/payment/cmd/main.go](services/payment/cmd/main.go) `webhookPSP` + `runOutboxRelay` |
| **Idempotency-Key** | design | Повтор `POST /orders` или `/payment` с тем же ключом возвращает тот же ответ — защита от дублей при retries клиента | [services/order/cmd/main.go](services/order/cmd/main.go) `createOrder` (Redis-кэш + `UNIQUE` в БД); [services/payment/cmd/main.go](services/payment/cmd/main.go) `initiatePayment` |
| **Cache-Aside** | design | Restaurant card и search — read-heavy. Redis перед Postgres, TTL 60 с, ключ нормализован округлением координат до 100 м | [services/catalog/cmd/main.go](services/catalog/cmd/main.go) `searchRestaurants` + `getRestaurant` (toggle через env `CACHE_ENABLED`) |
| **Circuit Breaker** | resilience | Защита от деградации PSP: если 50%+ вызовов фейлится за 30 с — OPEN на 60 с, клиент получает `503 Retry-After` сразу, не ждёт таймаут | [services/payment/cmd/main.go](services/payment/cmd/main.go) `gobreaker.NewCircuitBreaker` |
| **Timeout** | resilience | Жёсткий cap на внешние вызовы: PSP — 2500 мс из latency-бюджета ADR-003, catalog → order — 500 мс | [services/payment/cmd/main.go](services/payment/cmd/main.go) `callPSP`, [services/order/cmd/main.go](services/order/cmd/main.go) `fetchRestaurant` |
| **Retry with backoff (reconciliation)** | resilience | Зависшие в `pending_confirmation` платежи воркер опрашивает каждые 15 с (тиковая backoff'ная схема в ADR-003) | [services/payment/cmd/main.go](services/payment/cmd/main.go) `runReconciliation` |
| **Rate Limiting** | resilience | Token-bucket в Redis на `POST /orders`: спайк клиента не должен снести БД | [services/order/cmd/main.go](services/order/cmd/main.go) `rateLimit` (Lua-скрипт INCR + PEXPIRE) |
| **Health Check** | resilience | `/healthz` (живой) + `/readyz` (готов: пингует БД), используется compose'ом и nginx upstream | каждый сервис в `cmd/main.go` |

## Итерации оптимизации

Полный лог с цифрами и анализом — [docs/optimization-log.md](docs/optimization-log.md). Каждая итерация — отдельный git tag:

```bash
git checkout iter-0   # baseline (без индексов, без кэша, pgxpool=4)
git checkout iter-1   # + индексы, тюнинг Postgres, pgxpool=20
git checkout iter-2   # + Redis cache-aside на каталоге
git checkout iter-3   # + горизонтальное масштабирование catalog/order ×2 за nginx
```

Сводка по реальным замерам (полный лог цифр и stats — в [docs/optimization-log.md](docs/optimization-log.md)):

**Load: 5 минут на target NFR (120 read / 30 write RPS)**

| Метрика | NFR | iter-0 | iter-1 | iter-2 | iter-3 |
|---|---|---:|---:|---:|---:|
| read p99 | < 500 ms | 2.33 ms | 1.65 ms | 1.29 ms | 1.31 ms |
| write p99 | < 1000 ms | 200.92 ms | 200.85 ms | 200.93 ms | 200.79 ms |
| error rate | < 1% | 0.00% | 0.00% | 0.00% | 0.00% |

Все четыре итерации проходят NFR. На target RPS система недогружена — разница между итерациями там минимальна. Эффект каждой итерации виден под stress.

**Stress: ramp до 800 read / 160 write RPS (10 минут)**

| Метрика | iter-0 | iter-3 |
|---|---:|---:|
| http_req_duration p99 | 1.18 s | 699 ms |
| error rate | 6.23% (✗) | 0.27% (✓) |
| sustained req/s | 421 | 536 |
| bottleneck | catalog CPU + pgxpool=4 (упор на 600 RPS read) | не упёрся (gateway 50% MiB, catalog 40% от cap) |

**Spike: 2× пик 200 read / 60 write на 30 секунд**

| Метрика | NFR | iter-3 |
|---|---|---:|
| error rate | < 5% | 0.00% |

## Ограничения PoC и что осталось вне скоупа

Подробно — в [docs/poc.md](docs/poc.md). Кратко: нет Tracking/Notification сервисов, нет PostGIS (расстояние считается в коде), нет партиционирования таблиц, нет Patroni — один Postgres-инстанс, RPO = 0 не воспроизводится. Это сознательные упрощения под VM 2 vCPU / 8 GB; в production ADR-002/003 остаются в силе.
