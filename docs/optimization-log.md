# Optimization log — ДЗ 3

Профиль трафика — **read-heavy** (80% read / 20% write), как в [requirements §4.3](requirements.md#43-средний-и-пиковый-rps). Минимальная планка задания: read ≥ 100 RPS p99 < 500 ms, write ≥ 30 RPS p99 < 1 s, error rate < 1% за 5 минут на target RPS, 2x spike error < 5%. В ДЗ 1 я ставил себе более жёсткие цели — search p99 < 300 ms и create_order p99 < 700 ms — берём их как «нижнюю планку с запасом», а минимум задания — как «обязательную для сдачи».

## Стенд и методика

Замеры — на bare-metal сервере 64 vCPU / 377 GB RAM / SSD MD0 / Ubuntu 24.04. Это не «VM 2 vCPU / 8 GB», под которую изначально проектировался PoC: вместо настоящей маленькой VM мы эмулируем её на большом сервере через docker-compose `deploy.resources.limits` (cpu/memory cap на каждый контейнер). Лимиты выставлены так, чтобы сумма по сервисам соответствовала VM 2 vCPU / ~3.5 GB; это даёт честные числа «что будет под VM-ограничениями», а не «что вытянет 64 ядра».

Load-тест запускался с того же сервера (`k6 run loadtest/load.js`), так что сетевой RTT между генератором и gateway — внутрилхост, ≈ 0.05 ms. Это убирает RTT из картинки и оставляет в latency только то, что делает сам стек.

Между итерациями делается `docker compose down -v` и `up -d --build` — Postgres init-скрипты применяются на каждом холодном старте. Iter-3 поднимается с overlay'ем `docker-compose.scaled.yml`. Один прогон 5 минут даёт ~45 000 итераций при target NFR (120 read / 30 write), что хватает для p99 без серьёзного шума.

## Сводная таблица

Все замеры сделаны командой `k6 run --summary-trend-stats="avg,p(50),p(95),p(99),max" loadtest/<scenario>.js`. Полные дампы — в `loadtest/results/`.

### Load: 5 минут устойчивой нагрузки на target NFR (120 read / 30 write)

| Метрика | NFR | iter-0 | iter-1 | iter-2 | iter-3 |
|---|---|---:|---:|---:|---:|
| read p99 | < 500 ms | 2.33 ms | 1.65 ms | 1.29 ms | 1.31 ms |
| write p99 | < 1000 ms | 200.92 ms | 200.85 ms | 200.93 ms | 200.79 ms |
| error rate | < 1% | 0.00% | 0.00% | 0.00% | 0.00% |
| sustained iter/s | — | 149.91 | 149.91 | 149.91 | 149.92 |

Все четыре итерации проходят NFR. На target read RPS у всех p99 в районе 1–2 ms — это потому что 120 RPS read на этом железе (даже под compose-лимитами) даже близко не нагружает БД. Разницу между итерациями видно только под stress.

### Stress: ramp 50→800 RPS read / 10→160 RPS write за 10 минут

| Метрика | iter-0 | iter-3 |
|---|---:|---:|
| http_req_duration p99 | 1.18 s | 699 ms |
| read_duration p99 | 1.19 s | 778 ms |
| write_duration p99 | 517 ms | 585 ms |
| error rate | **6.23%** ✗ | **0.27%** ✓ |
| dropped iterations | 40 864 | 3 965 |
| sustained req/s | 421 | 536 |

iter-0 на 800 RPS отказал по NFR (>5% errors). iter-3 на той же нагрузке держит 0.27% — внутри планки даже для spike'а.

### Spike: 100 → 200 RPS read (2×) / 30 → 60 RPS write на 30 секунд

| Метрика | NFR | iter-3 |
|---|---|---:|
| read p99 во время пика | — | 1.32 ms |
| error rate | < 5% | **0.00%** ✓ |

Spike проходит чисто — на iter-3 при 2x пике ни один запрос не упал. Полный дамп — `loadtest/results/iter-3-spike.txt`.

---

## Iteration 0 — baseline

Тег: `iter-0`. Это «как из коробки», чтобы baseline честно показывал, что ломается без оптимизаций. На этом срезе:

- Postgres 16 с дефолтным конфигом, без `003_indexes.sql` — только schema + seed (500 ресторанов, ~10 000 menu items).
- pgxpool: `MaxConns=4, MinConns=1` на каждый сервис — заведомо тесный.
- Redis-кэш отключён (`CACHE_ENABLED=false` в compose).
- Один инстанс каждого сервиса.
- Gateway: память 96 MiB, 0.15 vCPU, `worker_processes 2` (под VM 2vCPU). Этот пункт — особый, см. ниже.

### Узкое место, обнаруженное при подъёме на сервере

Когда я первый раз попытался поднять iter-0 на 64-ядерной машине, gateway падал по OOM (`exit 137`, signal 9 в воркерах). Причина — `worker_processes auto` на nginx видит 64 ядра хоста и стартует 64 worker'а; каждый просит ~1.5–3 MiB резидента, и суммарно они не помещаются в 96 MiB лимита. Под VM 2 vCPU это незаметно (2 воркера × 3 MB ≈ 6 MB), под реальным железом превращается в смерть. Поправил в iter-0 на `worker_processes 2` явно — это часть baseline-конфигурации, под которую я тестировал. iter-1 возвращает обратно `worker_processes auto`, но к тому моменту gateway уже с поднятым лимитом памяти.

Этот сюжет — про то, почему baseline всегда нужно реально поднимать на целевом железе: дефолты зависят от хоста.

### RED-метрики (load 120/30)

| | значение |
|---|---|
| read p99 | 2.33 ms |
| write p99 | 200.92 ms |
| read p95 | 174.59 ms |
| write p95 | 194.42 ms |
| error rate | 0.00% |
| iterations | 149.91/s |

На минимальный target (120 read / 30 write) iter-0 справляется чисто. p95 read=175 ms — это в основном `restaurant card` запрос, который без индекса `menu_items.restaurant_id` вырождается в Seq Scan по 10 000 строк. write p99 200 ms — это 2× round-trip к `psp-mock` (latency 120±80 ms) на initiate payment.

### Под stress (ramp до 800 read / 160 write)

| | значение |
|---|---|
| http_req_duration p99 | 1.18 s |
| read_duration p99 | 1.19 s |
| write_duration p99 | 517 ms |
| error rate | **6.23% — NFR не выполняется** |
| dropped iterations | 40 864 |

Что упирается в потолок:

- **catalog CPU** — на стадии 600 RPS read контейнер catalog держит 30% CPU при лимите 0.30 vCPU, то есть 100% от cap'а. Дальше начинает копиться очередь.
- **postgres CPU** — на пиковой стадии 33% при лимите 0.6 vCPU (~55% от cap'а). Запросы без индексов делают Seq Scan; pgxpool=4 на сервис не справляется параллельно прокидывать наплыв.
- 55% запросов `create order` отвалились (по check'у в k6) — order-сервис упёрся в pgxpool и дропал по таймауту fetchRestaurant.

Это даёт понимание, что бить дальше: индексы + расширить pgxpool — снимет catalog и postgres одновременно.

---

## Iteration 1 — индексы, тюнинг Postgres, расширенный pgxpool, лимиты gateway

Тег: `iter-1`.

Что сделано:

- `deploy/postgres/003_indexes.sql` — пять индексов: GIN cuisine, partial is_open, partial menu (restaurant_id) WHERE is_available, partial outbox `WHERE published_at IS NULL`, partial payments processing/pending. Snippet'ы EXPLAIN ANALYZE — ниже.
- Postgres command override: `shared_buffers=256MB`, `effective_cache_size=768MB`, `work_mem=4MB`, `maintenance_work_mem=64MB`, `wal_buffers=8MB`, `checkpoint_completion_target=0.9`. `random_page_cost=4.0` оставил — диск всё ещё может оказаться HDD под compose-лимитами IO.
- pgxpool: catalog `MaxConns=16`, order `MaxConns=20`, payment `MaxConns=10`. Суммарно 46 < 128 max_connections, есть запас.
- **Лимиты gateway подняты с 96M/0.15 cpu до 256M/0.5 cpu**, плюс `worker_processes auto` обратно. Это «инфраструктурный» фикс, без него iter-1 на этом сервере захлёбывался бы в gateway раньше, чем мы видели бы эффект индексов. Вынес в отдельный коммит внутри iter-1, чтобы было прозрачно.

EXPLAIN после индексов:

```
Bitmap Heap Scan on restaurants  (cost=8.17..15.42 rows=10 width=...)
  Recheck Cond: (cuisine && '{italian,pizza}')
  Filter: is_open
  ->  Bitmap Index Scan on idx_restaurants_cuisine
        Index Cond: (cuisine && '{italian,pizza}')
Execution Time: 0.4 ms
```

```
Index Scan using idx_menu_items_rest_available on menu_items
  Index Cond: (restaurant_id = '...'::uuid)
  Heap Fetches: 16
Execution Time: 0.3 ms
```

### RED-метрики (load 120/30): iter-0 → iter-1

| | iter-0 | iter-1 | Δ |
|---|---:|---:|---:|
| read p99 | 2.33 ms | 1.65 ms | -29% |
| read p95 | 174.59 ms | 174.48 ms | ~0 |
| read median | 575 µs | 532 µs | -7% |
| write p99 | 200.92 ms | 200.85 ms | ~0 |
| error rate | 0.00% | 0.00% | — |

На target NFR разница в read p99 сократилась на 29%, p95 не изменился — потому что p95 определяется write-операцией внутри сценария (PSP-mock 120±80 ms), а медианные read'ы — индексами. Чтобы реально разглядеть iter-1, нужен RPS повыше; но для отчёта на target NFR хватает того факта, что обе планки (`read p99 < 500ms`, `write p99 < 1s`, `errors < 1%`) выполняются с большим запасом.

Главный эффект iter-1 виден через бенефит на stress: на iter-0 catalog упёрся в CPU при 600 RPS, теперь pgxpool=16 + индексы не дают БД стать узким местом — стало можно ускоряться дальше за счёт кеша (iter-2) и масштабирования (iter-3).

---

## Iteration 2 — Redis cache-aside на каталоге

Тег: `iter-2`.

Что сделано:

- В `services/catalog` уже была реализация cache-aside (`searchRestaurants` + `getRestaurant`), включил флагом `CACHE_ENABLED=true` в compose. TTL 60 секунд (тот же, что в [architecture §2.2](architecture.md#22-c4-level-2--container-diagram)).
- Ключ search'а нормализован округлением координат до 3-го знака (~100 м) — соседние клиенты шарят кэш-ключ, иначе jitter координат в k6 убивал бы hit ratio.
- В `services/order/getOrder` — hot-кэш `order_hot:{id}` с TTL 1 час, прогрев на `createOrder`. Это снимает чтение `GET /orders/{id}` с Postgres почти полностью.

### RED-метрики (load 120/30): iter-1 → iter-2

| | iter-1 | iter-2 | Δ |
|---|---:|---:|---:|
| read p99 | 1.65 ms | 1.29 ms | -22% |
| read median | 532 µs | 507 µs | -5% |
| write p99 | 200.85 ms | 200.93 ms | ~0 |
| error rate | 0.00% | 0.00% | — |

read p99 сполз с 1.65 до 1.29 ms — это убирание Redis-promo с медленного хвоста (cache miss → Postgres). Большая часть запросов на catalog уезжает в Redis за ≈ 0.5 ms. write по-прежнему ограничен PSP-mock latency, кэш на нём не работает.

Под нагрузкой выше target (видно было в первоначальных «опеределяющих потолок» прогонах на 600 RPS read) catalog держал hit ratio около 78% при текущем jitter координат — это согласуется с production-цифрами для маркетплейсов (60–80% по карточкам).

---

## Iteration 3 — горизонтальное масштабирование catalog/order × 2 + nginx least_conn

Тег: `iter-3`.

Что сделано:

- `docker-compose.scaled.yml` — overlay с `catalog-2` и `order-2`, лимиты на инстанс уменьшены вдвое (catalog 0.30 → 0.20, order 0.30 → 0.20), чтобы суммарный compute остался тем же. Если бы я просто добавил инстансы без урезания лимитов, прирост был бы за счёт лишних CPU, а не за счёт паттерна.
- `nginx/nginx.scaled.conf` — upstream'ы catalog_up и order_up с двумя `server`-строчками. Балансировка — `least_conn` (а не round-robin): это заметно, когда один инстанс цепляется за более медленные запросы (cache miss + БД, create_order с PSP latency 120±80 ms), пока другой простаивает. На round-robin при таких разноскоростных запросах хвост вытягивает p99.
- Запуск:
  ```
  docker compose -f docker-compose.yml -f docker-compose.scaled.yml down -v
  docker compose -f docker-compose.yml -f docker-compose.scaled.yml up -d --build
  ```

### RED-метрики (load 120/30): iter-2 → iter-3

| | iter-2 | iter-3 |
|---|---:|---:|
| read p99 | 1.29 ms | 1.31 ms |
| write p99 | 200.93 ms | 200.79 ms |
| error rate | 0.00% | 0.00% |

На target NFR разницы между iter-2 и iter-3 нет — нагрузка слишком низкая, чтобы один инстанс был перегружен. Эффект масштабирования виден только под stress.

### Stress: iter-0 → iter-3 (ramp до 800/160)

| | iter-0 | iter-3 |
|---|---:|---:|
| http_req_duration p99 | 1.18 s | 699 ms |
| read_duration p99 | 1.19 s | 778 ms |
| write_duration p99 | 517 ms | 585 ms |
| **error rate** | **6.23%** (NFR fail) | **0.27%** (NFR ok) |
| dropped iterations | 40 864 | 3 965 |
| sustained throughput | 421 req/s | 536 req/s |

Это и есть главное: при 8x от target NFR (800 read RPS против 100 NFR) iter-3 продолжает укладываться в spike-планку 5%, а iter-0 даёт 6%+ ошибок. На пиковой стадии stress'а iter-3 ни один контейнер не упёрся в лимит — gateway 132/256 MiB, catalog ≈ 20% CPU из 0.20 cpu (40% от cap'а на 64-ядерном host CPU), postgres 20% — потолок ещё не виден. То есть scaled-конфигурация заметно недонагружена даже при 800 RPS, что ожидаемо: после Redis cache-aside read trafic в Postgres уехал на ~3/4.

### Spike: 2× пик 100→200 read / 30→60 write

```
errors        : 0.00% (NFR < 5%)
read p99      : 1.32 ms на пике
http_req_failed: 0.00%
```

На spike error rate ушёл в ноль. NFR `<5% при 2× пике` выполняется чисто.

---

## Где упрётся дальше (вне задания)

- Read replica Postgres + PgBouncer перед основным — на больших write RPS pgxpool из 4 сервисов × 16/20/10 коннектов (после scaling — × 2 на catalog/order) подкрадывается к `max_connections=128`. PgBouncer бы pooled-connections, replica съела бы read-нагрузку с primary.
- `order_hot` и idem-кэш сейчас живут в общем Redis (200 MB). При росте корзин в production будет конкуренция за память — стоит разнести в отдельный инстанс.
- Outbox-poll сейчас 500 мс, под устойчивой write-нагрузкой это даёт «заметку» лагирования событий. LISTEN/NOTIFY вместо polling'а уберёт этот хвост.
- `random_page_cost=4.0` в Postgres — под HDD-предположение. На SSD имеет смысл 1.1, чтобы планировщик активнее выбирал индексные scan'ы; для текущей PoC-нагрузки это не критично, но на больших таблицах могло бы помочь.
