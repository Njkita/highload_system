# highload_system

Проект по курсу MIPT Highload Systems 2026 — сервис доставки еды.

Грузин Никита, Б13-303.

## Структура

```
docs/
├── requirements.md            # ДЗ 1 — требования и capacity
├── architecture.md            # ДЗ 2 — архитектура, C4, API, модель данных
├── diagrams/                  # C4 L1/L2 + 3 sequence (.mmd + .svg + .png)
└── adr/                       # Architecture Decision Records
    ├── 001-architecture-style.md
    ├── 002-data-storage.md
    └── 003-payment-reliability.md

openapi/
└── v1.yaml                    # OpenAPI 3.1, 5 эндпоинтов
```

## Навигация по ДЗ 2

- Архитектурный стиль — [architecture.md §1](docs/architecture.md#1-архитектурный-стиль) + [ADR-001](docs/adr/001-architecture-style.md).
- C4 L1 / L2 — [architecture.md §2](docs/architecture.md#2-компоненты); исходники в [docs/diagrams/](docs/diagrams/).
- Sequence (happy / error / async) — [architecture.md §3](docs/architecture.md#3-sequence-diagrams).
- API — [architecture.md §4](docs/architecture.md#4-api), контракт в [openapi/v1.yaml](openapi/v1.yaml).
- БД и модель данных — [architecture.md §5](docs/architecture.md#5-выбор-бд-и-модель-данных) + [ADR-002](docs/adr/002-data-storage.md).

## Рендер диаграмм

Исходники — Mermaid (`.mmd`), рядом уже лежат `.svg` и `.png`. Пересобрать:

```bash
npm i -g @mermaid-js/mermaid-cli
cd docs/diagrams
for f in *.mmd; do mmdc -i "$f" -o "${f%.mmd}.svg" -b white; done
```

Inline Mermaid в `architecture.md` GitHub рендерит нативно при просмотре.
