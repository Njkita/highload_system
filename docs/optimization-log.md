# Optimization log — ДЗ 3

Профиль трафика — **read-heavy** (80% read / 20% write), как в [requirements §4.3](requirements.md#43-средний-и-пиковый-rps). Минимальная планка задания — read ≥ 100 RPS p99 < 500 ms, write ≥ 30 RPS p99 < 1 s, error rate < 1% при устойчивых 5 минутах. Цели из ДЗ 1 жёстче (search p99 < 300 ms, create_order p99 < 700 ms) — берём их.

Стенд: VM 2 vCPU / 8 GB / 72 GB HDD, Ubuntu 22.04, Docker 24.0.7. Load-тест запускался с ноутбука по локальной сети, RTT VM↔ноут ≈ 1–2 ms. Между итерациями делается `docker compose down -v` и `up -d --build` — Postgres init-скрипты применяются только при первом создании volume'а.

Замеры — медиана из трёх прогонов `loadtest/load.js` по 5 минут на устойчивой ступеньке. Cross-run variance ≈ ±10% по latency, ±5% по RPS — нормально для системы на HDD, где WAL fsync время плывёт.

## Сводная таблица

| Метрика | NFR (ДЗ 1) | iter-0 |
|---|---|---:|
| Read p99 (search + card) | < 500 ms | **470 ms** |
| Read max RPS | ≥ 100 | 180 |
| Write p99 (create + pay) | < 1000 ms | **720 ms** |
| Write max RPS | ≥ 30 | 45 |
| Error rate (5 min @ target) | < 1% | 0.4% |
| 2x spike error rate | < 5% | 12% |
| CPU (на пике) | 70–90% | 100% (postgres) |
| RAM (на пике) | — | 2.1 GB |
| Bottleneck | — | pgxpool + Seq Scan |
| NFR ДЗ 1 достигнут? | — | нет (search) |
| Минимальная планка? | — | да (read) / нет (spike) |

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
