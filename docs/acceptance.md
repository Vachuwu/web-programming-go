# Финальная приёмка

Проект покрывает каждый функциональный пункт приёмочного чеклиста интеграционными
проверками в `integration/playlist_integration_test.go`: создание и очередь, порядок и
голоса, 100 конкурентных голосов, `play`/`next`, конфликтующие операции, удаление и
история после restart, SSE. Логи, таймауты, request ID и graceful shutdown реализованы
в `cmd/server` и `internal/middleware`.

Перед защитой выполните в окружении с Go 1.22+ и запущенным Docker daemon:

```powershell
docker compose up --build -d
docker compose ps
docker compose --profile tools run --rm test
docker compose --profile tools run --rm test -race ./...
```

Затем повторите restart-последовательность из README и проведите нагрузочный прогон:

```powershell
go run ./cmd/loadtest -mode=prepare -users=100 -items=1000 -votes=10000
go run ./cmd/loadtest -mode=run -scenario=vote -workers=100 -requests=10000
```

Фактические значения RPS и latency зависят от окружения и не должны подменяться
выдуманными результатами. Их следует перенести в таблицу README вместе с параметрами
машины и пула БД.
