## MODIFIED Requirements

### Requirement: Overload and backpressure

При работе с полным NER сервис SHALL ограничивать совокупную конкуренцию всех дорогих запросов к одному Python worker, включая планирование окон и inference. Ожидание SHALL быть ограничено общим deadline запроса, числом ожидающих и объёмом удерживаемого ввода. При превышении допустимой очереди сервис SHALL вернуть `429` с `Retry-After` и без `result`, до создания нового `payload_id` claim. Насыщение worker из-за собственного Go клиента MUST NOT давать `503`; действительная недоступность worker по-прежнему SHALL fail closed.

#### Scenario: Plan and infer share capacity
- **WHEN** параллельные запросы требуют `/plan_windows` и `/infer` сверх допустимой конкуренции worker
- **THEN** Go клиент ожидает в ограниченной очереди, а число одновременных worker POST не превышает настроенный предел

#### Scenario: Queue is full
- **WHEN** ограничение очереди по числу или объёму достигнуто до первого claim
- **THEN** сервис отвечает `429` с `Retry-After`, без `result`, и тот же `payload_id` может быть использован в позднем повторе

#### Scenario: Request deadline or cancellation
- **WHEN** ожидающий запрос отменён или его общий deadline истёк
- **THEN** он удаляется из очереди, не запускает новую worker операцию и не удерживает permit

### Requirement: Retry after transient model failure

Отказ worker, его перегрузка и отмена SHALL завершать текущую попытку безопасно, но MUST NOT навсегда закреплять отказ за `payload_id`. После восстановления поздний повтор идентичного исходного payload SHALL иметь возможность атомарно повторить маскирование. При этом concurrent одинаковые запросы SHALL разделять одного writer, отличный payload с тем же ID SHALL получать безопасный конфликт, а ранее готовый result SHALL оставаться неизменным.

#### Scenario: Worker recovers
- **WHEN** первая попытка для нового ID завершилась `503` без выданного result и worker снова доступен
- **THEN** поздний повтор того же исходного payload получает корректную маску, не используя plaintext fallback

#### Scenario: Conflicting retry
- **WHEN** после временной ошибки приходит другой payload с тем же ID
- **THEN** он не захватывает claim исходного payload и получает безопасный `409`

#### Scenario: Published result remains stable
- **WHEN** маска уже была выдана или сохранена как готовая запись
- **THEN** повтор исходного payload возвращает прежнюю маску, а восстановление прежней маски не выполняет NER

### Requirement: Safe reuse of completed NER results

Для точного повторения одного текста в пределах процесса сервис MAY повторно использовать только проверенные spans/labels/confidence/model source при той же идентичности загруженных моделей и режима. Ключ кэша SHALL быть секретным индексом без хранения исходного текста, а память и срок хранения SHALL быть ограничены. Tokenization, vault mappings, `payload_id` record и конечный `result` SHALL рассчитываться отдельно для каждого запроса; ошибка или частичный worker ответ MUST NOT кэшироваться.

#### Scenario: Identical text under different IDs
- **WHEN** два новых `payload_id` содержат идентичный текст и модельная конфигурация та же
- **THEN** проверенные NER spans могут быть использованы повторно, но каждый ID проходит собственную policy, tokenization и record state machine

#### Scenario: Distinct or failed inference
- **WHEN** текст или модельная конфигурация отличаются либо прежний worker вызов завершился ошибкой
- **THEN** результат предыдущего текста/ошибки не используется
