## 1. Конфигурация уровня

- [x] 1.1 Добавить в `internal/config` закрытое перечисление `PII_LOG_LEVEL=info|debug`, default `info` и startup validation неизвестных значений; покрыть default, регистронезависимый override и безопасную ошибку unit-тестами, затем проверить `go test ./internal/config`.

## 2. Debug HTTP logger

- [x] 2.1 Реализовать отдельный пакет debug HTTP logger: конкурентно-безопасный JSONL writer, захват фактически прочитанного request body и записанного response body, фиксированный allowlist route templates, исключение headers/URL и безопасная обработка write error; unit-тестами проверить success, malformed/error response, параллельные записи, отсутствие `Authorization` и фактического `scope_id`, исключение health/unknown routes и сохранение optional `http.ResponseWriter` behavior, затем выполнить focused tests пакета.
- [x] 2.2 Реализовать безопасное открытие `/var/log/pii-service/debug.jsonl` только в режиме `debug`: append, обычный файл без symlink, mode `0600`, fail-fast и закрытие descriptor; unit-тестами проверить create, append, исправление mode, symlink/permission failure и отсутствие файловых операций в `info`, затем выполнить focused tests пакета.

## 3. Service wiring

- [x] 3.1 Подключить debug middleware в `cmd/pii-service` после admission и до router, не меняя audit/metrics и не оборачивая metrics listener или HTTP clients; integration-тестами проверить raw request/response bodies публичных data routes, `4xx`/`5xx`, отсутствие событий для health/metrics/model worker/LLM, неизменный клиентский ответ при ошибке записи и fail-fast до listeners, затем выполнить `go test ./cmd/pii-service ./internal/api/...`.

## 4. Эксплуатация и согласование

- [x] 4.1 Обновить Docker image/Compose, README и runbook: создать защищённый каталог, описать `PII_LOG_LEVEL`, фиксированный путь, volume, наличие PII/токенов, внешнюю ротацию и процедуру удаления; согласовать конфликтующие audit-сценарии активного `add-reversible-russian-pii-protection` с новым debug-исключением, затем проверить `docker compose config` и поиск устаревшего безусловного запрета body logging.

## 5. Регрессия

- [x] 5.1 Выполнить `gofmt`, `go test ./...`, `go vet ./...` и `openspec validate add-debug-http-file-logging --strict`; synthetic smoke в `info` MUST не создавать debug-файл, а в `debug` MUST создать mode `0600` и записать request/response bodies без headers.
