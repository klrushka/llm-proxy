## Why

Нужен хакатонный сервис, который безопасно находит русскоязычные персональные данные, заменяет их обратимыми токенами перед передачей текста во внешнюю LLM и восстанавливает значения после обработки. Сейчас в репозитории нет требований, архитектуры и API-контракта для такого сервиса, поэтому сначала фиксируем поведение через OpenSpec.

## What Changes

- Добавляется capability обнаружения ПДн с Go rules/validators, Python NER worker-ом, контекстной классификацией типов и детерминированным merge spans.
- Добавляется capability определения принадлежности ПДн физлицу или организации с hard negatives и review metadata для неоднозначных сущностей.
- Добавляется capability обратимой токенизации и восстановления по `scope_id` с encrypted vault, TTL, revoke scope и режимами detokenize.
- Добавляется capability HTTP API для detect/tokenize/detokenize/scope revoke/health endpoints без возврата plaintext mappings.
- Фиксируются safety-ограничения: fine-tuning запрещен, Python не принимает privacy-решения, persistent vault хранит только ciphertext, логи не содержат значения ПДн.

## Capabilities

### New Capabilities
- `pii-detection`: Обнаружение и типизация русскоязычных ПДн через Go rules/validators, Python NER spans, contextual classification и deterministic merge.
- `pii-ownership`: Определение, является ли найденная сущность ПДн физлица, организации или неоднозначным упоминанием, с reason codes и review metadata.
- `reversible-tokenization`: Обратимая scoped-токенизация подтвержденных ПДн, encrypted vault, TTL, revoke и detokenize без повторного NER.
- `pii-api`: HTTP API и audit logging контракт для detect, tokenize, detokenize, scope revoke и health checks.

### Modified Capabilities
- Нет существующих capabilities для изменения.

## Impact

- Будет создан Go-сервис с ориентировочной структурой `cmd/pii-service`, `internal/api`, `internal/detection`, `internal/rules`, `internal/ownership`, `internal/tokenization`, `internal/vault`, `internal/audit`, `internal/modelclient`.
- Будет создан Python model worker только для готовых Hugging Face моделей `redmadrobot-rnd/rubert-base-pii-ner` и `vladlinv/ru-pii-ner-gliner2.5`.
- Будут добавлены Docker/README, unit/integration/E2E тесты и демонстрационный round trip.
- Реализация не входит в proposal-фазу и начнется только после review/approval этих артефактов.
