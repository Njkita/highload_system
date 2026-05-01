-- ДЗ 3 (PoC) — упрощённая схема под happy path заказа и оплаты.
-- Партиционирования и Patroni из ADR-002/003 здесь нет: на VM 2 vCPU/8 GB смысла никакого,
-- это всё равно не воспроизводимо в одном инстансе. Инварианты, которые важны для PoC,
-- держим constraint-уровнем: уникальность psp_payment_id и idempotency_key.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE restaurants (
    id            UUID         PRIMARY KEY,
    name          VARCHAR(255) NOT NULL,
    cuisine       TEXT[]       NOT NULL,
    rating        NUMERIC(2,1) NOT NULL CHECK (rating BETWEEN 0 AND 5),
    eta_minutes   SMALLINT     NOT NULL,
    delivery_fee  INTEGER      NOT NULL,
    min_order     INTEGER      NOT NULL,
    is_open       BOOLEAN      NOT NULL DEFAULT TRUE,
    lat           DOUBLE PRECISION NOT NULL,
    lon           DOUBLE PRECISION NOT NULL,
    preview_url   TEXT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE menu_items (
    id            UUID         PRIMARY KEY,
    restaurant_id UUID         NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE,
    name          VARCHAR(255) NOT NULL,
    description   TEXT         NOT NULL DEFAULT '',
    price         INTEGER      NOT NULL CHECK (price >= 0),
    category      VARCHAR(100) NOT NULL,
    is_available  BOOLEAN      NOT NULL DEFAULT TRUE
);

CREATE TABLE orders (
    id               UUID PRIMARY KEY,
    user_id          UUID NOT NULL,
    restaurant_id    UUID NOT NULL,
    status           VARCHAR(32) NOT NULL,
    items_snapshot   JSONB NOT NULL,
    total_amount     INTEGER NOT NULL,
    delivery_fee     INTEGER NOT NULL,
    delivery_address JSONB NOT NULL,
    idempotency_key  UUID NOT NULL UNIQUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE payments (
    id              UUID PRIMARY KEY,
    order_id        UUID NOT NULL REFERENCES orders(id),
    status          VARCHAR(32) NOT NULL,
    amount          INTEGER NOT NULL,
    psp_payment_id  VARCHAR(255),
    idempotency_key UUID NOT NULL UNIQUE,
    failure_reason  TEXT,
    attempt_no      SMALLINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at    TIMESTAMPTZ
);

-- инвариант ADR-003: один psp_payment_id может быть привязан только к одному платежу,
-- через webhook не дадим создать дубль
CREATE UNIQUE INDEX uq_payments_psp_id ON payments(psp_payment_id) WHERE psp_payment_id IS NOT NULL;

CREATE TABLE outbox_events (
    id             BIGSERIAL PRIMARY KEY,
    aggregate_type VARCHAR(32) NOT NULL,
    aggregate_id   UUID        NOT NULL,
    event_type     VARCHAR(64) NOT NULL,
    payload        JSONB       NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ
);

-- идемпотентность webhook'ов от PSP
CREATE TABLE processed_webhooks (
    psp_payment_id VARCHAR(255) PRIMARY KEY,
    event_type     VARCHAR(64)  NOT NULL,
    processed_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
