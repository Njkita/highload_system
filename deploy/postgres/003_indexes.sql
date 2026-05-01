-- Iter-0 базовая (НЕ накатывается): без этих индексов Postgres делает Seq Scan
-- на restaurants (500 строк) и Seq Scan на menu_items (10 000 строк) для каждой
-- карточки. На HDD при росте таблиц это быстро становится bottleneck'ом.
--
-- Этот файл подключается профилем `iter1` через переменную окружения POSTGRES_APPLY_INDEXES=1
-- (см. docker-compose.yml). На iter-0 файл существует, но не накатывается, чтобы
-- baseline честно показывал «как плохо без индексов».

CREATE INDEX IF NOT EXISTS idx_restaurants_cuisine ON restaurants USING GIN (cuisine);
CREATE INDEX IF NOT EXISTS idx_restaurants_open    ON restaurants (is_open) WHERE is_open = TRUE;

-- основной паттерн каталога — «открой ресторан, покажи доступные позиции»
CREATE INDEX IF NOT EXISTS idx_menu_items_rest_available
    ON menu_items (restaurant_id) WHERE is_available = TRUE;

-- частный кабинет пользователя
CREATE INDEX IF NOT EXISTS idx_orders_user_created ON orders (user_id, created_at DESC);

-- горячий селектор для outbox relay: только «ещё не опубликованные»
CREATE INDEX IF NOT EXISTS idx_outbox_unpublished
    ON outbox_events (id) WHERE published_at IS NULL;

-- селектор для reconciliation: малый, только processing/pending
CREATE INDEX IF NOT EXISTS idx_payments_pending
    ON payments (created_at) WHERE status IN ('processing','pending_confirmation');

ANALYZE;
