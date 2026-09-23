## Purpose

Определяет явно включаемый диагностический журнал с фактическими телами запросов и ответов публичного HTTP API для локального разбора полного обмена.

## ADDED Requirements

### Requirement: Уровень логирования задаётся environment
Система SHALL читать `PII_LOG_LEVEL`, принимать только `info` и `debug` без учёта регистра и использовать `info` по умолчанию. При любом другом значении система MUST завершать запуск с ошибкой, которая не содержит значения других environment variables.

#### Scenario: Уровень по умолчанию
- **WHEN** `PII_LOG_LEVEL` отсутствует
- **THEN** система запускается с уровнем `info`

#### Scenario: Debug включён
- **WHEN** `PII_LOG_LEVEL=debug`
- **THEN** система запускается с включённым debug-журналом HTTP

#### Scenario: Неизвестный уровень отклонён
- **WHEN** `PII_LOG_LEVEL` содержит значение, отличное от `info` и `debug`
- **THEN** система не запускает HTTP listeners и сообщает об ошибке конфигурации `PII_LOG_LEVEL`

### Requirement: Debug-журнал хранится в фиксированном защищённом файле
При уровне `debug` система SHALL дописывать события в `/var/log/pii-service/debug.jsonl`, SHALL создавать файл с правами `0600` и MUST завершать запуск до открытия listeners, если файл нельзя открыть для добавления или его права нельзя ограничить до `0600`. При уровне `info` система MUST NOT создавать или открывать этот файл.

#### Scenario: Debug-файл открыт
- **WHEN** уровень равен `debug`, каталог существует и доступен для записи
- **THEN** система создаёт либо открывает `/var/log/pii-service/debug.jsonl` для добавления и ограничивает права файла до `0600`

#### Scenario: Debug-файл недоступен
- **WHEN** уровень равен `debug`, но файл нельзя безопасно открыть или ограничить его права
- **THEN** система завершает запуск до открытия API и metrics listeners

#### Scenario: Info не касается debug-файла
- **WHEN** уровень равен `info`
- **THEN** наличие, отсутствие или недоступность `/var/log/pii-service/debug.jsonl` не влияет на запуск и файл не изменяется

### Requirement: Debug-журнал содержит тела публичного API
При уровне `debug` система SHALL после каждого допущенного запроса к `/process`, `/v1/pii/detect`, `/v1/pii/tokenize`, `/v1/pii/detokenize`, `DELETE /v1/pii/scopes/{scope_id}` и `/v1/runtime/chat` записывать ровно одно валидное JSONL-событие. Событие SHALL содержать timestamp, HTTP method, фиксированный route template, status code, фактически прочитанные handler-ом байты request body и фактически отправленные байты response body. Тела SHALL сохраняться как JSON strings без маскирования PII, исходного текста, восстановленного текста или токенов. Параллельные события MUST NOT смешиваться.

#### Scenario: Успешный запрос записан полностью
- **WHEN** при уровне `debug` обработан успешный запрос к публичному data route и handler прочитал его тело полностью
- **THEN** одна строка debug-файла содержит исходное request body и полное response body этого запроса

#### Scenario: Ошибочный ответ записан
- **WHEN** при уровне `debug` публичный data route возвращает `4xx` или `5xx`
- **THEN** одна строка debug-файла содержит прочитанную часть request body, фактическое error response body и status code

#### Scenario: Удаление scope не раскрывает path parameter
- **WHEN** при уровне `debug` обработан `DELETE /v1/pii/scopes/customer-secret`
- **THEN** событие содержит route `/v1/pii/scopes/{scope_id}` и не содержит `customer-secret` как значение пути

#### Scenario: Info не пишет тела
- **WHEN** при уровне `info` обработан любой публичный data route
- **THEN** request body и response body не записываются в debug-файл

### Requirement: Область debug-журнала ограничена публичными телами
Система MUST NOT записывать в debug-журнал HTTP headers, cookies, query parameters или фактический URL path. Система MUST NOT записывать health и metrics запросы либо тела HTTP-обмена с model worker и downstream LLM. Существующий audit-журнал SHALL сохранять прежний формат и назначение независимо от `PII_LOG_LEVEL`.

#### Scenario: Authorization исключён
- **WHEN** debug-запрос содержит заголовок `Authorization`
- **THEN** значение заголовка отсутствует в debug-событии

#### Scenario: Служебные endpoints исключены
- **WHEN** вызывается health endpoint или внутренний metrics listener
- **THEN** debug-событие для вызова не создаётся

#### Scenario: Внутренний HTTP-обмен исключён
- **WHEN** обработка публичного запроса вызывает model worker или downstream LLM
- **THEN** их HTTP request body и response body не создают отдельных debug-событий

#### Scenario: Audit-формат не меняется
- **WHEN** включён уровень `debug` и обработан data route
- **THEN** существующее audit-событие содержит тот же allowlisted набор полей, что и при уровне `info`

### Requirement: Ошибка записи не меняет HTTP-контракт
Если запись debug-события завершается ошибкой после успешного запуска, система MUST сохранить фактический HTTP status и body ответа клиенту, MUST NOT повторять data-операцию и SHALL вывести в stderr только фиксированное сообщение без содержимого тел.

#### Scenario: Ошибка файла после обработки
- **WHEN** data route сформировал ответ, но debug-файл перестал принимать запись
- **THEN** клиент получает исходный status и body ровно один раз, а stderr не содержит request body или response body
