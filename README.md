# HookRelay

**HookRelay** — сервис асинхронной доставки вебхуков с использованием Go и PostgreSQL.

Проект предназначен для надёжной передачи событий внешним системам с поддержкой конкурентной обработки задач, восстановления после сбоев и защиты HTTP-доставки.

## Возможности

- REST API для управления endpoints и подписками на события.
- Приём событий и автоматическое создание задач доставки.
- Хранение событий и очереди доставок в PostgreSQL.
- Конкурентный захват задач с использованием `FOR UPDATE SKIP LOCKED`.
- Lease-механизм для восстановления незавершённых задач после сбоя Worker.
- Асинхронная HTTP-доставка вебхуков.
- Параллельная обработка задач несколькими Worker.
- SSRF-защита, включая проверку DNS-ответов и защиту от DNS rebinding.
- Обработка HTTP-ошибок и повторная постановка временно неудачных доставок в очередь.
- Exponential backoff с jitter и ограничением числа автоматических повторов.
- История попыток доставки в PostgreSQL.
- Ручной повтор доставки (`POST /api/v1/deliveries/{id}/retry`) для задач в статусе `dead`: доставка возвращается в очередь с новым циклом автоматических повторов.
- HMAC-SHA256 подпись каждого вебхука секретом endpoint (`X-HookRelay-Signature: sha256=<hex>`).
- Разделение API и Worker на отдельные режимы запуска.

## Технологии

- Go
- PostgreSQL
- pgx
- net/http
- SQL migrations
- Go testing

## Архитектура

HookRelay использует модульный монолит с двумя режимами запуска:

- **API** — принимает события, управляет endpoints и подписками.
- **Worker** — получает задачи из PostgreSQL, отправляет HTTP-запросы и сохраняет результаты доставки.

PostgreSQL выполняет роль как основного хранилища, так и очереди задач.

Для конкурентного захвата используется `FOR UPDATE SKIP LOCKED`. Lease-механизм позволяет повторно обрабатывать задачи, оставшиеся незавершёнными после сбоя Worker.

Доставка использует семантику **at-least-once**: при сбоях возможна повторная отправка одного события, поэтому получателям следует поддерживать идемпотентность.

## Запуск

### Docker Compose (рекомендуется)

```bash
git clone <repo-url> hookrelay && cd hookrelay
cp .env.example .env
# Сгенерируйте мастер-ключ для шифрования секретов:
echo "HOOKRELAY_ENCRYPTION_KEY=$(openssl rand -base64 32)" >> .env
# Задайте POSTGRES_PASSWORD в .env

docker compose up --build -d
```

Compose поднимает 4 сервиса: `postgres` (с healthcheck и volume `pgdata`), `migrate` (одноразовый — применяет все SQL-миграции и завершается), `api` (порт 8080) и `worker`. API и Worker стартуют только после успешных миграций.

Проверка API:

```bash
curl localhost:8080/health    # {"status":"ok"}
```

Создание endpoint — ответ содержит `secret` (base64url) **один раз**, сохраните его:

```bash
curl -X POST localhost:8080/api/v1/endpoints \
  -H 'Content-Type: application/json' \
  -d '{"name":"my-hook","url":"https://example.com/webhook"}'
# → {"id":1,"name":"my-hook","url":"...","secret":"...","created_at":"..."}
```

Подписка на событие:

```bash
curl -X PUT localhost:8080/api/v1/endpoints/1/subscriptions \
  -H 'Content-Type: application/json' \
  -d '{"event_types":["order.created"]}'
```

Тестовое событие → Worker доставит вебхук с подписью `X-HookRelay-Signature`:

```bash
curl -X POST localhost:8080/api/v1/events \
  -H 'Content-Type: application/json' \
  -d '{"event_type":"order.created","payload":{"order_id":42}}'
```

Логи и управление:

```bash
docker compose logs -f api worker migrate
docker compose ps
docker compose down          # остановить, данные PostgreSQL сохранятся в volume pgdata
docker compose down -v       # остановить и УДАЛИТЬ данные PostgreSQL
```

### Локальный запуск (без Docker)

API и Worker запускаются из одной кодовой базы в разных режимах через переменную `APP_MODE`.

```bash
APP_MODE=api
```

```bash
APP_MODE=worker
```

Потребуются `DATABASE_URL` и `HOOKRELAY_ENCRYPTION_KEY` (см. `.env.example`). Миграции применяются командой:

```bash
go run ./cmd/migrate   # DATABASE_URL=... go run ./cmd/migrate
```


Дополнительные параметры Worker:

- `WORKER_BATCH_SIZE` — размер пакета захватываемых задач (по умолчанию 10).
- `WORKER_POLL_INTERVAL` — интервал опроса очереди (по умолчанию 1s).
- `WORKER_LEASE_DURATION` — длительность lease задачи (по умолчанию 30s; должна превышать `DELIVERY_HTTP_TIMEOUT` с запасом).
- `DELIVERY_HTTP_TIMEOUT` — таймаут HTTP-запроса доставки (по умолчанию 10s).
- `WORKER_RETRY_DELAY` — базовая задержка перед первым повтором (по умолчанию 5s).
- `WORKER_MAX_RETRY_DELAY` — максимальная задержка backoff (по умолчанию 5m).
- `WORKER_MAX_RETRIES` — число автоматических повторов после первой попытки (по умолчанию 5; итого до 6 HTTP-попыток).

### Секреты и подпись вебхуков

Каждый endpoint получает собственный секрет (32 байта, base64url) при создании. Секрет используется для HMAC-SHA256 подписи тела запроса — получатель может проверить подлинность и целостность доставки.

Секрет возвращается **только один раз** — в ответе на `POST /api/v1/endpoints`. Обычные GET-ответы его не содержат: сохраните его сразу. В PostgreSQL секрет хранится зашифрованным AES-256-GCM.

Мастер-ключ задаётся переменной `HOOKRELAY_ENCRYPTION_KEY` — base64-кодированные 32 байта. Сгенерировать можно так:

```bash
openssl rand -base64 32
```

Перевыпуск секрета (старый перестаёт действовать, новый возвращается один раз):

```bash
curl -X POST http://localhost:8080/api/v1/endpoints/1/rotate-secret
```

Проверка подписи на стороне получателя (пример на Python):

```python
import hmac, hashlib
expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
assert hmac.compare_digest(received_signature, expected)
```

Если у endpoint нет секрета (создан до введения секретов) или он не расшифровывается, доставка не отправляется и считается окончательной ошибкой — перевыпустите секрет через `rotate-secret`.

Повторные попытки используют exponential backoff: задержка удваивается после каждой неудачной попытки, ограничена `WORKER_MAX_RETRY_DELAY` и содержит jitter ±20%. После исчерпания повторов доставка переходит в статус `dead`.

## Статус проекта

Проект находится в активной разработке.

Реализованы API, хранение событий и доставок, конкурентная очередь PostgreSQL, Worker, безопасная HTTP-отправка, ручной retry доставок, HMAC-SHA256 подписи вебхуков и Docker-окружение с автоматическими миграциями.

В планах:
- Финальные end-to-end проверки.