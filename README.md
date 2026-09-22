# llm-proxy — сервис защиты персональных данных (PII)

Рабочий прототип сервиса, который обнаруживает персональные данные в тексте,
заменяет их на обратимые токены (маскирование), а затем восстанавливает
оригиналы по токенам. Оркестрация, правила, валидаторы, токенизация, vault и
аудит-логирование выполняются на Go; Python-воркер отвечает только за NER-модели
и возвращает spans/labels/confidence/model source.

## Два контура

- **Продуктовый runtime** — `POST /v1/runtime/chat`: полный поток
  `запрос -> маскирование -> LLM -> демаскирование -> ответ`. Сейчас маршрут
  зарегистрирован, но бинарник честно возвращает `503`, пока не подключён
  реальный настраиваемый LLM-клиент (см. ниже).
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

Координатор runtime и автоматический E2E (mask -> LLM -> demask) существуют и
покрыты тестами, но запускаемый бинарник намеренно возвращает `503` до тех пор,
пока не подключён реальный настраиваемый LLM-клиент. Фейковый или вендорный
клиент не подразумевается и не изобретается.

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

## Consumer policy

Сейчас в `cmd/pii-service` захардкожен один benchmark/default consumer
(`benchmark`), которому разрешены все канонические типы ПДн; запрос без
доверенного identity резолвится именно в него. Транспортно-резолвимые
per-system политики реализованы в пакете `policy`, но пока не подключены к
конфигурации, поэтому настроить их через переменные окружения или файл нельзя.
Демаскирование и rules-only degraded mode по умолчанию выключены (fail closed).