## Purpose

Capability задает поведение обнаружения, типизации и объединения русскоязычных кандидатов ПДн из Go rules/validators и Python NER без принятия ownership-решений в Python.

## ADDED Requirements

### Requirement: Supported PII type registry
Система SHALL поддерживать registry для типов `FULL_NAME`, `FIRST_NAME`, `LAST_NAME`, `MIDDLE_NAME`, `BIRTH_DATE`, `BIRTH_PLACE`, `PASSPORT_NUMBER`, `CITIZENSHIP`, `PASSPORT_ISSUER`, `PASSPORT_DIVISION_CODE`, `PASSPORT_ISSUE_DATE`, `DRIVER_LICENSE_NUMBER`, `ADDRESS`, `ADDRESS_COUNTRY`, `ADDRESS_POSTAL_CODE`, `ADDRESS_REGION`, `ADDRESS_CITY`, `ADDRESS_STREET`, `ADDRESS_HOUSE`, `ADDRESS_BUILDING`, `ADDRESS_APARTMENT`, `EMAIL`, `PHONE`, `INN_PERSON`, `BANK_CARD_NUMBER`, `CARD_CVV`, `CARD_PIN`, `CARDHOLDER_NAME`.

#### Scenario: Registry covers required canonical names
- **WHEN** сервис загружает registry типов
- **THEN** каждый канонический тип из требования доступен для detection result, token type и API metadata

#### Scenario: New type can be registered without pipeline rewrite
- **WHEN** разработчик добавляет новый тип через registry
- **THEN** основной detection pipeline принимает этот тип без изменения порядка стадий pipeline

### Requirement: Python NER worker contract
Python worker MUST загружать готовые модели один раз при старте и возвращать только spans, labels, confidence и model source с offsets исходного текста `start` inclusive, `end` exclusive.

#### Scenario: Worker returns model candidates
- **WHEN** Go-сервис отправляет worker-у текст с именем
- **THEN** worker возвращает entities с `label`, `start`, `end`, `confidence`, `model` и не возвращает plaintext mappings или privacy decisions

#### Scenario: Case-insensitive detection preserves offsets
- **WHEN** текст содержит ФИО в обычном, верхнем или смешанном регистре
- **THEN** detection находит ФИО без предварительного lowercasing исходного текста и возвращает offsets исходной строки

### Requirement: Long text windowing
Система SHALL поддерживать вход до 100 000 токенов через windowing/chunking NER с overlap, bounded parallelism и восстановлением глобальных UTF-8 offsets. Способ подсчёта токенов SHALL привязываться к tokenizer выбранной модели и фиксироваться как проверяемый acceptance. Токены MUST NOT подменяться символами или словами.

#### Scenario: Long text is processed in windows with global offsets
- **WHEN** входной текст превышает контекст модели
- **THEN** текст разбивается на окна с overlap, обрабатывается с bounded parallelism, а результаты объединяются с восстановлением глобальных UTF-8 offsets

#### Scenario: Token counting is bound to model tokenizer
- **WHEN** сервис определяет размер входа
- **THEN** подсчёт токенов использует tokenizer выбранной модели, а не символы или слова

### Requirement: Degraded mode on model worker unavailability
При недоступности model worker система SHALL применять rules-only degraded mode только если consumer policy явно разрешает его; иначе возвращать `503`. RuBERT fallback при недоступном Python worker не называется, потому что RuBERT загружен внутри него.

#### Scenario: Rules-only mode when policy allows
- **WHEN** model worker недоступен и consumer policy явно разрешает rules-only degraded mode
- **THEN** detection выполняется только Go rules/validators

#### Scenario: Rules-only mode fails closed when policy disallows
- **WHEN** model worker недоступен и consumer policy не разрешает degraded mode
- **THEN** сервис возвращает `503` без `result`

### Requirement: Detection quality metrics
Качество маскирования SHALL считаться нормализованным span-based Levenshtein в диапазоне 0..1; восстановление SHALL сравниваться exact с исходной строкой; целевой итоговый показатель официального checker SHALL быть не ниже 95 процентов. Локально дополнительно считаются per-type precision/recall/F1. Не утверждается, что каждый precision и recall обязан быть 95 процентов.

#### Scenario: Masking quality uses normalized span-based Levenshtein
- **WHEN** официальный checker оценивает качество маскирования
- **THEN** качество считается нормализованным span-based Levenshtein в диапазоне 0..1

#### Scenario: Restoration is compared exact to original
- **WHEN** официальный checker оценивает восстановление
- **THEN** восстановленный текст сравнивается exact с исходной строкой

#### Scenario: Overall checker score is at least 95 percent
- **WHEN** официальный checker оценивает сервис
- **THEN** итоговый показатель не ниже 95 процентов

#### Scenario: Local harness computes per-type metrics
- **WHEN** локальный harness запускается на локальном synthetic corpus
- **THEN** он дополнительно считает per-type precision/recall/F1

### Requirement: Go rules and validators
Go-сервис SHALL создавать rule/validator candidates для email, phone, passport, division code, dates, postal code, INN, bank card, CVV, PIN и address components с обязательными checksum/context validations.

#### Scenario: INN person checksum accepted
- **WHEN** текст содержит валидный синтетический 12-значный ИНН физлица с корректной контрольной суммой
- **THEN** detection возвращает `INN_PERSON`

#### Scenario: Organization INN rejected as person INN
- **WHEN** текст содержит 10-значный ИНН организации или организационный контекст
- **THEN** detection не возвращает `INN_PERSON` для этого значения

#### Scenario: Bank card Luhn validation
- **WHEN** текст содержит валидный синтетический номер карты, проходящий Luhn
- **THEN** detection возвращает `BANK_CARD_NUMBER`

#### Scenario: Invalid bank card rejected
- **WHEN** текст содержит номер карты, не проходящий Luhn
- **THEN** detection не возвращает `BANK_CARD_NUMBER`

#### Scenario: CVV requires local context
- **WHEN** текст содержит `CVV: 123` или `CVC: 123`
- **THEN** detection возвращает `CARD_CVV`

#### Scenario: Plain three digits are not CVV
- **WHEN** текст содержит `кабинет 123`
- **THEN** detection не возвращает `CARD_CVV`

#### Scenario: PIN requires local context
- **WHEN** текст содержит `ПИН-код: 4567` или `PIN: 4567`
- **THEN** detection возвращает `CARD_PIN`

#### Scenario: Plain four digits are not PIN
- **WHEN** текст содержит `код подтверждения 4567`
- **THEN** detection не возвращает `CARD_PIN`

### Requirement: Contextual classification
Система SHALL переклассифицировать общий `DATE` и `LOCATION` только при достаточном локальном контексте и MUST не повышать ambiguous date/location до ПДн-типа автоматически.

#### Scenario: Birth date differs from issue date and generic date
- **WHEN** текст содержит `дата рождения 01.02.1990`, `паспорт выдан 03.04.2020` и обычную дату встречи
- **THEN** detection возвращает `BIRTH_DATE` для даты рождения, `PASSPORT_ISSUE_DATE` для даты выдачи и не классифицирует обычную дату как ПДн-дату

#### Scenario: Birth place and address contexts
- **WHEN** текст содержит `место рождения город Тестовск` и `адрес регистрации: индекс, город, улица, дом, квартира`
- **THEN** detection возвращает `BIRTH_PLACE`, `ADDRESS` и доступные address components

### Requirement: Deterministic candidate merge
Go merger SHALL объединять exact duplicates, сохранять sources `rubert`, `gliner`, `regex`, `validator`, детерминированно разрешать overlaps, отдавать приоритет проверенному rule-кандидату над широким NER-кандидатом и не создавать nested или repeated tokens.

#### Scenario: Passport block parsed into components
- **WHEN** текст содержит паспортный блок с номером, органом выдачи, датой выдачи и кодом подразделения
- **THEN** merged result содержит `PASSPORT_NUMBER`, `PASSPORT_ISSUER`, `PASSPORT_ISSUE_DATE`, `PASSPORT_DIVISION_CODE` с сохраненными sources

#### Scenario: Address parsed into components
- **WHEN** текст содержит адрес физлица со страной, индексом, регионом, городом, улицей, домом, корпусом и квартирой
- **THEN** merged result содержит `ADDRESS` и component metadata для найденных частей

#### Scenario: Rule candidate wins overlap
- **WHEN** NER возвращает широкий span, пересекающийся с валидированным номером паспорта
- **THEN** merger сохраняет валидированный `PASSPORT_NUMBER` как отдельный результат с приоритетом rule/validator source
