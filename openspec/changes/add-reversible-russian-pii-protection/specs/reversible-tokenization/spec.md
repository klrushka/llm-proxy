## Purpose

Capability задает обратимую токенизацию подтвержденных ПДн по `scope_id`, безопасное хранение mappings и восстановление текста после внешней обработки без повторного обнаружения ПДн.

## ADDED Requirements

### Requirement: Scoped opaque token generation
Система SHALL заменять только подтвержденные `personal=true` сущности токенами формата `<PII_TYPE_SUFFIX>`, где token содержит тип, но не содержит исходное значение, hash или ciphertext значения.

#### Scenario: Confirmed client data is tokenized
- **WHEN** tokenize получает текст с подтвержденными ПДн клиента и `scope_id`
- **THEN** `tokenized_text` содержит opaque tokens для подтвержденных сущностей и не содержит plaintext mappings

#### Scenario: Repeated value inside scope reuses token
- **WHEN** одинаковое значение встречается несколько раз внутри одного `scope_id`
- **THEN** все вхождения получают один и тот же token

#### Scenario: Same value in different scopes gets different tokens
- **WHEN** одинаковое значение токенизируется в двух разных `scope_id`
- **THEN** сгенерированные tokens различаются

### Requirement: Replacement by offsets
Система SHALL выполнять замену справа налево по offsets и MUST токенизировать внешний span один раз без вложенных и повторных tokens.

#### Scenario: Nested spans produce one outer token
- **WHEN** merged result содержит внешний `FULL_NAME` и компоненты имени внутри него
- **THEN** tokenized text содержит один token для внешнего span, а компоненты остаются metadata

### Requirement: Token vault storage
Vault SHALL хранить mapping `scope_id + token -> encrypted original value`, поддерживать TTL и revoke всего scope; persistent adapter MUST хранить только ciphertext.

#### Scenario: Mapping failure blocks tokenized response
- **WHEN** vault не может сохранить mapping для tokenized text
- **THEN** API возвращает ошибку и не возвращает `tokenized_text`

#### Scenario: Expired or revoked token is not disclosed
- **WHEN** token просрочен или его `scope_id` отозван
- **THEN** detokenize не раскрывает исходное значение

#### Scenario: Cross-scope token is not disclosed
- **WHEN** detokenize получает token из другого `scope_id`
- **THEN** token не раскрывается в текущем scope

### Requirement: Detokenize modes
Detokenizer SHALL находить tokens в тексте, разрешать mappings только в указанном `scope_id` и MUST не запускать NER.

#### Scenario: Round trip restores original values
- **WHEN** текст проходит tokenize, затем внешнюю обработку с сохраненными tokens, затем detokenize с тем же `scope_id`
- **THEN** исходные значения восстанавливаются один-в-один для неизмененных tokens

#### Scenario: Strict mode rejects unresolved token
- **WHEN** detokenize в режиме `strict` встречает неизвестный, чужой, просроченный или отозванный token
- **THEN** операция завершается ошибкой и частичный plaintext не возвращается

#### Scenario: Preserve mode keeps unresolved token
- **WHEN** detokenize в режиме `preserve` встречает неизвестный token
- **THEN** token остается без изменений и добавляется в `unresolved_tokens`

### Requirement: Restore by previously issued mask
Передача ранее выданной маски с тем же `payload_id` SHALL восстанавливать оригинал без повторного NER. Restore является повторяемым чтением, а не необратимым переходом.

#### Scenario: Passing previously issued mask restores original
- **WHEN** клиент передаёт ранее выданную маску с тем же `payload_id`
- **THEN** сервис восстанавливает оригинал без повторного NER и возвращает его как `result`

#### Scenario: Restore is repeatable read
- **WHEN** клиент повторно передаёт ранее выданную маску с тем же `payload_id`
- **THEN** сервис снова возвращает восстановленный оригинал, не переводя запись в необратимое состояние

### Requirement: Runtime flow demasking after LLM
В основном runtime flow LLM SHALL получать только защищённый текст, а демаскирование ответа LLM SHALL выполняться после вызова LLM перед ответом пользователю. Ошибки tokenization/vault/LLM/detokenization SHALL fail closed без plaintext fallback.

#### Scenario: LLM output is demasked before user response
- **WHEN** runtime flow получает ответ LLM, содержащий ранее выданные tokens
- **THEN** сервис демаскирует ответ LLM и возвращает пользователю восстановленный текст

#### Scenario: Detokenization failure fails closed in runtime flow
- **WHEN** демаскирование ответа LLM завершается ошибкой
- **THEN** runtime flow завершается ошибкой без plaintext fallback

### Requirement: Vault unavailability fails closed
При недоступности vault система SHALL возвращать `503` без `result` и MUST NOT возвращать необработанный plaintext как `result`.

#### Scenario: Vault unavailable fails closed
- **WHEN** vault недоступен при обработке запроса
- **THEN** сервис возвращает `503` без `result`
