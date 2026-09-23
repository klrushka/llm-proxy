## Why

Сейчас метрики сервиса нельзя собрать ни в одном профиле доступа. Их пишет только `POST /process`. В профиле `checker` сам `/metrics` отвечает `403`. В профиле `production` `/process` запрещён, поэтому все метрики равны нулю. Маршруты `/v1/pii/*` и `/v1/runtime/chat`, ошибки, отказы при перегрузке, вызовы model worker и внешней LLM не замеряются. Шесть гаужей, уже усреднённых сервисом за минуту, нельзя корректно сложить между инстансами и посчитать за произвольный период.

Изменение приводит метрики к принятому в индустрии виду: метод RED (Rate, Errors, Duration — запросы, ошибки, длительность) для каждого маршрута и каждой зависимости, названия по OpenTelemetry semantic conventions, официальная библиотека Prometheus, отдельный внутренний порт, готовые дашборд и алерты.

## What Changes

- Метрики отдаются через `prometheus/client_golang` — официальную Go-библиотеку Prometheus. В вывод добавляются стандартные метрики Go runtime (`go_*`) и процесса (`process_*`).
- `/metrics` переезжает на отдельный внутренний порт `PII_METRICS_LISTEN_ADDRESS` (по умолчанию `127.0.0.1:9464`). Он не проходит consumer access и не обслуживает data-маршруты. **BREAKING:** основной API-порт больше не отдаёт `/metrics`.
- HTTP-метрики сервера по OpenTelemetry semantic conventions: `http_server_request_duration_seconds` (гистограмма) с метками `http_request_method`, `http_route` (шаблон маршрута, например `/v1/pii/scopes/{scope_id}`), `http_response_status_code`; `http_server_active_requests`; `http_server_request_body_size_bytes`. Отказы доступа (`401`/`403`) и перегрузки (`429`) видны.
- HTTP-метрики клиента для model worker и внешней LLM: `http_client_request_duration_seconds` с меткой `server` (`model_worker` / `llm`), операцией и исходом.
- Доменные метрики: `pii_entities_detected_total{type}` — найденные ПДн по типу; `pii_vault_mappings` — число живых записей в vault; `pii_build_info{version, model_mode}`.
- Старые гаужи `pii_latency_*`, `pii_rps`, `pii_tps` удаляются. **BREAKING.**
- Grafana-дашборд `deploy/grafana` строится на новых метриках; добавляются правила алертов Prometheus `deploy/prometheus/alerts.yml` по SLO (цели доступности и задержки).

## Capabilities

### New Capabilities
- Нет.

### Modified Capabilities
- `pii-api`: требование Metrics заменяется на стандартные HTTP-, runtime- и доменные метрики на отдельном внутреннем порту.

## Impact

- Новая зависимость: `github.com/prometheus/client_golang`.
- Код: `internal/metrics`, `internal/api`, `internal/modelclient`, `internal/llmclient`, `internal/vault`, `internal/config`, `cmd/pii-service`.
- Конфигурация: `PII_METRICS_LISTEN_ADDRESS`; в `docker-compose.yml` порт метрик только `expose`.
- Эксплуатация: `deploy/grafana`, `deploy/prometheus`, README, `docs/deployment-runbook.md`.
- Метки никогда не содержат значение пути, `scope_id`, `payload_id`, system ID, текст, токены или значения ПДн.
