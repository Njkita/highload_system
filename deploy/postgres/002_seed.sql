-- Seed: 500 ресторанов в районе Москвы, по ~20 позиций меню.
-- Хватает, чтобы поиск с фильтром был не «1 ряд в кэше», а возвращал десятки строк
-- и индексы реально что-то меняли. Координаты — равномерное распределение в радиусе ~15 км
-- от центра (lat 55.75, lon 37.62), чтобы radius-фильтр имел смысл.

INSERT INTO restaurants (id, name, cuisine, rating, eta_minutes, delivery_fee, min_order, is_open, lat, lon, preview_url)
SELECT
    gen_random_uuid(),
    'Restaurant #' || g,
    CASE g % 8
        WHEN 0 THEN ARRAY['italian','pizza']
        WHEN 1 THEN ARRAY['japanese','sushi']
        WHEN 2 THEN ARRAY['russian']
        WHEN 3 THEN ARRAY['american','burgers']
        WHEN 4 THEN ARRAY['georgian']
        WHEN 5 THEN ARRAY['chinese','asian']
        WHEN 6 THEN ARRAY['indian','curry']
        ELSE        ARRAY['french','bakery']
    END,
    ROUND((3.5 + (random() * 1.5))::numeric, 1),
    20 + (g % 40),
    99 + (g % 5) * 50,
    300 + (g % 4) * 100,
    TRUE,
    55.75 + (random() - 0.5) * 0.27,
    37.62 + (random() - 0.5) * 0.45,
    'https://cdn.example.com/r/' || g || '/cover.jpg'
FROM generate_series(1, 500) AS g;

-- Меню: 20 позиций на ресторан, 4 категории.
INSERT INTO menu_items (id, restaurant_id, name, description, price, category, is_available)
SELECT
    gen_random_uuid(),
    r.id,
    'Item ' || g || ' @ ' || r.name,
    'Описание блюда #' || g,
    150 + (g * 17 % 800),
    (ARRAY['starters','mains','desserts','drinks'])[1 + (g % 4)],
    (g % 25) <> 0
FROM restaurants r,
     generate_series(1, 20) AS g;

ANALYZE;
