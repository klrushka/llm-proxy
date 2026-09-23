## Context

См. мотивацию в `proposal.md`. Текущий запуск оборачивает router middleware-ом consumer access, который выбирает checker/production profile, извлекает Bearer key и подменяет policy в context. Pipeline также namespace-ит scope по consumer ID. Config, Docker и документация требуют профиль и consumers для production.

## Goals / Non-Goals

**Goals:**
- Убрать входящий access-control путь и связанные с ним configuration surface, middleware, consumer lookup и тесты.
- Сохранить единый полный набор типов и детокенизацию для каждого запроса.
- Вернуть прямую семантику `scope_id` для всех extended API операций.
- Не затрагивать исходящую авторизацию внешней LLM.

**Non-Goals:**
- Не менять алгоритмы обнаружения, ownership, токенизации, vault или аудит.
- Не добавлять новый механизм аутентификации, rate limiting или сетевой perimeter.
- Не менять контракт и валидацию `PII_LLM_API_KEY`.

## Decisions

### Удалить access middleware вместо открытия checker profile

Router будет получать только audit и admission middleware. Оставить checker profile и открыть в нём все маршруты отклонили: это сохраняет ненужные profile/env branches и позволяет ошибочно вернуть access gate в будущем.

### Сохранить статическую внутреннюю policy как processing dependency

Pipeline продолжит получать одну полную internal policy, необходимую ownership stage для фильтрации канонических типов, но она не будет содержать consumer identity и не будет устанавливаться из HTTP context. Полная переработка ownership API не нужна для удаления входящей авторизации и увеличила бы риск для классификации ПДн.

### Использовать исходный scope_id без consumer namespace

Удаление identity делает namespaced scope неосмысленным. Pipeline удалит context-dependent scope derivation и будет передавать caller `scope_id` напрямую в vault и token issuer. Альтернатива с постоянным namespace сохраняет непрозрачное отличие публичной семантики от переданного идентификатора без выгоды.

### Удалить access configuration целиком

`Config` перестанет содержать access fields; parser consumers, validation, profile constants и Compose variables удаляются. `PII_LLM_API_KEY` остаётся в LLM configuration, поскольку используется только HTTP client-ом к downstream LLM.

## Risks / Trade-offs

- [Все клиенты, знающие scope_id, могут восстановить mappings] → это сознательное следствие публичного API; развёртывание должно ограничивать доступ на сетевом уровне при необходимости.
- [Коллизия одинакового scope_id у разных внешних клиентов] → callers должны выбирать уникальные scope_id; прежняя изоляция consumer identity больше не применяется.
- [Незавершённый OpenSpec change содержит старые consumer-policy требования] → текущий change явно фиксирует breaking replacement и при реализации должны быть обновлены затронутые тесты и документы.

## Migration Plan

1. Удалить access configuration и middleware вместе с зависимым code path.
2. Перевести pipeline на статическую полную policy и прямой scope.
3. Обновить тесты, Compose и документацию; запустить полный Go test suite и Docker smoke без Authorization.
4. Rollback: вернуть отдельный commit задачи, если публичный режим нельзя применять в целевом окружении.
