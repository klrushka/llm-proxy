## 1. Конфигурация и access wiring

- [x] 1.1 Удалить access profile, consumers parser и их валидацию из `internal/config`; обновить config tests и проверить `go test ./internal/config`.
- [x] 1.2 Удалить consumer-access middleware, production resolver и wiring из запуска сервиса; сохранить статическую полную processing policy и проверить `go test ./cmd/pii-service ./internal/api`.

## 2. Публичная семантика API

- [x] 2.1 Удалить context-dependent consumer policy и namespace derivation из pipeline, передавая caller `scope_id` напрямую; обновить unit и integration tests, проверить `go test ./internal/api ./internal/ownership`.
- [x] 2.2 Заменить access-control тесты сценариями публичных `/v1/pii/*`, `/process`, `/metrics` и runtime routes без Bearer header, включая round trip одного scope; проверить `go test ./cmd/pii-service ./internal/api`.

## 3. Операционная конфигурация и проверка

- [x] 3.1 Удалить `PII_ACCESS_PROFILE` и `PII_CONSUMERS_JSON` из Docker Compose, README, runbook и smoke scripts; сохранить документацию `PII_LLM_API_KEY` как исходящего ключа и проверить `docker compose config` с заданным `PII_VAULT_KEY`.
- [x] 3.2 Выполнить регрессионную проверку `go test ./...` и Docker smoke: `POST /v1/pii/tokenize` без `Authorization` больше не возвращает `401` или `403`.
