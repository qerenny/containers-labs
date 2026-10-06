# api

Подопытный Go-сервис для лабораторной по observability.

## Запуск

```bash
go run .
```

Сервис слушает `:8080`. Адрес можно изменить через `HTTP_ADDR`. OTLP/gRPC
экспортёр использует стандартные переменные OpenTelemetry, например:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger:4317 ./api
```

Эндпоинты: `/health`, `/fail`, `/slow`, `/load?count=50`, `/metrics`.
Для `/load` внутренний адрес сервиса задаётся через `SERVICE_BASE_URL`.
