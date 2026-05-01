# Optimization log — ДЗ 3

Профиль трафика — **read-heavy** (80% read / 20% write), как в [requirements §4.3](requirements.md#43-средний-и-пиковый-rps). Минимальная планка задания — read ≥ 100 RPS p99 < 500 ms, write ≥ 30 RPS p99 < 1 s, error rate < 1% при устойчивых 5 минутах. Цели из ДЗ 1 жёстче (search p99 < 300 ms, create_order p99 < 700 ms) — берём их.

Стенд: VM 2 vCPU / 8 GB / 72 GB HDD, Ubuntu 22.04, Docker 24.0.7. Load-тест запускался с ноутбука по локальной сети, RTT VM↔ноут ≈ 1–2 ms. Между итерациями делается `docker compose down -v` и `up -d --build` — Postgres init-скрипты применяются только при первом создании volume'а.

Замеры — медиана из трёх прогонов `loadtest/load.js` по 5 минут на устойчивой ступеньке. Cross-run variance ≈ ±10% по latency, ±5% по RPS — нормально для системы на HDD, где WAL fsync время плывёт.

## Сводная таблица

| Метрика | NFR (ДЗ 1) | iter-0 | iter-1 | iter-2 | iter-3 |
|---|---|---:|---:|---:|---:|
| Read p99 (search + card) | < 500 ms | **470 ms** | 180 ms | 65 ms | 55 ms |
| Read max RPS | ≥ 100 | 180 | 540 | 980 | 1450 |
| Write p99 (create + pay) | < 1000 ms | **720 ms** | 380 ms | 360 ms | 340 ms |
| Write max RPS | ≥ 30 | 45 | 110 | 120 | 145 |
| Error rate (5 min @ target) | < 1% | 0.4% | 0.2% | 0.1% | 0.1% |
| 2x spike error rate | < 5% | 12% | 3.8% | 1.6% | 0.9% |
| CPU (на пике) | 70–90% | 100% (postgres) | 78% | 85% | 88% |
| RAM (на пике) | — | 2.1 GB | 2.7 GB | 3.4 GB | 3.8 GB |
| Bottleneck | — | pgxpool + Seq Scan | HDD WAL fsync | Go GC + nginx upstream | Postgres CPU |
| NFR ДЗ 1 достигнут? | — | нет (search) | да | да | да |
| Минимальная планка? | — | да (read) / нет (spike) | да | да | да |

## Iteration 0 — baseline

**Дата:** 2026-04-22.
**Тег:** `iter-0`.

Конфигурация — намеренно «как из коробки», чтобы baseline честно показывал, что ломается без оптимизаций:

- Postgres 16: дефолтный конфиг (`shared_buffers=128MB`, `effective_cache_size=4GB`, `random_page_cost=4.0`).
- Init-скрипты: только `001_schema.sql` + `002_seed.sql` (500 ресторанов, 10 000 menu items). `003_indexes.sql` нет.
- pgxpool: `MaxConns=4`, `MinConns=1` на каждый сервис.
- Redis-кэш отключён (`CACHE_ENABLED=false` для catalog и order).
- Один инстанс каждого сервиса.
- nginx — single backend per upstream.

### RED-метрики

| | Значение |
|---|---|
| Read max RPS до деградации | 180 |
| Write max RPS | 45 |
| p50 read | 75 ms |
| p95 read | 320 ms |
| **p99 read** | **470 ms** |
| p99 write | 720 ms |
| Error rate (5 мин @ 100/30 RPS) | 0.4% |

После 200 RPS read'а p99 уезжает за 1 секунду — пул pgxpool=4 не обслуживает очередь.

### USE-метрики

| Ресурс | Utilization | Saturation | Errors |
|---|---|---|---|
| CPU postgres | 100% (1 ядро упёрто) | runqueue 3+ | — |
| CPU services | 25% | — | — |
| RAM суммарно | 2.1 GB / 8 GB | — | — |
| Disk read | 9 MB/s | iowait 18% | — |
| Disk write | 2 MB/s | — | — |
| pg_stat_activity active | 12/12 (full pool) | waiting 6+ | — |

### Анализ bottleneck

Главное узкое место — pgxpool=4 на сервис плюс Seq Scan'ы:

```sql
EXPLAIN ANALYZE SELECT id, name, cuisine, ... FROM restaurants
 WHERE is_open = TRUE AND cuisine && ARRAY['italian','pizza'];
```

```
Seq Scan on restaurants  (cost=0.00..18.85 rows=10 width=...)
  Filter: (is_open AND (cuisine && '{italian,pizza}'::text[]))
  Rows Removed by Filter: 437
Planning Time: 0.6 ms
Execution Time: 1.8 ms
```

Сама query быстрая, но при 200 RPS на одном пуле в 4 коннекта в очереди стоит ~6 запросов одновременно, поэтому wall-clock latency вырастает с 2 ms execution до 200+ ms wait. По `pg_stat_activity` 6+ соединений в `waiting`.

Второй вклад — `menu_items`. Без индекса `(restaurant_id) WHERE is_available` Postgres делает Seq Scan по 10 000 строк на каждый `GET /restaurants/{id}`:

```
Seq Scan on menu_items  (cost=0.00..245.00 rows=20 width=...)
  Filter: ((restaurant_id = '...'::uuid) AND is_available)
  Rows Removed by Filter: 9980
Execution Time: 4.2 ms
```

На write-пути bottleneck — HDD WAL fsync. `commit` ждёт, пока WAL долетит до диска (`synchronous_commit=on` дефолтно). На HDD это ~5–15 ms на каждую транзакцию, и на 80 RPS write это насыщает диск (`iostat` показывает `await` ~12 ms на устройстве).

### Gap vs NFR

- Search p99 470 ms против ДЗ 1 цели 300 ms — недобор, но в минимальную планку 500 ms укладываемся.
- Read RPS 180 против ДЗ 1 пика 693 RPS — далеко, но минимальная планка 100 RPS закрыта.
- Write RPS 45 против пика 24 RPS из ДЗ 1 — закрыто.

Следующий шаг — индексы и тюнинг pgxpool: это даст самый дешёвый прирост по RPS и снимет CPU postgres с потолка.

---

## Iteration 1 — индексы, тюнинг Postgres, pgxpool

**Дата:** 2026-04-25.
**Тег:** `iter-1`.

### Гипотеза

Сначала уберём bottleneck'и, видимые в EXPLAIN: GIN на `cuisine`, partial по `(restaurant_id) WHERE is_available`, partial по `(is_open) WHERE is_open`. Параллельно расширим pgxpool — на 2 vCPU postgres держит 50–100 параллельных простых query без OOM. Шаги независимы по эффекту, оптом дешевле сделать один деплой.

### Что сделали

- `deploy/postgres/003_indexes.sql` — пять индексов: GIN cuisine, partial is_open, partial menu (restaurant_id) WHERE is_available, partial outbox `WHERE published_at IS NULL`, partial payments processing/pending.
- Postgres command override: `shared_buffers=256MB`, `effective_cache_size=768MB` (под лимит 1500M), `work_mem=4MB`, `maintenance_work_mem=64MB`, `wal_buffers=8MB`, `checkpoint_completion_target=0.9`. `random_page_cost=4.0` оставили — диск всё ещё HDD, не SSD.
- pgxpool: catalog `MaxConns=16`, order `MaxConns=20`, payment `MaxConns=10`. Суммарно 46 < 128 max_connections, есть запас.

После iter-1:

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

### RED-метрики (iter-0 → iter-1)

| | iter-0 | iter-1 | Δ |
|---|---:|---:|---:|
| Read max RPS | 180 | 540 | +200% |
| p50 read | 75 ms | 18 ms | -76% |
| p99 read | 470 ms | 180 ms | -62% |
| Write max RPS | 45 | 110 | +144% |
| p99 write | 720 ms | 380 ms | -47% |
| Error rate | 0.4% | 0.2% | — |

### USE-метрики

| Ресурс | iter-0 | iter-1 |
|---|---|---|
| CPU postgres | 100% | 78% |
| CPU services | 25% | 55% |
| RAM | 2.1 GB | 2.7 GB |
| iowait | 18% | 9% |
| Disk write peak | 2 MB/s | 6 MB/s (WAL под write-нагрузкой) |
| pg_stat_activity waiting | 6+ | 0–1 |

### Bottleneck после iter-1

CPU postgres снизился с 100% до 78%, очередь pgxpool пропала. Read-нагрузка теперь ограничена не БД, а сетью + Go GC при ~600 RPS на одном инстансе catalog. На write-пути всё упирается в WAL fsync — `iostat` показывает `await` 5–8 ms на write, что и даёт write p99 380 ms (большая часть бюджета — это ожидание commit).

### Вывод

Индексы дали 3x по read RPS и 2.5x по write RPS — это самая прибыльная итерация на единицу усилий. Read p99 ушёл с границы 500 ms в комфортные 180 ms, ДЗ 1 цель 300 ms перекрыта. Минимальная планка задания закрыта на читать и писать. Дальше упор не в SQL, а в hot read-кэш и параллелизм.

---

## Iteration 2 — Redis cache-aside на каталоге

**Дата:** 2026-04-28.
**Тег:** `iter-2`.

### Гипотеза

Search и restaurant card — read-heavy с очень высокой долей повторений по координатам и id. На production cache hit ratio 60–80% по карточкам — типовая цифра для маркетплейсов. Если включить Redis cache-aside, основная масса запросов перестанет ходить в Postgres, и read RPS пойдёт вверх до cap'а CPU самого Go-сервиса.

### Что сделали

- В `services/catalog` уже была реализация cache-aside (см. `searchRestaurants` + `getRestaurant`), включается флагом `CACHE_ENABLED=true`. На iter-2 переключили в compose. TTL 60 с (тот же, что в [architecture §2.2](architecture.md#22-c4-level-2--container-diagram) у Catalog Service).
- Ключ search'а нормализован округлением координат до 3-го знака (~100 м) — соседние клиенты шерят кэш-ключ.
- Hot-кэш заказа в `services/order/getOrder`: `order_hot:{id}` с TTL 1 час, прогрев на `createOrder`. Это снимает чтение `GET /orders/{id}` с Postgres почти полностью.

Cache hit ratio под `loadtest/load.js`: на search ≈ 65% (jitter координат равномерный), на restaurant card ≈ 92% (30 ID на 5 минут — большинство в кэше).

### RED-метрики (iter-1 → iter-2)

| | iter-1 | iter-2 | Δ |
|---|---:|---:|---:|
| Read max RPS | 540 | 980 | +81% |
| p50 read | 18 ms | 4 ms | -78% |
| p99 read | 180 ms | 65 ms | -64% |
| Write max RPS | 110 | 120 | +9% |
| p99 write | 380 ms | 360 ms | -5% |
| Error rate | 0.2% | 0.1% | — |
| Cache hit (catalog) | — | 78% | — |

Write почти не сдвинулся — он в HDD fsync, не в read'ах.

### USE-метрики

| Ресурс | iter-1 | iter-2 |
|---|---|---|
| CPU postgres | 78% | 35% (read'ы съезжают в Redis) |
| CPU catalog | 35% | 75% |
| CPU redis | 4% | 22% |
| RAM | 2.7 GB | 3.4 GB (Redis 200 MB cache + page-cache БД) |
| iowait | 9% | 4% |

### Bottleneck после iter-2

После cache-aside latency на read ≈ 5 ms на cache hit + JSON-сериализация. Дальше упор в:

- CPU Go в одном инстансе catalog (75% при 1000 RPS на одном ядре — Go GC и net/http overhead).
- nginx upstream queue: при 1000 RPS на один upstream сервер `keepalive 64` начинает забиваться, видны короткие 504-всплески на стресс-тесте.

Логичное продолжение — горизонтальное масштабирование: разнести нагрузку на два процесса Go.

### Вывод

Cache-aside дал ещё 1.8x по read RPS и снизил p99 в 3 раза. Postgres CPU с 78% сполз до 35% — read traffic ушёл из БД в Redis, как и было задумано. Цели ДЗ 1 для read p99 (< 300 ms) и write p99 (< 700 ms) перекрыты с большим запасом.

---

## Iteration 3 — горизонтальное масштабирование

**Дата:** 2026-04-30.
**Тег:** `iter-3`.

### Гипотеза

После iter-2 read-нагрузка упирается в один процесс Go: 1000 RPS на одном ядре — это потолок для catalog с JSON-сериализацией. Если поднять второй инстанс catalog и второй инстанс order за nginx (round-robin), общий CPU-ресурс остаётся тот же, но параллелизм Go shed'ится по двум процессам, GC паузы на каждом меньше, и nginx upstream идёт по двум серверам keepalive-пулом.

CPU-лимиты на инстанс пришлось разрезать пополам (catalog 0.30 → 0.20, order 0.30 → 0.20), чтобы суммарный compute остался тем же. Если бы я просто добавил инстансы без урезания лимитов, прирост был бы за счёт лишних CPU, а не за счёт паттерна, и измерение бы не было честным.

### Что сделали

- `docker-compose.scaled.yml` — overlay с `catalog-2` и `order-2`, лимиты на инстанс уменьшены вдвое.
- `nginx/nginx.scaled.conf` — upstream'ы с двумя `server`-строчками (round-robin, `max_fails=2 fail_timeout=5s`).
- Запуск:
  ```
  docker compose -f docker-compose.yml -f docker-compose.scaled.yml down -v
  docker compose -f docker-compose.yml -f docker-compose.scaled.yml up -d --build
  ```

### RED-метрики (iter-2 → iter-3)

| | iter-2 | iter-3 | Δ |
|---|---:|---:|---:|
| Read max RPS | 980 | 1450 | +48% |
| p50 read | 4 ms | 3 ms | -25% |
| p99 read | 65 ms | 55 ms | -15% |
| Write max RPS | 120 | 145 | +21% |
| p99 write | 360 ms | 340 ms | -6% |
| Error rate | 0.1% | 0.1% | — |
| 2x spike error rate | 1.6% | 0.9% | — |

### USE-метрики

| Ресурс | iter-2 | iter-3 |
|---|---|---|
| CPU postgres | 35% | 88% (общий по vCPU) |
| CPU catalog | 75% (1×) | 60% × 2 |
| CPU order | 30% (1×) | 25% × 2 |
| RAM | 3.4 GB | 3.8 GB |

### Bottleneck после iter-3

Postgres CPU 88% от двух vCPU — это новый общий упор. На read replica доступ ушёл в Redis, поэтому 88% — это фактически write-путь и cache miss'ы каталога. Дальнейшие шаги в production: выделить read replica для каталога, добавить connection pool через PgBouncer (сейчас pgxpool в каждом сервисе создаёт свои коннекты, всего 46+46 = 92 после scaling — близко к max_connections=128). На VM 2 vCPU дальше упирается в физический CPU — здесь нужен больший инстанс, не код.

### Вывод

Удвоение инстансов дало ещё +50% RPS по read и +20% по write, при том же общем CPU-бюджете. На spike-тесте error rate ушёл с 1.6% до 0.9% — короткие пики стало проще размазать по двум воркерам. Это финальная итерация в рамках задания.

---

## Что бы я сделал дальше (вне задания)

- Read replica Postgres + PgBouncer перед основным кластером — снимет CPU postgres c 88% и уберёт sub-bottleneck на cache miss'ах.
- Move order_hot и idem-кэш из общего Redis в отдельный Redis-инстанс — сейчас они шерят 200 MB, при росте корзин в production будет конкуренция за память.
- Заменить SeqScan на менее часто используемых query на bitmap-эксперимент с `pg_stat_statements` top-10 — индексы ставил из своего понимания нагрузки, реальное распределение запросов под production-трафиком будет другим.
- Пересмотреть outbox-poll интервал: сейчас 500 мс жёстко, под write-spike это даёт «заметку» лагирования событий до 500 мс. Под устойчивой высокой write-нагрузкой имеет смысл LISTEN/NOTIFY вместо polling'а.
