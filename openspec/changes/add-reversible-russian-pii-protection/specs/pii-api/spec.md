## Purpose

Capability определяет HTTP API и audit logging контракт сервиса обнаружения, токенизации, восстановления и управления scope без раскрытия plaintext ПДн в логах или ответах по умолчанию. Включает расширенный API `/v1/pii/*`, тонкий benchmark adapter `POST /process`, record state machine, consumer policy, overload и метрики.

## ADDED Requirements

### Requirement: HTTP endpoints
Go-сервис SHALL предоставлять `POST /v1/pii/detect`, `POST /v1/pii/tokenize`, `POST /v1/pii/detokenize`, `DELETE /v1/pii/scopes/{scope_id}`, `GET /health/live`, `GET /health/ready`, `POST /process`, `POST /v1/runtime/chat` и `GET /metrics`.

#### Scenario: Health endpoints are available
- **WHEN** клиент вызывает live и ready endpoints
- **THEN** сервис возвращает состояние процесса и готовность зависимостей без раскрытия конфигурационных секретов

### Requirement: Runtime chat endpoint contract
`POST /v1/runtime/chat` SHALL принимать JSON с обязательными строковыми полями `text` и `scope_id`; успешный ответ `200` SHALL содержать строковое поле `result` с восстановленным ответом пользователя. Ошибки tokenization/vault/LLM/detokenization SHALL fail closed: ответ `500` с фиксированным безопасным телом без `result`, без исходного текста, без защищённого текста, без токенов, без mappings и без деталей upstream ошибки.

#### Scenario: Runtime chat returns restored result
- **WHEN** клиент отправляет `POST /v1/runtime/chat` с `text` и `scope_id`
- **THEN** сервис выполняет mask -> LLM -> demask и возвращает `200` со строковым `result`, содержащим восстановленные значения в модифицированном LLM тексте

#### Scenario: Runtime chat fails closed on any stage error
- **WHEN** ошибка возникает на этапе tokenization, vault, LLM или detokenization
- **THEN** сервис возвращает `500` с фиксированным безопасным телом без `result` и без plaintext fallback

### Requirement: Process benchmark adapter contract
`POST /process` SHALL принимать JSON с обязательными строковыми полями `payload` и `payload_id`; успешный ответ `200` SHALL содержать строковое поле `result`. Существующие `/v1/pii/*` endpoints сохраняются как отдельный расширенный API и не удаляются.

#### Scenario: Process accepts required string fields
- **WHEN** клиент отправляет `POST /process` с JSON, содержащим обязательные строковые `payload` и `payload_id`
- **THEN** сервис принимает запрос и возвращает успешный ответ `200`

#### Scenario: Process success response contains only result
- **WHEN** сервис успешно обрабатывает `POST /process`
- **THEN** ответ `200` содержит строковое поле `result` и не содержит дополнительных полей

### Requirement: Runtime flow orchestration
Основной runtime flow SHALL быть `запрос пользователя -> маскирование/токенизация ПДн -> вызов настраиваемой LLM -> демаскирование ответа LLM -> ответ пользователю`. Оркестрация SHALL выполняться на Go, LLM SHALL получать только защищённый текст, а ошибки tokenization/vault/LLM/detokenization SHALL fail closed без plaintext fallback. Benchmark adapter `POST /process` SHALL оставаться отдельным mask/restore контуром с текущим протоколом checker и MUST NOT вызывать LLM.

#### Scenario: LLM receives only protected text
- **WHEN** runtime flow обрабатывает запрос пользователя с ПДн
- **THEN** LLM получает только защищённый текст, а ответ пользователю формируется после демаскирования ответа LLM

#### Scenario: Runtime flow fails closed on any stage error
- **WHEN** ошибка возникает на этапе tokenization, vault, LLM или detokenization
- **THEN** runtime flow завершается ошибкой без plaintext fallback

#### Scenario: Process adapter does not call LLM
- **WHEN** клиент вызывает `POST /process`
- **THEN** adapter выполняет только mask/restore по протоколу checker и не вызывает LLM

### Requirement: Process record state machine
Для каждого `payload_id` система SHALL вести record state machine с минимальными состояниями `claim`/`ready`/`expired` (или эквивалентной record state machine). Для нового `payload_id` вход маскируется и correlation record атомарно сохраняется. Повтор исходного payload идемпотентно возвращает ту же ранее выданную маску. Передача ранее выданной маски с тем же `payload_id` восстанавливает оригинал без NER. Любой третий несвязанный payload для того же id возвращает безопасный `409` и не меняет запись. Restore является повторяемым чтением, а не необратимым переходом в `RESTORED`.

#### Scenario: New payload is masked and recorded
- **WHEN** клиент отправляет `POST /process` с новым `payload_id` и исходным текстом
- **THEN** сервис маскирует вход, атомарно сохраняет correlation record и возвращает `result` с маской

#### Scenario: Retry of original payload is idempotent
- **WHEN** клиент повторно отправляет тот же исходный payload с тем же `payload_id`
- **THEN** сервис возвращает ту же ранее выданную маску без повторной токенизации

#### Scenario: Passing previously issued mask restores original
- **WHEN** клиент отправляет `POST /process` с тем же `payload_id` и ранее выданной маской
- **THEN** сервис восстанавливает оригинал без повторного NER и возвращает его как `result`

#### Scenario: Third unrelated payload conflicts
- **WHEN** клиент отправляет третий несвязанный payload с тем же `payload_id`
- **THEN** сервис возвращает безопасный `409` и не меняет существующую запись

#### Scenario: Restore is repeatable read
- **WHEN** клиент повторно передаёт ранее выданную маску с тем же `payload_id`
- **THEN** сервис снова возвращает восстановленный оригинал, не переводя запись в необратимое состояние

### Requirement: Concurrent first request atomic claim
Concurrent первый запрос для одного `payload_id` SHALL использовать atomic claim/single writer: одинаковые запросы получают один стабильный result, конфликтующие не перезаписывают запись.

#### Scenario: Concurrent identical requests share one result
- **WHEN** несколько одинаковых первых запросов с одним `payload_id` приходят одновременно
- **THEN** все получают один стабильный `result`, запись не перезаписывается

#### Scenario: Concurrent different first payloads do not overwrite
- **WHEN** одновременно приходят разные первые payload с одним `payload_id`
- **THEN** один claim выигрывает, другой получает безопасный конфликт, и запись не перезаписывается

### Requirement: Consumer policy
Идентичность системы не добавляется в body `/process`. Система SHALL применять transport-resolved consumer policy с заранее настроенным benchmark/default consumer для проверяющего стенда; production identity может приходить из доверенного заголовка или сетевого контура. Нестандартный header не делается обязательным для benchmark.

#### Scenario: Benchmark consumer is used by default
- **WHEN** запрос `POST /process` приходит без доверенного заголовка identity
- **THEN** применяется заранее настроенный benchmark/default consumer policy

#### Scenario: Production identity resolves from trusted transport
- **WHEN** запрос приходит из доверенного заголовка или сетевого контура
- **THEN** применяется соответствующая consumer policy без поля identity в body

### Requirement: Overload and backpressure
Система SHALL ограничивать concurrency через bounded concurrency/backpressure и возвращать `429` с `Retry-After` при перегрузке.

#### Scenario: Overload returns 429 with Retry-After
- **WHEN** число одновременных запросов превышает bounded concurrency
- **THEN** сервис возвращает `429` с заголовком `Retry-After`

### Requirement: Fail closed on vault and model worker unavailability
При недоступности vault система SHALL всегда fail closed: возвращать `503` без `result`. Rules-only degraded mode допустим только если consumer policy явно разрешает его; иначе `503`. Система MUST NOT возвращать необработанный plaintext как `result`. RuBERT fallback при недоступном Python worker не называется, потому что RuBERT загружен внутри него.

#### Scenario: Vault unavailable fails closed
- **WHEN** vault недоступен при обработке `POST /process`
- **THEN** сервис возвращает `503` без `result`

#### Scenario: Model worker unavailable degrades when policy allows
- **WHEN** model worker недоступен и consumer policy явно разрешает rules-only degraded mode
- **THEN** сервис обрабатывает запрос в rules-only режиме

#### Scenario: Model worker unavailable fails closed when policy disallows
- **WHEN** model worker недоступен и consumer policy не разрешает degraded mode
- **THEN** сервис возвращает `503` без `result`

### Requirement: Metrics
Система SHALL предоставлять метрики latency, RPS и TPS на `GET /metrics`.

#### Scenario: Metrics expose latency, RPS and TPS
- **WHEN** клиент вызывает `GET /metrics`
- **THEN** ответ содержит метрики latency, RPS и TPS без раскрытия plaintext значений

### Requirement: Checker compatibility
Сервис SHALL быть совместимым с официальным checker: успешный ответ `POST /process` содержит только строковый `result`, а итоговый показатель checker не ниже 95 процентов. Метрики качества маскирования и восстановления определяются в capability `pii-detection`.

#### Scenario: Checker receives only result in success response
- **WHEN** официальный checker вызывает `POST /process` и получает успешный ответ
- **THEN** ответ содержит только строковое поле `result`

#### Scenario: Checker overall score is at least 95 percent
- **WHEN** официальный checker оценивает сервис
- **THEN** итоговый показатель не ниже 95 процентов

### Requirement: Onboarding compatibility of checking client
Проверяющий клиент SHALL использовать timeout 10 секунд, до двух повторов (три попытки) и остановку прогона после пяти последовательных invalid responses; `429` не является invalid и не сбрасывает счётчик. Это поведение tester, а не server-side counters; сервис MUST NOT реализовывать счётчик повторов/некорректных ответов тестера.

#### Scenario: Checking client retries idempotently
- **WHEN** проверяющий клиент повторяет запрос после timeout или некорректного ответа
- **THEN** повтор идемпотентен и возвращает валидный JSON

#### Scenario: Checking client stops after five consecutive invalid responses
- **WHEN** проверяющий клиент получает пять последовательных invalid responses
- **THEN** прогон останавливается

#### Scenario: Checking client treats 429 as not invalid
- **WHEN** проверяющий клиент получает `429`
- **THEN** `429` не считается invalid и не сбрасывает счётчик некорректных ответов

### Requirement: Detect API response
`POST /v1/pii/detect` SHALL принимать text, `include_non_personal` и `request_id`, возвращать `request_id`, `has_personal_data`, `detected_types` и entity metadata без исходных значений сущностей по умолчанию.

#### Scenario: Detect returns metadata without values
- **WHEN** detect получает текст `Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ`
- **THEN** ответ содержит типы, offsets, confidence, personal flag, owner metadata, sources и reason codes, но не возвращает найденное plaintext значение по умолчанию

### Requirement: Tokenize API response
`POST /v1/pii/tokenize` SHALL принимать text, `scope_id`, `ttl_seconds`, `request_id` и возвращать `tokenized_text`, `detected_types`, entity metadata и `scope_id` без plaintext mappings.

#### Scenario: Tokenize returns scoped tokenized text
- **WHEN** tokenize получает текст с подтвержденными ПДн и валидный `scope_id`
- **THEN** ответ содержит `tokenized_text`, `scope_id`, detected types и metadata без mapping values

### Requirement: Detokenize API response
`POST /v1/pii/detokenize` SHALL принимать text, `scope_id`, `mode`, `request_id` и возвращать `restored_text`, `resolved_token_count`, `unresolved_tokens` согласно режиму восстановления.

#### Scenario: Detokenize returns restored text count and unresolved list
- **WHEN** detokenize получает текст с известными tokens в режиме `preserve`
- **THEN** ответ содержит восстановленный текст, количество resolved tokens и список unresolved tokens

### Requirement: Scope revoke API
`DELETE /v1/pii/scopes/{scope_id}` SHALL отзывать все mappings указанного scope.

#### Scenario: Revoked scope cannot disclose mappings
- **WHEN** клиент отзывает scope и затем вызывает detokenize для token из этого scope
- **THEN** исходное значение не раскрывается
