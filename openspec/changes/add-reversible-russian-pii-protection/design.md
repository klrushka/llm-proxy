## Context

Репозиторий зеленый: рабочей реализации пока нет, есть только brief и OpenSpec каркас. Проект должен сохранить жесткую границу: Go принимает решения, хранит и восстанавливает mappings, а Python worker только возвращает модельные spans.

## Goals / Non-Goals

**Goals:**
- Спроектировать единый pipeline `detect -> merge -> context classification -> ownership -> tokenize` на Go.
- Ограничить Python worker ролью адаптера готовых NER моделей.
- Обеспечить обратимость через `scope_id`, encrypted vault и detokenize без повторного NER.
- Сформировать API и audit logging так, чтобы plaintext ПДн и секреты не попадали в ответы и логи.

**Non-Goals:**
- Fine-tuning моделей.
- Хранение plaintext mappings в persistent storage.
- Расширение набора типов сверх перечисленного в brief.
- Поддержка произвольных языков кроме русскоязычного текста в рамках этой change.

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

## Risks / Trade-offs

- Модели могут вернуть некорректные offsets -> Go model client должен валидировать boundaries и отбрасывать invalid spans.
- Regex для CVV/PIN может давать false positives -> validators обязаны требовать локальный контекст `CVV`, `CVC`, `код безопасности`, `PIN`, `ПИН` или `пин-код`.
- Merge nested spans может потерять компоненты ФИО или адреса -> внешний span токенизируется один раз, а компоненты сохраняются в metadata.
- In-memory vault не переживает restart -> README должен маркировать его как demo adapter; persistent adapter хранит только ciphertext.
- Логи легко загрязнить plaintext на error path -> добавить тест-перехватчик логов и запретить логирование request/response body.

## Migration Plan

Проект создается с нуля. Rollback для implementation phase: откатить задачу-коммит, соответствующий одному пункту `tasks.md`.
