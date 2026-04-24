# ADR-003. Надёжность платежей: Idempotency-Key + transactional outbox + reconciliation

- Статус: Accepted
- Дата: 2026-04-24
- Ссылки: [architecture.md §3.1–3.2](../architecture.md#3-sequence-diagrams), [ADR-001](001-architecture-style.md), [ADR-002](002-data-storage.md)

## Контекст

«Оплата критична, подтверждённые платежи терять нельзя» — прямая цитата бизнес-вводных ([requirements §1.1](../requirements.md#11-исходные-вводные)). Формальные требования: `availability фиксация платежа = 99.99%` ([requirements §3.3](../requirements.md#нфт-003-availability)) — меньше 4.4 мин простоя в месяц; `RPO = 0, RTO ≤ 15 мин` для подтверждённых платежей ([§3.4](../requirements.md#нфт-004-durability--сохранность-данных)); `Idempotency-Key` живёт не меньше 24 ч ([§3.6](../requirements.md#нфт-006-consistency-для-критичного-контура-заказа-и-оплаты)); двойных успешных списаний — не больше 1 на 1 000 000; `∆ order ↔ payment ≤ 60 с в 99.9% случаев`; статус `paid` показывается клиенту только после подтверждения системой или PSP.

Природа внешнего PSP ([requirements §5.2](../requirements.md#52-ограничения), п. 5) — асинхронная: инициация синхронная, результат приходит webhook'ом. Webhook может прийти дважды (ретраи PSP при сетевых сбоях), прийти через минуты (3DS), либо не прийти вовсе (падение PSP после получения 200 OK от нас).

Это та самая «unknown» зона, в которой угадывание `success`/`fail` ведёт к нарушению требований: угадали `success` при реальном `fail` — заказ пошёл без оплаты, угадали `fail` при реальном `success` — двойное списание при повторной попытке клиента.

## Рассмотренные альтернативы

### A. Fire-and-forget + dual-write

Payment Service пишет в БД, параллельно публикует в Kafka и дёргает PSP. Ошибки логируются.

Между записью в БД и публикацией в Kafka возможен обрыв: в одном порядке теряются события, в обратном — консюмеры считают несуществующий платёж прошедшим. Повторный webhook делает второй UPDATE и второй publish — клиент получает дубль уведомлений и при неудачной комбинации двойную активацию заказа. Всё это ровно те failure-modes, от которых надо защищаться. Отклонено.

### B. Синхронный REST-вызов PSP с retry до успеха

Checkout ждёт от PSP финальный результат, ретраит при таймауте, отвечает клиенту только после этого.

Нереализуемо с реальными PSP: СБП, Юкасса, Тинькофф и остальные российские провайдеры работают как `initiate → redirect/3DS на стороне клиента → webhook`. Финал-статуса в один HTTP-round-trip у них попросту нет. Даже если бы был — latency-budget [§4.4](../requirements.md#44-latency-budget-для-критичного-сценария) отводит на PSP 1500 мс и на весь checkout 2500 мс; retry по таймауту выходит за бюджет кратно. Плюс если наш сервис упал в момент retry, а PSP транзакцию провёл — мы об этом не узнаем. Отклонено.

### C. 2PC (two-phase commit) между PostgreSQL и Kafka

XA-транзакции или Kafka 3.x transactional producer + PG prepare/commit для атомарной записи в БД и шину.

2PC в продуктиве с PostgreSQL и Kafka — известный источник боли: длинные open-transactions блокируют WAL, падение координатора оставляет in-doubt transactions, производительность деградирует. Плюс 2PC защищает только собственную шину и не решает идемпотентность входящих webhook'ов PSP. Редкая экспертиза, мало готового инструментария. Отклонено как несоразмерное задаче.

### D. Idempotency-Key + transactional outbox + idempotent webhook + reconciliation — выбрана

Четыре согласованных механизма.

**Idempotency-Key** обязателен на всех write-API (`POST /orders`, `POST /orders/{id}/payment`). Сервис пишет результат первой успешной попытки в Redis `idem:{key}` с TTL 24 ч; повтор с тем же ключом возвращает сохранённый ответ, логика не выполняется заново.

**Transactional outbox:** `orders`, `payments` и `outbox_events` лежат в одной PostgreSQL-БД; любое изменение состояния идёт одной транзакцией:

```sql
BEGIN;
UPDATE payments SET status = 'succeeded', confirmed_at = now() WHERE id = $1;
UPDATE orders   SET status = 'paid' WHERE id = $2;
INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
VALUES ('payment', $1, 'payment.succeeded', $3);
COMMIT;
```

Outbox relay (Debezium или polling с `FOR UPDATE SKIP LOCKED`) читает `outbox_events WHERE published_at IS NULL` и публикует в Kafka, после подтверждения проставляет `published_at`. Даунтайм relay не теряет события — они копятся и догоняются.

**Идемпотентный webhook** — `POST /api/v1/webhooks/psp` с HMAC-подписью:

```sql
BEGIN;
SELECT * FROM payments WHERE psp_payment_id = $1 FOR UPDATE;
-- если уже succeeded / failed — COMMIT и 200, без побочных эффектов
-- иначе UPDATE payments + UPDATE orders + INSERT outbox
COMMIT;
```

Гарантии держатся за счёт `UNIQUE (psp_payment_id)` на уровне БД (физически не даст вставить дубликат), `SELECT ... FOR UPDATE` сериализует параллельные webhook'и по одной записи, условный `UPDATE ... WHERE status = 'processing'` работает last line of defence и не даёт откатить уже финализированный статус.

**Reconciliation worker** — периодический процесс, читает `payments WHERE status IN ('processing','pending_confirmation')` возрастом больше 15 с, идёт в PSP по `GET /intents/{psp_id}` и дожимает финальный статус если webhook потерялся. Расписание — exponential backoff 15 с → 1 мин → 5 мин, TTL 30 мин. После 30 мин заказ отменяется и снимается hold в качестве компенсирующей транзакции. Последовательность — [§3.2 architecture.md](../architecture.md#32-ошибка-таймаут-psp-на-инициации-оплаты).

## Решение

Принимаем вариант D. Инварианты, которые держатся в коде и покрываются тестами:

- `orders_db.payments`: `UNIQUE (psp_payment_id, created_at)` и `UNIQUE (idempotency_key, created_at)` — constraints, не application-level проверки.
- Любое изменение `payments.status` всегда идёт одной транзакцией с `INSERT INTO outbox_events`.
- Webhook-обработчик безусловно начинает с `SELECT ... FOR UPDATE WHERE psp_payment_id = $1`.
- Все исходящие вызовы PSP обёрнуты в circuit breaker (`sony/gobreaker`, пороги: ошибок > 50% в окне 30 с при ≥ 20 запросах → OPEN на 60 с).
- Таймаут HTTP-клиента к PSP — ровно 2500 мс, из latency-budget [§4.4](../requirements.md#44-latency-budget-для-критичного-сценария).
- При таймауте — `UPDATE status = 'pending_confirmation'` и клиенту `202 Accepted`. Никогда 500 и никогда синхронный retry.
- Reconciliation worker — лидер-элекшн через PostgreSQL advisory lock (`pg_try_advisory_lock`), один активный worker в кластере.

## Последствия

Все требования НФТ-006 измеримо выполнимы: ключ идемпотентности живёт 24 ч, дубль-списания ≤ 1 на 1 000 000 за счёт `UNIQUE(psp_payment_id)`, `∆ order ↔ payment` идёт к ≤ 60 с за счёт короткого polling-интервала reconciliation. `RPO = 0` держится потому, что `payments.processing` создаётся до вызова PSP: даже полная потеря Payment Service после этой точки не теряет платёж — reconciliation его найдёт и дожмёт. Двойной webhook (типичный случай у российских PSP) не вызывает побочных эффектов. Получаем переиспользуемый примитив для любых будущих интеграций, где внешняя система может не дойти или прислать дубль — SMS-провайдеры, partner API ресторанов.

Из цены — два дополнительных фоновых процесса (outbox relay и reconciliation), каждый со своим health-check и алертом на lag (`max(now() - created_at) FROM outbox_events WHERE published_at IS NULL`). Outbox-таблица растёт линейно и требует регламента очистки — опубликованные строки живут 7 дней для аудита и уезжают в `outbox_archive`. Клиент больше не получает финальный статус в том же HTTP-запросе; нужно показывать промежуточное состояние `payment_pending_confirmation` и финал push-уведомлением — это продуктовая работа, но обязательная.

Нужен runbook «outbox lag > 60 с» как эксплуатационная задача. На PoC-этапе ([Assignment 03](../../README.md)) reconciliation может быть упрощён до `SELECT` + `GET /intents` без advisory lock — инстанс единственный; отдельный ADR появится при горизонтальном масштабировании worker'а.
