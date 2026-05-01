# ДЗ 3 — PoC

Short doc: что входит в скоуп PoC, что не входит и почему. Подробное обоснование архитектуры — [architecture.md](architecture.md), оно не повторяется здесь.

## Что в скоупе

Реализованы три сервиса из шести, описанных в ДЗ 2: `catalog`, `order`, `payment`. Они закрывают критичный путь — search → restaurant card → create order → initiate payment → webhook → paid. Этого достаточно, чтобы воспроизвести инвариант ADR-003 (transactional outbox + идемпотентный webhook + reconciliation) под нагрузкой.

API-операции, реализованные на нагрузку:

- `GET /api/v1/restaurants` — read-heavy, search в радиусе с фильтром по cuisine
- `GET /api/v1/restaurants/{id}` — read-heavy hot path, карточка ресторана с меню
- `GET /api/v1/orders/{id}` — read, состояние заказа из Redis hot-кэша или Postgres
- `POST /api/v1/orders` — write, идемпотентное создание + outbox-событие в одной транзакции
- `POST /api/v1/orders/{id}/payment` — write, инициация в PSP через circuit breaker
- `POST /webhooks/psp` — async, идемпотентный webhook, переводит заказ в `paid`

## Что не в скоупе

`Tracking` и `Notification` сервисы из ДЗ 2 не реализованы. Trekking — это WebSocket fan-out + GEOADD по курьерам, его нагрузка не вписана в PoC happy-path. Notification потребовал бы ещё один Go-консьюмер из Kafka и абстрактный SMS-шлюз. На VM 2 vCPU / 8 GB и тот, и другой съели бы CPU, нужный для honest замеров latency.

PostGIS не подключён: `ST_DWithin` заменён на app-level Haversine по 500 строкам — это микросекунды в Go и не искажает замеры. В ADR-002 PostGIS остаётся для production: на 60 000 ресторанов app-level фильтр уже даст p99 в сотнях миллисекунд.

Партиционирование `orders` и `payments` не сделано: за 5-минутный load-тест в одну партицию падает ~30 тысяч строк, оптимизация не даёт измеримого эффекта. Patroni и synchronous replica тоже выкинуты — один Postgres-инстанс. RPO = 0 на уровне ноды не воспроизводится, но инвариант «orders + outbox в одной транзакции» проверяется.

S3 + CDN не подняты — фото блюд в PoC отдаются как ссылки на `https://cdn.example.com/...`, фактически не читаются.

## PSP-mock

Внешний платёжный провайдер заменён на отдельный сервис `psp-mock` с настраиваемой задержкой и долей ошибок (см. env `PSP_LATENCY_MS`, `PSP_ERROR_RATE`, `PSP_TIMEOUT_RATE`). Это позволяет:

1. Получать реальные цифры latency на happy path (PSP-mock даёт 120-200 мс, как реальный российский эквайер).
2. Триггерить circuit breaker, повышая `PSP_ERROR_RATE` до 0.7 без правки кода — паттерн проверяется на работающей системе, а не «теоретически».
3. Триггерить fallback в `pending_confirmation`, повышая `PSP_TIMEOUT_RATE` — проверяется reconciliation-воркер.

## Resource budget на VM

| Контейнер | CPU | RAM |
|---|---:|---:|
| postgres | 0.6 | 1500 M |
| redpanda | 0.5 | 768 M |
| redis | 0.15 | 256 M |
| catalog (×1 или ×2) | 0.30 | 256 M |
| order (×1 или ×2) | 0.30 | 256 M |
| payment | 0.20 | 256 M |
| psp-mock | 0.10 | 64 M |
| nginx gateway | 0.15 | 96 M |

Суммарно на iter-0..2: ~2.2 vCPU / 3.4 GB. На iter-3 (×2 catalog + ×2 order, лимиты на каждый половинятся): ~2.5 vCPU / 3.8 GB. Остальное — ОС и HDD page-cache, который критичен для каталога.

## Запуск, проверка, нагрузка

См. [README.md](../README.md) — секции «Как запустить», «Как проверить», «Как запустить load-тест». Команды протестированы на чистой VM Ubuntu 22.04 + docker 24.x.

## Git tags

Каждая итерация — отдельный git tag, чтобы проверяющий мог `git checkout iter-N && docker compose down -v && docker compose up -d --build` и воспроизвести ровно то состояние:

```
iter-0   baseline: без индексов, без Redis-кэша, pgxpool=4, default Postgres
iter-1   + 003_indexes.sql, тюнинг Postgres, pgxpool=20
iter-2   + Redis cache-aside на каталоге (CACHE_ENABLED=true)
iter-3   + горизонтальное масштабирование catalog/order ×2 за nginx
```

`docker compose down -v` обязателен между итерациями — Postgres init-скрипты применяются только при первом создании volume'а, без `-v` индексы из iter-1 не накатятся.
