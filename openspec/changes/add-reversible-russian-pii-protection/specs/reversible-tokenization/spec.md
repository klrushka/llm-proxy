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
