## Purpose

Capability определяет HTTP API и audit logging контракт сервиса обнаружения, токенизации, восстановления и управления scope без раскрытия plaintext ПДн в логах или ответах по умолчанию.

## ADDED Requirements

### Requirement: HTTP endpoints
Go-сервис SHALL предоставлять `POST /v1/pii/detect`, `POST /v1/pii/tokenize`, `POST /v1/pii/detokenize`, `DELETE /v1/pii/scopes/{scope_id}`, `GET /health/live`, `GET /health/ready`.

#### Scenario: Health endpoints are available
- **WHEN** клиент вызывает live и ready endpoints
- **THEN** сервис возвращает состояние процесса и готовность зависимостей без раскрытия конфигурационных секретов

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

### Requirement: Audit logging safety
Система SHALL логировать request_id, operation, наличие ПДн, найденные типы, количество сущностей, personal flags, sources, reason codes, duration, model mode и результат операции; система MUST NOT логировать исходный текст, найденные значения, восстановленный текст, mappings, CVV/PIN, ciphertext, ключи, Authorization headers или request/response body при ошибке.

#### Scenario: Logs contain types but not values
- **WHEN** запрос обрабатывает synthetic ПДн
- **THEN** перехваченные логи содержат найденные типы и metadata, но не содержат synthetic plaintext values

#### Scenario: Error logs do not include bodies or secrets
- **WHEN** обработчик возвращает ошибку валидации, vault или model worker
- **THEN** лог ошибки не содержит request body, response body, Authorization header, ciphertext или ключи
