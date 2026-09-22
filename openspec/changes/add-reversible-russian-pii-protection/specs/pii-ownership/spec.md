## Purpose

Capability определяет, какие найденные сущности являются персональными данными физлица, какие относятся к организации или публичному/обычному упоминанию, и как возвращается неоднозначность.

## ADDED Requirements

### Requirement: Ownership metadata
Для каждой найденной сущности система SHALL возвращать `personal`, `owner_type`, `owner_id`, `ownership_score`, `reason_codes` и при неоднозначности `review_recommended`.

#### Scenario: Personal client data receives shared owner
- **WHEN** текст содержит `Клиент ТЕСТОВ ТЕСТ ТЕСТОВИЧ, паспорт 00 00 000000`
- **THEN** ФИО и паспорт получают `personal=true`, `owner_type=PERSON`, общий `owner_id` и reason codes для клиентского и паспортного контекста

### Requirement: Positive personal context
Система SHALL считать положительным контекстом слова `клиент`, `заявитель`, `заёмщик`, `пользователь`, `владелец`, `гражданин`, `ФИО`, `дата рождения`, `паспорт`, `адрес регистрации`, а также совместное присутствие ФИО и паспорта, телефона, email или даты рождения.

#### Scenario: Client data is personal
- **WHEN** текст содержит данные клиента с ФИО, паспортом, телефоном или email
- **THEN** связанные сущности получают `personal=true` и один `owner_id`

### Requirement: Negative and organization context
Система MUST не считать сущность персональными данными физлица при контексте `поэт`, `писатель`, `автор`, `биография`, `стихотворение`, `ООО`, `АО`, `ПАО`, `банк`, `филиал`, `отделение`, `юридический адрес`, `ИНН организации`.

#### Scenario: Poet name is not tokenized as personal data
- **WHEN** текст содержит `Александр Пушкин — русский поэт.`
- **THEN** ФИО может быть найдено, но получает `personal=false` и не должно передаваться в tokenization как подтвержденное ПДн

#### Scenario: Bank branch address is organization data
- **WHEN** текст содержит `Отделение банка находится по адресу: ...`
- **THEN** адрес может быть найден, но получает `owner_type=ORGANIZATION`, `personal=false` и не должен токенизироваться

### Requirement: Ambiguous entities require review
Если контекст не позволяет надежно определить принадлежность физлицу, система MUST вернуть `personal=false` и `review_recommended=true`, не выполняя автоматическую токенизацию такой сущности.

#### Scenario: Ambiguous location is not personal automatically
- **WHEN** текст содержит location без признаков физлица, организации или адреса регистрации
- **THEN** location получает `personal=false`, `review_recommended=true` и не токенизируется

### Requirement: Per-system type settings
Consumer policy SHALL определять per-system настройки типов ПДн, влияющие на то, какие типы считаются персональными данными для данной системы.

#### Scenario: Per-system settings affect ownership
- **WHEN** consumer policy для системы исключает определённый тип из числа ПДн
- **THEN** сущности этого типа получают `personal=false` и не токенизируются для данной системы
