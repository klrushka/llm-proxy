## 1. Основа

- [x] 1.1 Подключить `prometheus/client_golang`: отдельный registry с `go_*`, `process_*` и `pii_build_info`; удалить старые гаужи `pii_latency_*`, `pii_rps`, `pii_tps` и оконный замер `recordProcess`; observable check: unit test вывода проходит.
- [x] 1.2 Добавить `PII_METRICS_LISTEN_ADDRESS` (по умолчанию `127.0.0.1:9464`) и поднять отдельный metrics listener; убрать `/metrics` с API listener, из consumer access и admission; observable check: wiring tests — `200` на metrics listener в обоих профилях, API listener метрики не отдаёт.

## 2. HTTP-метрики сервера

- [x] 2.1 Реализовать внешний middleware: `http_server_request_duration_seconds`, `http_server_active_requests`, `http_server_request_body_size_bytes` с метками метода, шаблона маршрута и статуса; observable check: tests для каждого маршрута, `401`/`403`/`429` и отсутствия `scope_id` в выводе проходят.

## 3. Зависимости и домен

- [x] 3.1 Инструментировать model client: `http_client_request_duration_seconds{server="model_worker"}` по операции и исходу; observable check: unit tests с тестовым сервером проходят.
- [x] 3.2 Инструментировать LLM client: `http_client_request_duration_seconds{server="llm"}` по исходу; observable check: unit tests с тестовым сервером проходят.
- [x] 3.3 Добавить `pii_entities_detected_total{type}` и `pii_vault_mappings`; observable check: unit tests — метки только из canonical registry, значения сущностей в выводе отсутствуют.

## 4. Эксплуатация

- [x] 4.1 Обновить `docker-compose.yml` (порт метрик только `expose`), README и `docs/deployment-runbook.md` (сбор, NetworkPolicy, удалённые метрики); observable check: `docker compose config` проходит.
- [x] 4.2 Перестроить Grafana-дашборд `deploy/grafana` на новых метриках: обзор RED, маршруты, зависимости, ПДн по типам, vault, Go runtime; observable check: JSON валиден, все запросы используют только метрики из 1.1–3.3.
- [x] 4.3 Добавить `deploy/prometheus/alerts.yml`: burn-rate по доступности, p95, сервис не опрашивается, model worker недоступен, устойчивые `429`, рост vault; observable check: `promtool check rules` и `promtool test rules deploy/prometheus/alerts_test.yml` проходят.
- [ ] 4.4 Прогнать load bench с метриками; observable check: RPS и p95 не хуже прогона из `docs/load-benchmark-verified-run.md` более чем на 5 процентов.
