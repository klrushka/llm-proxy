## Context

Репозиторий зеленый: рабочей реализации пока нет, есть только brief и OpenSpec каркас. Проект должен сохранить жесткую границу: Go принимает решения, хранит и восстанавливает mappings, а Python worker только возвращает модельные spans.

## Goals / Non-Goals

**Goals:**
- Спроектировать единый pipeline `detect -> merge -> context classification -> ownership -> tokenize` на Go.
- Спроектировать основной runtime flow `запрос пользователя -> маскирование/токенизация ПДн -> вызов настраиваемой LLM -> демаскирование ответа LLM -> ответ пользователю` с оркестрацией на Go, где LLM получает только защищённый текст, а ошибки tokenization/vault/LLM/detokenization fail closed без plaintext fallback.
- Ограничить Python worker ролью адаптера готовых NER моделей.
- Обеспечить обратимость через `scope_id`, encrypted vault и detokenize без повторного NER.
- Сформировать API и audit logging так, чтобы plaintext ПДн и секреты не попадали в ответы и логи.
- Добавить тонкий benchmark adapter `POST /process` со строгим контрактом обязательных строковых `payload`/`payload_id` -> строковый `result` и record state machine для `payload_id`.
- Обеспечить overload через bounded concurrency/backpressure с `429` и `Retry-After`, fail closed при недоступности vault и rules-only degraded mode только при явном разрешении consumer policy.
- Обеспечить поддержку входа до 100 000 токенов через windowing/chunking NER с overlap, bounded parallelism и восстановлением глобальных UTF-8 offsets.
- Обеспечить метрики latency/RPS/TPS и load/quality tooling с целевым итоговым показателем официального checker не ниже 95 процентов и целевой нагрузкой 1000 RPS.

**Non-Goals:**
- Fine-tuning моделей.
- Хранение plaintext mappings в persistent storage.
- Расширение набора типов сверх перечисленного в brief.
- Поддержка произвольных языков кроме русскоязычного текста в рамках этой change.
- Удаление или замена существующих `/v1/pii/*` endpoints: они сохраняются как отдельный расширенный API.
- Вызов LLM внутри benchmark adapter `POST /process`: adapter остаётся отдельным mask/restore контуром с текущим протоколом checker и сам LLM не вызывает.
- Реализация счётчика повторов/некорректных ответов проверяющего клиента внутри сервиса: timeout, повторы и остановка после некорректных ответов описываются только как свойства проверяющего клиента.

## Decisions

1. Основной сервис реализуется на Go 1.23+ и стандартном `net/http`.

   Альтернатива: HTTP framework. Отклонено, потому что brief рекомендует `net/http`, а зеленый проект не требует дополнительных зависимостей.

2. Python worker предоставляет один внутренний inference API и возвращает только `{label,start,end,confidence,model}`.

   Альтернатива: выполнять tokenization или ownership рядом с моделями. Отклонено из-за требования не давать Python доступ к mappings, ключам и privacy-решениям.

3. Go pipeline нормализует кандидатов в общий `EntityCandidate` с offsets исходного текста, sources и metadata.

   Альтернатива: хранить отдельные структуры для regex и NER до поздней стадии. Отклонено, потому что merge/ownership должны одинаково обрабатывать sources и overlap.

4. Validators имеют приоритет над широкими NER spans при пересечениях.

   Альтернатива: выбирать highest confidence. Отклонено, потому что структурные идентификаторы проверяются детерминированно и должны не теряться внутри широких spans.

5. Ownership возвращает `personal=false` для отрицательного и неоднозначного контекста, а неоднозначность отмечает `review_recommended=true`.

   Альтернатива: токенизировать все найденные NER сущности. Отклонено, потому что brief требует не токенизировать Пушкина, адрес банка и неоднозначные сущности.

6. Token vault задается интерфейсом с in-memory demo adapter и persistent adapter contract, где persistent значения хранятся только как AES-256-GCM ciphertext.

   Альтернатива: начать только с persistent storage. Отклонено для хакатонного demo, но интерфейс должен сохранить возможность persistent adapter.

7. Detokenizer ищет токены regex-ом и разрешает их через vault по `scope_id`, не вызывая detection pipeline.

   Альтернатива: rerun NER перед восстановлением. Отклонено, потому что LLM output может отличаться от исходного текста, а brief запрещает повторный NER для detokenize.

8. Публичный benchmark adapter `POST /process` имеет строгий контракт: принимает JSON с обязательными строковыми полями `payload` и `payload_id`, успешный ответ `200` содержит строковое поле `result`. Существующие `/v1/pii/*` endpoints сохраняются как отдельный расширенный API и не удаляются.

   Альтернатива: сделать `/process` единственным публичным API. Отклонено, потому что расширенный API уже спроектирован, а `/process` нужен как тонкий adapter для проверяющего стенда.

9. Record state machine для `payload_id` использует минимальные состояния `claim`/`ready`/`expired` (или эквивалентную record state machine). Для нового `payload_id` вход маскируется и correlation record атомарно сохраняется. Повтор исходного payload идемпотентно возвращает ту же ранее выданную маску. Передача ранее выданной маски с тем же `payload_id` восстанавливает оригинал без NER. Любой третий несвязанный payload для того же id возвращает безопасный `409` и не меняет запись. Restore является повторяемым чтением, а не необратимым переходом в `RESTORED`.

   Альтернатива: необратимый переход в `RESTORED`. Отклонено, потому что restore должен быть повторяемым чтением.

10. Concurrent первый запрос использует atomic claim/single writer: одинаковые запросы получают один стабильный result, конфликтующие не перезаписывают запись.

    Альтернатива: последний запрос побеждает. Отклонено, потому что это нарушает идемпотентность и стабильность result.

11. Идентичность системы не добавляется в body `/process`. Применяется transport-resolved consumer policy с заранее настроенным benchmark/default consumer для проверяющего стенда; production identity может приходить из доверенного заголовка или сетевого контура. Нестандартный header не делается обязательным для benchmark.

    Альтернатива: обязательный `system_id` в body. Отклонено, потому что это нарушает строгий контракт `/process`.

12. Overload реализуется через bounded concurrency/backpressure с `429` и `Retry-After`. При недоступности vault всегда fail closed: `503` без `result`. Rules-only degraded mode допустим только если consumer policy явно разрешает его; иначе `503`. Никогда не возвращается необработанный plaintext как `result`. RuBERT fallback при недоступном Python worker не называется, потому что RuBERT загружен внутри него.

    Альтернатива: всегда деградировать до rules-only. Отклонено, потому что это может раскрыть plaintext и противоречит fail closed.

13. Поддержка входа до 100 000 токенов реализуется через windowing/chunking NER с overlap, bounded parallelism и восстановлением глобальных UTF-8 offsets. Способ подсчёта токенов привязывается к tokenizer выбранной модели и фиксируется как проверяемый acceptance. Токены не подменяются символами или словами.

    Альтернатива: обрабатывать весь текст одним вызовом. Отклонено, потому что модели имеют ограничение контекста.

14. Метрики latency/RPS/TPS и load/quality tooling: качество маскирования считается нормализованным span-based Levenshtein в диапазоне 0..1, восстановление сравнивается exact с исходной строкой, локально дополнительно считаются per-type precision/recall/F1. Целевой итоговый показатель официального checker не ниже 95 процентов; не утверждается, что каждый precision и recall обязан быть 95 процентов.

    Альтернатива: требовать 95 процентов по каждому precision и recall. Отклонено, потому что итоговый показатель checker является целевым.

15. Целевая нагрузка официального прогона — 1000 RPS при полном прогоне около 5 минут; latency не более 1 секунды является целевым ориентиром, а не жёстким правилом checker. Бонусный профиль 2000 RPS необязателен и не блокирует completion.

    Альтернатива: трактовать latency <=1s как жёсткое правило checker. Отклонено, потому что Appendix называет его целевым ориентиром.

16. Основной runtime flow реализуется как отдельный контур: запрос пользователя -> маскирование/токенизация ПДн -> вызов настраиваемой LLM -> демаскирование ответа LLM -> ответ пользователю. Оркестрация выполняется на Go, LLM получает только защищённый текст, а ошибки tokenization/vault/LLM/detokenization fail closed без plaintext fallback. Benchmark adapter `POST /process` остаётся отдельным mask/restore контуром с текущим протоколом checker и сам LLM не вызывает.

    Альтернатива: встроить вызов LLM в `POST /process`. Отклонено, потому что `/process` должен оставаться тонким benchmark adapter для проверяющего стенда, а runtime flow с LLM является отдельным продуктовым контуром.

## Risks / Trade-offs

- Модели могут вернуть некорректные offsets -> Go model client должен валидировать boundaries и отбрасывать invalid spans.
- Regex для CVV/PIN может давать false positives -> validators обязаны требовать локальный контекст `CVV`, `CVC`, `код безопасности`, `PIN`, `ПИН` или `пин-код`.
- Merge nested spans может потерять компоненты ФИО или адреса -> внешний span токенизируется один раз, а компоненты сохраняются в metadata.
- In-memory vault не переживает restart -> README должен маркировать его как demo adapter; persistent adapter хранит только ciphertext.
- Логи легко загрязнить plaintext на error path -> добавить тест-перехватчик логов и запретить логирование request/response body.
- Concurrent первый запрос может гоняться -> atomic claim/single writer гарантирует один стабильный result и не перезаписывает запись.
- Недоступность vault -> fail closed `503` без `result`; никогда не возвращать необработанный plaintext как `result`.
- Недоступность model worker -> rules-only degraded mode только при явном разрешении consumer policy; иначе `503`.
- Long text до 100 000 токенов -> windowing/chunking NER с overlap, bounded parallelism и восстановлением глобальных UTF-8 offsets; способ подсчёта токенов привязан к tokenizer модели.
- Quality gate зависит от официального checker -> локальный synthetic corpus фиксируется как fixture для локального harness; целевой итоговый показатель официального checker не ниже 95 процентов.

## Migration Plan

Проект создается с нуля. Rollback для implementation phase: откатить задачу-коммит, соответствующий одному пункту `tasks.md`. Общие domain/API contracts (контракт `/process`, record state machine, consumer policy, synthetic corpus) стабилизируются первыми, затем независимые задачи распределяются между четырьмя параллельными workstream.
