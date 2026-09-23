# llm-proxy — сервис защиты персональных данных (PII)

Рабочий прототип сервиса, который обнаруживает персональные данные в тексте,
заменяет их на обратимые токены (маскирование), а затем восстанавливает
оригиналы по токенам. Оркестрация, правила, валидаторы, токенизация, vault и
аудит-логирование выполняются на Go; Python-воркер отвечает только за NER-модели
и возвращает spans/labels/confidence/model source.

## Два контура

- **Продуктовый runtime** — `POST /v1/runtime/chat`: полный поток
  `запрос -> маскирование -> LLM -> демаскирование -> ответ`. Работает, когда
  задана полная группа LLM-конфигурации (`PII_LLM_URL` и `PII_LLM_MODEL`).
  Если группа отсутствует, маршрут честно возвращает `503`, чтобы
  `POST /process` мог работать отдельно.
- **Benchmark-адаптер** — `POST /process`: тонкий контур mask/restore по
  протоколу checker. Принимает `payload` и `payload_id`, возвращает ровно
  `result`. LLM не вызывает.

## Предварительные требования

- Go 1.23+ (для локального запуска).
- Python 3.11+ и зависимости из `python/model_worker/requirements.txt`
  (для локального запуска воркера).
- Docker с плагином Compose (для Docker-запуска).
- `jq` — только для демо round trip ниже.

## Docker-запуск (полный режим)

Ключ vault должен декодироваться ровно в 32 байта. Сгенерируйте его и поднимите
стек (Go-сервис + Python-воркер):

```sh
export PII_VAULT_KEY="$(openssl rand -base64 32)"
docker compose up -d --build
```

При первом запуске воркер скачивает две закреплённые Hugging Face-модели
(`redmadrobot-rnd/rubert-base-pii-ner` и `vladlinv/ru-pii-ner-gliner2.5`) в
том `hf-cache`; это может занять несколько минут и требует доступа в интернет.
Кэш сохраняется в Docker volume, повторные запуски его переиспользуют.
Подробности — в [docs/docker-startup.md](docs/docker-startup.md).

## Docker-запуск (fast mode)

Fast mode использует только Go-правила/валидаторы и не обращается к воркеру.
Запустите только Go-сервис с `--no-deps`, чтобы воркер не стартовал и не
скачивал модели:

```sh
PII_VAULT_KEY="$(openssl rand -base64 32)" \
PII_MODEL_MODE=fast docker compose up -d --build --no-deps pii-service
```

## Локальный запуск

Полный режим — сначала воркер, затем Go-сервис:

```sh
# терминал 1: Python-воркер
python python/model_worker/model_worker.py --host 127.0.0.1 --port 8000

# терминал 2: Go-сервис (полный режим)
export PII_VAULT_KEY="$(openssl rand -base64 32)"
go run ./cmd/pii-service
```

Fast mode — только Go-сервис, воркер не нужен:

```sh
export PII_VAULT_KEY="$(openssl rand -base64 32)"
PII_MODEL_MODE=fast go run ./cmd/pii-service
```

## Конфигурация

Все переменные читаются из окружения. Ключ `PII_VAULT_KEY` обязателен.

| Переменная | По умолчанию | Описание |
| --- | --- | --- |
| `PII_VAULT_KEY` | — (обязателен) | Ключ vault, base64 от ровно 32 байт |
| `PII_API_LISTEN_ADDRESS` | `127.0.0.1:8080` | Адрес HTTP-сервера |
| `PII_MODEL_WORKER_URL` | `http://127.0.0.1:8000` | Базовый URL Python-воркера |
| `PII_MODEL_MODE` | `full` | `full` или `fast` |
| `PII_MODEL_CLIENT_TIMEOUT` | `30s` | Таймаут вызова воркера |
| `PII_VAULT_TTL` | `15m` | TTL mappings в vault |
| `PII_LLM_URL` | — (опц.) | URL chat-completions-совместимого downstream LLM |
| `PII_LLM_MODEL` | — (опц.) | Идентификатор модели в исходящем JSON |
| `PII_LLM_API_KEY` | — (опц.) | Ключ для `Authorization: Bearer` (пустой — заголовок не шлётся) |
| `PII_LLM_TIMEOUT` | `60s` | Таймаут вызова downstream LLM |

LLM-конфигурация опциональна как полная группа: `PII_LLM_URL` и
`PII_LLM_MODEL` задаются вместе. Если обе отсутствуют, `POST /v1/runtime/chat`
возвращает `503`, а `POST /process` работает отдельно. Любая частичная
конфигурация — ошибка валидации при старте.

## Health и метрики

```sh
curl -s http://localhost:8080/health/live    # {"status":"ok"}
curl -s http://localhost:8080/health/ready   # {"status":"ready"}
curl -s http://localhost:8080/metrics        # latency, RPS, TPS
```

## Демо round trip через POST /process

Отправьте синтетический оригинал, сохраните возвращённую маску, затем отправьте
эту маску с тем же `payload_id` и убедитесь, что оригинал восстановлен. Никаких
реальных ПДн — только синтетика.

```sh
# 1. Маскирование оригинала
MASK=$(curl -s -X POST http://localhost:8080/process \
  -H 'Content-Type: application/json' \
  -d '{"payload":"Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com","payload_id":"demo-1"}' \
  | jq -r .result)
echo "mask: $MASK"

# 2. Восстановление по маске с тем же payload_id
curl -s -X POST http://localhost:8080/process \
  -H 'Content-Type: application/json' \
  -d "{\"payload\":\"$MASK\",\"payload_id\":\"demo-1\"}" \
  | jq -r .result
# ожидается исходный синтетический текст
```

## Load benchmark через `cmd/pii-load`

`cmd/pii-load` — небольшой стандартный Go-бенчмарк для `POST /process`. Он
прогревает ограниченный пул синтетических записей `payload_id/original/mask`,
затем в течение заданного времени гонит детерминированную смесь mask/restore
запросов с теми же id как open-loop целевую нагрузку (bounded concurrency и
явный per-request timeout; медленные ответы не превращают тест в closed-loop).
Целевая нагрузка выводится из RPS и duration как явное число planned-слотов,
каждый слот планируется по своей метке времени; если слот не может стартовать
из-за занятого concurrency-слота, он учитывается как `unschedulable`, а не
теряется. Печатает machine-readable JSON-отчёт: requested/achieved RPS,
duration, planned/scheduled/completed, errors, unschedulable, p50/p95/p99,
целевой latency и факт его достижения. Только синтетика, реальных ПДн нет.

Целевой latency ≤ 1s — это целевой ориентир из Appendix, а не жёсткое правило
официального checker-а. Команда возвращает ненулевой код при ошибках запроса,
контракта, round trip, невозможности распланировать/выполнить нагрузку
(`unschedulable > 0`) или отмене прогона.

Smoke-проверка (короткий прогон против локального fast-mode сервиса):

```sh
go run ./cmd/pii-load -base-url http://127.0.0.1:8080 \
  -rps 50 -duration 2s -pool 8 -concurrency 16 -timeout 2s
```

Канонический прогон 1000 RPS / 5 минут (против отдельно запущенного
fast-mode сервиса):

```sh
go run ./cmd/pii-load -base-url http://127.0.0.1:8080 \
  -rps 1000 -duration 5m -pool 1000 -concurrency 200 -timeout 10s
```

Флаги: `-base-url`, `-rps`, `-duration`, `-pool`, `-concurrency`, `-timeout`,
`-target-latency`.

Зафиксированный результат одного канонического прогона 1000 RPS / 5 минут
(локальный loopback, fast mode) с raw JSON-отчётом и расшифровкой latency —
в [docs/load-benchmark-verified-run.md](docs/load-benchmark-verified-run.md).

## Расширенный API /v1/pii/*

- `POST /v1/pii/detect` — обнаружение без значений: `text`,
  `include_non_personal`, `request_id` → `request_id`, `has_personal_data`,
  `detected_types`, `entities`.
- `POST /v1/pii/tokenize` — scoped-токенизация: `text`, `scope_id`,
  `ttl_seconds`, `request_id` → `tokenized_text`, `detected_types`, `entities`,
  `scope_id`.
- `POST /v1/pii/detokenize` — восстановление без NER: `text`, `scope_id`,
  `mode` (`strict`/`preserve`), `request_id` → `restored_text`,
  `resolved_token_count`, `unresolved_tokens`.
- `DELETE /v1/pii/scopes/{scope_id}` — отзыв всех mappings scope →
  `{"status":"revoked"}`.

## Статус POST /v1/runtime/chat

Координатор runtime, реальный LLM-клиент и автоматический E2E (mask -> LLM ->
demask) существуют и покрыты тестами. Запускаемый бинарник выполняет полный
поток, когда задана полная группа LLM-конфигурации (`PII_LLM_URL` и
`PII_LLM_MODEL`). Если группа отсутствует, маршрут возвращает `503`, чтобы
`POST /process` мог работать отдельно.

## Демо round trip через POST /v1/runtime/chat

Задайте полную LLM-группу и отправьте синтетический запрос. Downstream LLM
получает только непрозрачные токены (например `<EMAIL_...>`), а ответ
восстанавливает оригиналы. Никаких реальных ПДн — только синтетика.

```sh
# 1. Запустите сервис с полной LLM-группой (пример для локального запуска)
export PII_VAULT_KEY="$(openssl rand -base64 32)"
export PII_LLM_URL="https://your-llm.example.com/v1/chat/completions"
export PII_LLM_MODEL="your-model"
go run ./cmd/pii-service

# 2. Отправьте синтетический запрос
curl -s -X POST http://localhost:8080/v1/runtime/chat \
  -H 'Content-Type: application/json' \
  -d '{"text":"Клиент Иванов Иван, телефон +7 900 123-45-67, email ivanov@example.com","scope_id":"demo-runtime"}' \
  | jq -r .result
# ожидается ответ LLM, в котором оригинальные значения восстановлены
```

## Проверка

```sh
# Compose валиден (сначала задайте любой 32-байтовый base64 ключ)
export PII_VAULT_KEY="$(openssl rand -base64 32)"
docker compose config

# Go: сборка и тесты
go build ./...
go test ./...

# Python-воркер: тесты
python -m unittest discover -s python/model_worker -p 'test_*.py'

# Безопасность: Gitleaks и Semgrep CE
make gitleaks
make semgrep
```

## Доступ к API

Все HTTP-маршруты сервиса доступны без входящей авторизации и используют
единый полный набор канонических типов ПДн. `PII_LLM_API_KEY` применяется
только для исходящих запросов к настроенной внешней LLM.
