# HookRelay

[![CI](https://github.com/Vladimir-29/HookRelay/actions/workflows/ci.yml/badge.svg)](https://github.com/Vladimir-29/HookRelay/actions/workflows/ci.yml)

**HookRelay** — асинхронный сервис доставки webhook-событий, разработанный на Go и PostgreSQL.

Сервис принимает события через HTTP API, находит подписанные endpoints и передаёт доставку фоновым Worker'ам. Поддерживает автоматические повторы, историю попыток и HMAC-подписи запросов.

## Возможности

- **Асинхронная доставка** — API принимает событие, Worker обрабатывает доставки независимо.
- **Параллельная обработка** — PostgreSQL `SKIP LOCKED` позволяет нескольким Worker'ам безопасно забирать задачи.
- **Retry-механизм** — exponential backoff с jitter и ограничением числа попыток.
- **Управление доставками** — статусы, история попыток и ручной повтор.
- **Безопасность** — HMAC-SHA256 подписи, AES-256-GCM шифрование секретов и SSRF-защита исходящих запросов.
- **Надёжность** — lease-механизм для восстановления незавершённых задач и graceful shutdown.

## Архитектура

```text
                 HTTP API
                    |
                    v
              PostgreSQL
             (events / queue)
                    |
                    v
                  Worker
                    |
                    v
             HTTP Endpoint
```

API и Worker — два режима одного Go-приложения. PostgreSQL используется как основное хранилище и очередь доставок.

**Технологии:** Go · PostgreSQL · Docker Compose · GitHub Actions

## Быстрый запуск

Требуется Docker с поддержкой Docker Compose.

```bash
git clone https://github.com/Vladimir-29/HookRelay.git
cd HookRelay

cp .env.example .env
```

Укажи в `.env` значение `POSTGRES_PASSWORD` и сгенерируй ключ шифрования:

```bash
openssl rand -base64 32
```

Скопируй полученный ключ в `HOOKRELAY_ENCRYPTION_KEY`.

Запусти сервис:

```bash
docker compose up --build -d
```

Проверь API:

```bash
curl http://localhost:8080/health
```

Ожидаемый ответ:

```json
{"status":"ok"}
```

Миграции применяются автоматически при запуске.

Логи:

```bash
docker compose logs -f api worker
```

Остановка:

```bash
docker compose down
```

## API

Основные маршруты:

| Метод | Маршрут | Назначение |
|---|---|---|
| `POST` | `/api/v1/endpoints` | Создать endpoint |
| `GET` | `/api/v1/endpoints` | Получить список endpoints |
| `POST` | `/api/v1/endpoints/{id}/rotate-secret` | Обновить секрет |
| `GET` | `/api/v1/endpoints/{id}/subscriptions` | Получить подписки |
| `PUT` | `/api/v1/endpoints/{id}/subscriptions` | Заменить подписки |
| `POST` | `/api/v1/events` | Создать событие |
| `POST` | `/api/v1/deliveries/{id}/retry` | Повторить dead-доставку |

Пример создания подписки:

```bash
curl -X PUT http://localhost:8080/api/v1/endpoints/1/subscriptions \
  -H "Content-Type: application/json" \
  -d '{"event_types":["order.created"]}'
```

Пример отправки события:

```bash
curl -X POST http://localhost:8080/api/v1/events \
  -H "Content-Type: application/json" \
  -d '{
    "event_type": "order.created",
    "payload": {"order_id": "123"}
  }'
```

## Подпись webhook

Каждый endpoint получает собственный секрет. Он возвращается только при создании или перевыпуске — его необходимо сохранить.

Исходящие запросы подписываются с помощью HMAC-SHA256:

```text
X-HookRelay-Signature: sha256=<hex>
```

Секреты хранятся в PostgreSQL в зашифрованном виде. Для их расшифровки используется `HOOKRELAY_ENCRYPTION_KEY`.

## Тестирование

Интеграционные тесты требуют отдельной PostgreSQL-базы, указанной в `TEST_DATABASE_URL`. Перед запуском тестов необходимо применить миграции к этой базе.

```bash
DATABASE_URL="$TEST_DATABASE_URL" go run ./cmd/migrate

go test ./...
go test -race ./...
```

GitHub Actions автоматически проверяет форматирование, `vet`, сборку, unit- и интеграционные тесты, race detector и Docker Compose smoke-тест.

## Ограничения

- Доставка работает по модели **at-least-once**: при сбоях возможны повторные запросы. Получатели должны учитывать дубликаты.
- Строгий порядок доставки не гарантируется.
- API управления пока не имеет встроенной аутентификации. Не рекомендуется открывать его напрямую в недоверенную сеть.
