## 1. Go foundation и API skeleton

- [ ] 1.1 Создать Go module, базовую структуру `cmd/pii-service` и `internal/*`; проверить `go test ./...`.
- [ ] 1.2 Реализовать config loading для API, model worker, vault TTL, model mode и ключей без логирования секретов; проверить unit tests для defaults/validation.
- [ ] 1.3 Реализовать `net/http` router для `/v1/pii/detect`, `/v1/pii/tokenize`, `/v1/pii/detokenize`, `/v1/pii/scopes/{scope_id}`, `/health/live`, `/health/ready`; проверить handler contract tests.

## 2. Python model worker

- [ ] 2.1 Создать `python/model_worker` с загрузкой `redmadrobot-rnd/rubert-base-pii-ner` и `vladlinv/ru-pii-ner-gliner2.5` один раз при старте; проверить smoke test worker health/inference.
- [ ] 2.2 Ограничить worker ответом spans/labels/confidence/model source без tokenization, mappings и ключей; проверить contract test JSON schema.
- [ ] 2.3 Добавить Go `modelclient` с timeout, fast mode и validation offsets; проверить unit tests invalid spans и model-source propagation.

## 3. Go rules и validators

- [ ] 3.1 Реализовать registry типов ПДн и candidate model для всех канонических типов; проверить unit tests registry lookup и extensibility.
- [ ] 3.2 Реализовать regex candidates для email, phone, passport, dates, address components, INN, bank card, CVV/PIN context; проверить table tests с synthetic fixtures.
- [ ] 3.3 Реализовать validators Luhn, INN_PERSON checksum, passport division context, postal-code address context, CVV/PIN local context; проверить positive/negative unit tests из acceptance scenarios.

## 4. Merge, context classification и ownership

- [ ] 4.1 Реализовать deterministic merge: exact duplicates, source preservation, overlap priority, component metadata, no nested/repeated tokens; проверить merge unit tests.
- [ ] 4.2 Реализовать contextual classification DATE/LOCATION в `BIRTH_DATE`, `PASSPORT_ISSUE_DATE`, `BIRTH_PLACE`, `ADDRESS`; проверить scenario tests для birth/passport/generic dates and locations.
- [ ] 4.3 Реализовать ownership scoring с positive/negative context, shared `owner_id`, organization context и `review_recommended`; проверить hard negatives: Пушкин и адрес отделения банка.

## 5. Tokenization и vault

- [ ] 5.1 Реализовать cryptographically random scoped tokens `<PII_TYPE_SUFFIX>` с reuse внутри scope и различием между scopes; проверить unit tests repeated/scope separation.
- [ ] 5.2 Реализовать replacement справа налево по offsets только для `personal=true`; проверить tokenization tests на отсутствие nested/repeated tokens.
- [ ] 5.3 Реализовать vault interface, in-memory adapter, AES-256-GCM persistent contract, TTL и revoke scope; проверить vault unit tests для expiry/revoke/cross-scope denial.
- [ ] 5.4 Реализовать detokenize strict/preserve без NER; проверить round trip, unknown token strict error и preserve unresolved tokens.

## 6. Audit logging и безопасность

- [ ] 6.1 Реализовать structured audit logging только разрешенных metadata; проверить log capture test, что synthetic ПДн values отсутствуют.
- [ ] 6.2 Запретить возврат tokenized text при ошибке сохранения mapping в vault; проверить failure injection test.
- [ ] 6.3 Проверить, что ошибки не логируют request/response body, Authorization, ciphertext, keys, CVV/PIN; проверить security unit tests.

## 7. Integration, E2E, Docker и README

- [ ] 7.1 Добавить integration tests Go service + mocked model worker для всех acceptance scenarios; проверить `go test ./...`.
- [ ] 7.2 Добавить E2E demo tokenize -> external processing preserving tokens -> detokenize; проверить automated E2E command.
- [ ] 7.3 Добавить Dockerfile, docker-compose для Go service и Python worker; проверить `docker compose config` и documented startup.
- [ ] 7.4 Добавить README с запуском Go-сервиса, Python worker-а, fast mode, конфигом vault и demo round trip; проверить команды из README вручную или smoke script.
