## Workstreams and dependencies

Четыре параллельных workstream после стабилизации foundation и acceptance contracts:

- **WS-A (core /process):** фазы 1, 2, 3 — routers/handlers, state machine, overload, consumer policy.
- **WS-B (detection):** фазы 4, 5, 6, 8 — Python worker, model client, rules/validators, merge/ownership, long text windowing.
- **WS-C (tokenization):** фаза 7 — tokenization, vault, detokenize.
- **WS-D (ops/quality):** фазы 9, 10, 11 — audit/security, metrics/quality, performance.

Реальный момент разделения четырёх участников: сразу после фазы 0. WS-A берёт фазы 1–3, WS-B — фазы 4–6 и 8, WS-C — фазу 7, WS-D — фазы 9–11. WS-B и WS-C расходятся сразу после фазы 0: WS-C может начинать token generation и vault interface параллельно, не дожидаясь merge/ownership.

## Task-level dependency map

- Go module (0.1) блокирует только Go-задачи; Python model worker (4.x) и corpus fixtures (0.3) могут стартовать параллельно сразу.
- `/process` domain types (0.4) блокируют `/process` handler/state tasks (1.3–1.8).
- consumer-policy types (0.5) блокируют policy (3.x), degraded-mode (2.4) и ownership policy integration (6.3).
- model worker contract (4.2) блокирует Go model client (5.1) и long-text model integration (8.x); rules families (5.3–5.9) могут идти параллельно.
- registry/merge/ownership (5.2, 6.1–6.3) блокируют только использующие их части tokenization (7.2, 7.5); token generation (7.1) и vault interface (7.3) могут идти параллельно раньше.
- local quality harness (10.2) зависит от corpus fixtures (0.3); load/official quality gates (10.3, 11.1) зависят от интегрированного runnable service, а не искусственно от audit -> metrics -> performance.
- integration (12.x) зависит от соответствующих feature slices; deployment/live gates (13.x) — от runnable integration.
- security gates (9.4, 9.5) независимы от feature slices и могут выполняться параллельно сразу после появления базовой структуры репозитория.

## 0. Foundation и acceptance contracts

- [x] 0.1 Создать Go module и базовую структуру `cmd/pii-service`, `internal/*`; observable check: `go test ./...` проходит.
- [x] 0.2 Реализовать config loading для API, model worker, vault TTL, model mode и ключей без логирования секретов; observable check: unit tests для defaults/validation проходят.
- [x] 0.3 Зафиксировать локальный synthetic corpus schema, fixtures и ожидаемые spans+masks (без scorer/harness); observable check: fixtures загружаются и валидируются.
- [x] 0.4 Зафиксировать Go domain types для `/process` (обязательные строковые `payload`/`payload_id`, успешный ответ `200` со строковым `result`) и record state machine `claim`/`ready`/`expired`; observable check: contract unit tests проходят.
- [x] 0.5 Зафиксировать consumer policy types (benchmark/default consumer, per-system типы ПДн и demasking); observable check: policy unit tests проходят.

## 1. Routers, handlers и state semantics (WS-A)

- [x] 1.1 Реализовать base routing для `/health/live`, `/health/ready`, `/metrics`; observable check: handler contract tests проходят.
- [x] 1.2 Реализовать extended `/v1/pii/*` handlers (detect/tokenize/detokenize/scope revoke); observable check: handler contract tests проходят.
- [x] 1.3 Реализовать `/process` adapter handler с exact contract tests (обязательные строковые `payload`/`payload_id`, успешный ответ ровно с `result`); observable check: contract tests проходят.
- [x] 1.4 Реализовать record state machine `claim`/`ready`/`expired` для `payload_id`; observable check: state machine unit tests проходят.
- [x] 1.5 Реализовать идемпотентный retry оригинала (повтор исходного payload возвращает ту же маску); observable check: idempotency unit tests проходят.
- [x] 1.6 Реализовать восстановление по ранее выданной маске без NER (repeatable read); observable check: restore unit tests проходят.
- [ ] 1.7 Реализовать безопасный `409` для третьего несвязанного payload без изменения записи; observable check: conflict unit tests проходят.
- [ ] 1.8 Реализовать atomic claim/single writer для конкурентного первого запроса; observable check: concurrency unit tests проходят.

## 2. Overload и backpressure (WS-A)

- [ ] 2.1 Реализовать bounded concurrency и backpressure; observable check: load unit tests проходят.
- [ ] 2.2 Реализовать `429` с `Retry-After` при перегрузке; observable check: overload handler tests проходят.
- [ ] 2.3 Реализовать fail closed `503` без `result` при недоступности vault; observable check: failure injection test проходят.
- [ ] 2.4 Реализовать rules-only degraded mode только при явном разрешении consumer policy, иначе `503`; observable check: degradation unit tests проходят.

## 3. Consumer policy (WS-A)

- [ ] 3.1 Реализовать transport-resolved consumer policy с benchmark/default consumer; observable check: policy resolution unit tests проходят.
- [ ] 3.2 Реализовать per-system настройки типов ПДн; observable check: per-system ownership tests проходят.
- [ ] 3.3 Реализовать per-system настройки demasking; observable check: per-system demasking tests проходят.

## 4. Python model worker (WS-B)

- [ ] 4.1 Создать `python/model_worker` с загрузкой `redmadrobot-rnd/rubert-base-pii-ner` и `vladlinv/ru-pii-ner-gliner2.5` один раз при старте; observable check: smoke test worker health/inference проходит.
- [ ] 4.2 Ограничить worker ответом spans/labels/confidence/model source без tokenization, mappings и ключей; observable check: contract test JSON schema проходит.

## 5. Go model client и rules/validators (WS-B)

- [ ] 5.1 Реализовать Go model client: timeout/cancellation, fast/model mode, JSON schema, source propagation, bounds/UTF-8 offset validation; observable check: unit tests для invalid spans и model-source propagation проходят.
- [ ] 5.2 Реализовать registry типов ПДн и candidate model для всех канонических типов; observable check: registry lookup unit tests проходят.
- [ ] 5.3 Реализовать regex candidates для email и phone; observable check: table tests с synthetic fixtures проходят.
- [ ] 5.4 Реализовать regex candidates для passport, division code и driver license; observable check: table tests с synthetic fixtures проходят.
- [ ] 5.5 Реализовать regex candidates и checksum для INN (person/organization); observable check: positive/negative unit tests проходят.
- [ ] 5.6 Реализовать regex candidates и Luhn для bank card; observable check: positive/negative unit tests проходят.
- [ ] 5.7 Реализовать regex candidates для dates, включая текстовые даты; observable check: table tests с synthetic fixtures проходят.
- [ ] 5.8 Реализовать regex candidates для address и postal code; observable check: table tests с synthetic fixtures проходят.
- [ ] 5.9 Реализовать regex candidates и context validators для CVV/PIN/cardholder; observable check: positive/negative unit tests проходят.

## 6. Merge, context classification и ownership (WS-B)

- [ ] 6.1 Реализовать deterministic merge: exact duplicates, source preservation, overlap priority, component metadata, no nested/repeated tokens; observable check: merge unit tests проходят.
- [ ] 6.2 Реализовать contextual classification DATE/LOCATION в `BIRTH_DATE`, `PASSPORT_ISSUE_DATE`, `BIRTH_PLACE`, `ADDRESS`; observable check: scenario tests проходят.
- [ ] 6.3 Реализовать ownership scoring с positive/negative context, shared `owner_id`, organization context и `review_recommended`; observable check: hard negatives tests проходят.

## 7. Tokenization и vault (WS-C)

- [ ] 7.1 Реализовать cryptographically random scoped tokens `<PII_TYPE_SUFFIX>` с reuse внутри scope и различием между scopes; observable check: unit tests repeated/scope separation проходят.
- [ ] 7.2 Реализовать replacement справа налево по offsets только для `personal=true`; observable check: tokenization tests на отсутствие nested/repeated tokens проходят.
- [ ] 7.3 Реализовать vault interface и in-memory demo adapter; observable check: vault interface unit tests проходят.
- [ ] 7.4 Реализовать TTL и revoke scope; observable check: vault unit tests для expiry/revoke/cross-scope denial проходят.
- [ ] 7.5 Реализовать detokenize strict/preserve без NER; observable check: round trip, unknown token strict error и preserve unresolved tokens проходят.

## 8. Long text windowing (WS-B)

- [ ] 8.1 Реализовать windowing/chunking NER с overlap и bounded parallelism; observable check: windowing unit tests проходят.
- [ ] 8.2 Реализовать восстановление глобальных UTF-8 offsets из окон; observable check: offset reconstruction unit tests проходят.
- [ ] 8.3 Реализовать подсчёт токенов через tokenizer выбранной модели и acceptance для входа до 100 000 токенов; observable check: token-count acceptance test проходит.

## 9. Audit logging и безопасность (WS-D)

- [ ] 9.1 Реализовать structured audit logging только разрешенных metadata; observable check: log capture test, что synthetic ПДн values отсутствуют, проходит.
- [ ] 9.2 Запретить возврат tokenized text при ошибке сохранения mapping в vault; observable check: failure injection test проходит.
- [ ] 9.3 Проверить, что ошибки не логируют request/response body, Authorization, ciphertext, keys, CVV/PIN; observable check: security unit tests проходят.
- [ ] 9.4 Реализовать воспроизводимый Gitleaks gate: локальная команда и CI, сканирование Git history и текущих файлов, redacted output, без baseline для чистого нового репозитория; observable check: оба режима завершаются без findings.
- [ ] 9.5 Реализовать минимальный Semgrep CE SAST gate: без cloud token, explicit ruleset (не `--config auto`), metrics off, blocking только ERROR; локальная команда и CI; observable check: scan текущего Go/Python source проходит.

## 10. Метрики и quality tooling (WS-D)

- [ ] 10.1 Реализовать метрики latency, RPS и TPS на `GET /metrics`; observable check: metrics endpoint test проходит.
- [ ] 10.2 Реализовать scorer/harness для per-type precision/recall/F1, нормализованного span-based Levenshtein качества маскирования и exact-match восстановления; observable check: harness запускается на локальном synthetic corpus.
- [ ] 10.3 Проверить целевой итоговый показатель официального checker не ниже 95 процентов; observable check: quality test проходит.

## 11. Производительность (WS-D)

- [ ] 11.1 Реализовать бенчмарк целевой нагрузки 1000 RPS при полном прогоне около 5 минут; собирать p50/p95/p99 и проверять product target latency не более 1 секунды, поясняя, что Appendix называет его целевым ориентиром; observable check: benchmark проходит и отчёт содержит p50/p95/p99.

## 12. Integration, E2E, Docker и README

- [ ] 12.1 Добавить integration tests для extended `/v1/pii/*` API; observable check: `go test ./...` проходит.
- [ ] 12.2 Добавить integration tests для `/process` state/idempotency/concurrency/errors; observable check: `go test ./...` проходит.
- [ ] 12.3 Добавить integration tests для degradation/overload/no-plaintext leakage; observable check: `go test ./...` проходит.
- [ ] 12.4 Добавить E2E demo mask -> external processing preserving tokens -> restore; observable check: automated E2E command проходит.
- [ ] 12.5 Добавить Dockerfile и docker-compose для Go service и Python worker; observable check: `docker compose config` проходит и startup документирован.
- [ ] 12.6 Добавить README с запуском Go-сервиса, Python worker-а, fast mode, конфигом vault, demo round trip и инструкцией настройки consumer policy не более пяти предложений; observable check: команды из README проверены вручную или smoke script.
- [ ] 12.7 Реализовать минимальный Go runtime coordinator с инъецируемым/настраиваемым LLM client boundary и покрыть его automated E2E полного пути mask -> LLM -> demask: fake LLM подтверждает, что наружу к LLM уходит только защищённый текст, stage errors (tokenization/vault/LLM/detokenization) fail closed без plaintext fallback, а `POST /process` остаётся отдельным mask/restore контуром и сам LLM не вызывает; observable check: automated E2E команда проходит.

## 13. Source-only ZIP, deployment и live smoke

- [ ] 13.1 Добавить source-only ZIP verification с исключениями `.git`, `.venv`, binaries, caches, datasets, archives, media и secrets; observable check: ZIP собирается и проверяется на отсутствие исключённых элементов.
- [ ] 13.2 Добавить deployment/runbook; observable check: runbook команды выполняются.
- [ ] 13.3 Добавить live smoke после deployment по выбранному deployed URL (HTTP или HTTPS; для HTTPS smoke поддерживает self-signed режим checker-а); observable check: live smoke проходит по выбранному протоколу.

## Stretch (необязательно, не блокирует completion)

- Бонусный профиль 2000 RPS: реализовать и прогнать бенчмарк 2000 RPS; observable check: benchmark проходит, не блокирует основной профиль.