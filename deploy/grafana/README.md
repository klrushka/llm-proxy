# Grafana dashboard

`pii-service-dashboard.json` — дашборд pii-service по методу RED (запросы, ошибки,
длительность). Импорт: Grafana → Dashboards → New → Import, выбрать источник
Prometheus.

Разделы:

- обзор: инстансы, RPS, доля 5xx, p95, отказы 429, ошибки model worker;
- трафик и ошибки по маршрутам, отказы доступа (401/403) и перегрузка (429);
- задержка: p50/p95/p99, p95 по маршруту, тепловая карта, запросы в обработке;
- зависимости: model worker и LLM — вызовы по коду и задержка;
- персональные данные: найденные ПДн по типу, TPS, записи в vault;
- Go runtime и процесс: CPU, память, горутины, паузы GC.

Метрики собираются с внутреннего порта `9464` (см. `docs/deployment-runbook.md`,
раздел Monitoring). Правила алертов — `deploy/prometheus/alerts.yml`.
