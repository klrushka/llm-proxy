## Why

Локальное и интеграционное использование расширенного API усложнено обязательной настройкой профиля доступа, списка consumers и Bearer API key. Сервис должен предоставлять все свои HTTP-маршруты без входящей авторизации, сохраняя защиту данных через токенизацию, vault и правила обработки.

## What Changes

- **BREAKING** Удалить входящую HTTP-авторизацию, access profiles и проверку registered consumers; функциональные маршруты больше не возвращают `401` или `403` из-за отсутствия или недействительности Bearer API key.
- **BREAKING** Удалить конфигурацию `PII_ACCESS_PROFILE` и `PII_CONSUMERS_JSON`, а также связанную consumer policy из конфигурации, запуска Docker и документации.
- Применять единый внутренний policy с полным набором канонических типов и разрешённой detokenization для всех запросов без передачи идентичности клиента.
- Сохранить `PII_LLM_API_KEY`: это исходящая авторизация к опциональному внешнему LLM endpoint, а не входящая авторизация сервиса.
- Обновить HTTP-, конфигурационные и integration-тесты для публичного API без Bearer header.

## Capabilities

### New Capabilities
- `public-pii-api`: Публичный HTTP API без входящей авторизации и consumer-specific policy.

### Modified Capabilities

- Нет.

## Impact

- Затронуты `internal/api/consumer_access.go`, `internal/config/access.go`, wiring в `cmd/pii-service`, Docker Compose, README, deployment runbook и тесты, покрывающие consumer access.
- Контракты `/v1/pii/*`, `/process`, `/metrics` и `/v1/runtime/chat` становятся доступными без `Authorization`.
- Исходящий заголовок `Authorization` для `PII_LLM_API_KEY` не изменяется.
