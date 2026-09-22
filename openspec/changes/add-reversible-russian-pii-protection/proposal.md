## Why

Нужен хакатонный сервис, который безопасно находит русскоязычные персональные данные, заменяет их обратимыми токенами перед передачей текста во внешнюю LLM и восстанавливает значения после обработки. Сейчас в репозитории нет требований, архитектуры и API-контракта для такого сервиса, поэтому сначала фиксируем поведение через OpenSpec.

## What Changes

- Добавляется capability обнаружения ПДн с Go rules/validators, Python NER worker-ом, контекстной классификацией типов и детерминированным merge spans.
- Добавляется capability определения принадлежности ПДн физлицу или организации с hard negatives и review metadata для неоднозначных сущностей.
- Добавляется capability обратимой токенизации и восстановления по `scope_id` с encrypted vault, TTL, revoke scope и режимами detokenize.
- Добавляется расширенный HTTP API detect/tokenize/detokenize/scope revoke/health endpoints без возврата plaintext mappings.
- Добавляется основной runtime flow: запрос пользователя -> маскирование/токенизация ПДн -> вызов настраиваемой LLM -> демаскирование ответа LLM -> ответ пользователю. Оркестрация реализуется на Go, LLM получает только защищённый текст, ошибки tokenization/vault/LLM/detokenization fail closed без plaintext fallback.
- Добавляется тонкий benchmark adapter `POST /process` со строгим контрактом: принимает JSON с обязательными строковыми полями `payload` и `payload_id`, успешный ответ `200` содержит строковое поле `result`. Существующие `/v1/pii/*` endpoints сохраняются как отдельный расширенный API. `POST /process` остаётся отдельным benchmark adapter с текущим mask/restore протоколом checker и сам LLM не вызывает.
- Добавляется record state machine для `payload_id`: claim/ready/expired, идемпотентный retry оригинала, восстановление по ранее выданной маске без NER, безопасный `409` для третьего несвязанного payload, atomic claim для конкурентного первого запроса.
- Добавляется transport-resolved consumer policy с заранее настроенным benchmark/default consumer для проверяющего стенда; production identity может приходить из доверенного заголовка или сетевого контура.
- Добавляется overload через bounded concurrency/backpressure с `429` и `Retry-After`; fail closed `503` при недоступности vault; rules-only degraded mode только при явном разрешении consumer policy.
- Добавляются метрики latency/RPS/TPS и load/quality tooling: качество маскирования считается нормализованным span-based Levenshtein в диапазоне 0..1, восстановление сравнивается exact с исходной строкой, локально дополнительно считаются per-type precision/recall/F1; целевой итоговый показатель официального checker не ниже 95 процентов.
- Добавляется поддержка входа до 100 000 токенов через windowing/chunking NER с overlap, bounded parallelism и восстановлением глобальных UTF-8 offsets.
- Добавляются source-only ZIP verification, deployment/runbook и live smoke.
- Фиксируются safety-ограничения: fine-tuning запрещен, Python не принимает privacy-решения, persistent vault хранит только ciphertext, логи не содержат значения ПДн.

## Capabilities

### New Capabilities
- `pii-detection`: Обнаружение и типизация русскоязычных ПДн через Go rules/validators, Python NER spans, contextual classification и deterministic merge.
- `pii-ownership`: Определение, является ли найденная сущность ПДн физлица, организации или неоднозначным упоминанием, с reason codes и review metadata.
- `reversible-tokenization`: Обратимая scoped-токенизация подтвержденных ПДн, encrypted vault, TTL, revoke и detokenize без повторного NER.
- `pii-api`: HTTP API и audit logging контракт для detect, tokenize, detokenize, scope revoke и health checks, а также benchmark adapter `POST /process` с record state machine, consumer policy, overload и метриками.

### Modified Capabilities
- Нет существующих capabilities для изменения.

## Impact

- Будет создан Go-сервис с ориентировочной структурой `cmd/pii-service`, `internal/api`, `internal/detection`, `internal/rules`, `internal/ownership`, `internal/tokenization`, `internal/vault`, `internal/audit`, `internal/modelclient`.
- Будут добавлены реально нужные компоненты: benchmark adapter/correlation (`internal/process`), consumer policy (`internal/policy`), метрики (`internal/metrics`) и load/quality tooling.
- Будет создан Python model worker только для готовых Hugging Face моделей `redmadrobot-rnd/rubert-base-pii-ner` и `vladlinv/ru-pii-ner-gliner2.5`.
- Будут добавлены Docker/README, unit/integration/E2E тесты, демонстрационный round trip, source-only ZIP verification, deployment/runbook и live smoke.
- Реализация не входит в proposal-фазу и начнется только после review/approval этих артефактов.
