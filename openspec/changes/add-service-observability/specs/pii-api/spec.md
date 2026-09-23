## MODIFIED Requirements

### Requirement: Metrics
Система SHALL отдавать метрики в формате Prometheus на `GET /metrics` отдельного внутреннего listener-а `PII_METRICS_LISTEN_ADDRESS`, который не проходит consumer access и не обслуживает data-маршруты. Основной API listener MUST NOT отдавать метрики. Метрики SHALL включать HTTP-метрики сервера и клиента по OpenTelemetry semantic conventions, метрики Go runtime и процесса, доменные метрики найденных ПДн и vault. Значения меток MUST браться только из фиксированных перечислений и MUST NOT содержать фактический путь, `scope_id`, `payload_id`, system ID, текст, токены или значения ПДн.

#### Scenario: Metrics expose latency, RPS and TPS
- **WHEN** сборщик вызывает `GET /metrics` на metrics listener
- **THEN** ответ содержит гистограмму `http_server_request_duration_seconds` и счётчик размера входа, из которых выводятся latency, RPS и TPS, без раскрытия plaintext значений

#### Scenario: Metrics available in both access profiles
- **WHEN** сервис запущен в профиле `checker` или `production` и сборщик вызывает `GET /metrics` на metrics listener без ключа
- **THEN** ответ `200` с метриками

#### Scenario: API listener does not serve metrics
- **WHEN** клиент вызывает `GET /metrics` на основном API listener
- **THEN** метрики не возвращаются

#### Scenario: Every data route is measured
- **WHEN** обработан запрос к `POST /process`, `/v1/pii/*` или `POST /v1/runtime/chat`
- **THEN** наблюдение попадает в `http_server_request_duration_seconds` с метками `http_request_method`, `http_route` (шаблон маршрута) и `http_response_status_code`

#### Scenario: Access denials and overload are measured
- **WHEN** запрос отклонён access control (`401`/`403`) или admission (`429`)
- **THEN** наблюдение попадает в `http_server_request_duration_seconds` с этим `http_response_status_code`

#### Scenario: Dependencies are measured
- **WHEN** сервис вызывает model worker или внешнюю LLM
- **THEN** наблюдение попадает в `http_client_request_duration_seconds` с меткой `server` и исходом вызова

#### Scenario: Revoke path does not leak scope_id
- **WHEN** обработан `DELETE /v1/pii/scopes/{scope_id}`
- **THEN** метрики содержат `http_route="/v1/pii/scopes/{scope_id}"` и не содержат значение `scope_id`

#### Scenario: Detected entities are counted by type
- **WHEN** pipeline подтвердил сущности ПДн
- **THEN** `pii_entities_detected_total` увеличивается по метке `type` из canonical registry без значений сущностей
