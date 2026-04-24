# ADR-002. Стратегия хранения данных: polyglot с отложенной миграцией поиска

- Статус: Accepted
- Дата: 2026-04-24
- Ссылки: [architecture.md §5](../architecture.md#5-выбор-бд-и-модель-данных), [ADR-001](001-architecture-style.md), [ADR-003](003-payment-reliability.md)

## Контекст

Нужно выбрать стек хранения под четыре сильно разных паттерна доступа.

`orders` и `payments` — write-критичные: пик 48 RPS (24 + 24), но каждая запись обязана быть `RPO = 0` ([requirements §3.4](../requirements.md#нфт-004-durability--сохранность-данных)). Требуются ACID-транзакции сразу по нескольким таблицам (`orders + outbox` одним `COMMIT`, см. [ADR-003](003-payment-reliability.md)) и строгие UNIQUE по `idempotency_key` и `psp_payment_id`.

`restaurants` и `menu_items` — read-heavy до 433 RPS ([requirements §4.3.3](../requirements.md#433-пиковый-rps-по-операциям)), редкие UPDATE. Нужен полнотекстовый поиск, фильтрация по массиву кухонь, гео-радиус, сортировка по рейтингу или ETA.

Корзина, idempotency-ключи, hot-state заказа, геопозиция курьера — эфемерные данные с TTL и sub-ms требованиями. Персистентность не нужна.

События заказа и платежа — append-only поток с fan-out (Notification, Tracking, аналитика) и replay при инцидентах.

Медиа блюд — ~1.8 ТБ через год, ~6 ТБ через три ([requirements §4.5.3](../requirements.md#453-фото-блюд)). Раздача через CDN.

Плюс ограничения: бюджет и команда 3–5 инженеров ([requirements §5.2](../requirements.md#52-ограничения)) — лишние движки = лишняя операционная цена; PoC-этап должен запускаться на VM 2 vCPU / 8 ГБ ([Assignment 03](../../README.md)); PostgreSQL и Redis команда держит уверенно, с Elasticsearch знаком один человек.

## Рассмотренные альтернативы

### A. Один PostgreSQL на всё

Orders, payments, catalog, menu, cart, idempotency, event-log — в одной БД.

Плюс — единая технология и кросс-таблица транзакции между любыми сущностями. Но корзина и idempotency-ключи на PostgreSQL — это write-amplification на эфемерных данных с TTL 24 ч: VACUUM и распухание WAL. Sub-ms read-p99 на tracking при 971 RPS PostgreSQL не даёт без кэша перед собой. Event-log как таблица не даёт fan-out consumer groups с независимыми offset'ами и replay'ем — это пришлось бы писать вручную поверх LISTEN/NOTIFY. Полнотекстовый поиск на `pg_trgm + tsvector` перестаёт держать p99 < 300 мс при росте каталога за ~200 000 ресторанов — нужно иметь exit-план. Как целевое решение не подходит, но как Day 1 для части доменов (каталог без отдельного search-движка) — годится.

### B. Полный polyglot из пяти движков сразу

PostgreSQL (orders/payments) + MongoDB (catalog) + Elasticsearch (search) + Redis (ephemeral) + Kafka (events).

MongoDB не даёт преимуществ над PostgreSQL для нашего каталога: модель реляционная (ресторан → позиции), структура фиксирована, нужны транзакции при обновлении меню; JSONB в PostgreSQL делает то же с ACID. Пять движков — пять наборов бэкапов, мониторинга, практик апгрейда и баг-профилей; для команды 3–5 человек это явный перебор. Elasticsearch на Day 1 при 60 000 ресторанов — полный ELK-кластер под задачу, с которой сегодня справляется `pg_trgm + PostGIS`. Отклонено.

### C. Polyglot с отложенной миграцией поиска — выбрана

Day 1 — три движка плюс managed S3:

- PostgreSQL 16 в одном кластере, две логические БД: `orders_db` (orders, payments, outbox, tracking_events) и `catalog_db` (restaurants, menu_items). `pg_trgm + PostGIS` покрывают поиск и геофильтр.
- Redis 7 Cluster — корзина, idempotency, cache, `order_hot`, `courier_pos` GEO, token-bucket.
- Redpanda (Kafka-wire-compatible) — шина событий, outbox relay, DLQ.
- S3 + CDN — медиа.

Day 2 запускается по метрике, не по календарю: OpenSearch 2.x с CDC из PostgreSQL через Debezium. Триггер миграции — `p99 catalog search > 300 мс при > 500 RPS в течение 24 часов` или рост каталога за 200 000 ресторанов.

Redpanda выбрана вместо ванильной Kafka: один бинарник, без ZooKeeper и KRaft-узлов, ниже footprint — критично на PoC-этапе с VM 8 ГБ; wire-compatible, клиенты идентичны, миграция на Kafka — смена endpoint'а.

### D. Cassandra / ScyllaDB как primary store

Линейная масштабируемость записи до миллионов ops/s.

У нас write всего 48 RPS — под линейную запись в wide-column нет повода. Cassandra теряет ACID на multi-partition транзакциях, что ломает инвариант `orders + outbox` одним `COMMIT`. Вторичные индексы и OLTP-запросы за пределами partition-key в Cassandra слабые — приходится дублировать данные в дополнительные таблицы. Экспертиза в команде отсутствует. Отклонено.

### E. Managed SaaS (RDS + ElastiCache + MSK)

Managed-варианты тех же движков у облачного провайдера.

Снимают операционные задачи, но дороже в 2–3 раза на старте и завязывают на провайдера. На PoC-VM не запустить — нарушает требование локального развёртывания Assignment 03. Отклонено на старте, возможный будущий шаг при росте.

## Решение

Принимаем вариант C. Point-in-time стек: PostgreSQL 16 (Patroni + synchronous replica, PgBouncer, PostGIS 3.4, pg_trgm, `pg_uuidv7`) + Redis 7 Cluster (3 master + 3 replica, AOF `everysec`) + Redpanda 24.x + Yandex Object Storage + CDN.

Для `orders_db` — `synchronous_commit=remote_apply` (RPO = 0 для платежей); для `catalog_db` — `local` (потеря пары секунд каталога при failover допустима и экономит write-latency).

Партиционирование `orders` и `payments` — `BY RANGE (created_at)` по месяцам.

Миграция поиска на OpenSearch — отдельный проект, запускается по упомянутым метрическим триггерам, не раньше.

## Последствия

Стартуем на посильном наборе: три движка плюс облачный S3, операционный overhead минимален. Redis снимает с PostgreSQL то, что он плохо делает (sub-ms ephemeral + GEO). Exit-план на рост каталога задокументирован, а не «разберёмся когда упрёмся».

Общий PostgreSQL-инстанс — единая точка отказа; смягчается Patroni и synchronous-репликой, а разделение по логическим схемам позволяет без переработки кода разнести `orders_db` и `catalog_db` на разные физические инстансы при необходимости.

Day-2 миграция на OpenSearch — отдельный проект с CDC-пайплайном, периодом двойной записи и сверкой консистентности между PG и OS; ADR отдельно не выделен до активации.

Redpanda вместо Kafka означает чуть меньше готовых туториалов и stackoverflow-ответов; смягчается wire-compatibility. Managed SaaS не исключён в будущем — решение принимается на основе операционной нагрузки к концу года 1.

Нужен runbook «CDC PG → OpenSearch» до активации триггера и отдельный план бэкапов (WAL-G для PG, snapshot AOF для Redis, S3 lifecycle для медиа).
