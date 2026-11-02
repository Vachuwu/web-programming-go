# Shared Playlist

Учебный HTTP-сервис общей очереди воспроизведения на Go, PostgreSQL и `net/http`.
Аудио не хранится и не воспроизводится: трек содержит только метаданные и опциональный URL.

## Запуск

Для обычного запуска нужен только Docker с запущенным daemon: Go находится внутри
build-контейнера.

```powershell
docker compose up --build -d
docker compose ps
```

Swagger UI доступен на <http://localhost:8080/docs/>. Исходная спецификация —
[`docs/openapi.yaml`](docs/openapi.yaml). Остановка `Ctrl+C` прекращает новые запросы,
закрывает SSE-подписки, ожидает завершения HTTP-операций в пределах `SHUTDOWN_TIMEOUT`
и затем закрывает пул БД.

`app` стартует только после успешных миграций, а PostgreSQL хранит данные в именованном
томе `postgres_data`. Для просмотра логов: `docker compose logs -f app`; для остановки:
`docker compose down`. Не используйте `-v`, если нужно сохранить данные.

Для проверки сохранности данных остановите сервер, перезапустите PostgreSQL и снова
выполните миграции (они идемпотентны), затем запустите сервер. Том `postgres_data` не
удаляется этой последовательностью:

```powershell
docker compose restart postgres
docker compose up -d
```

## Архитектура и данные

`handler → service → repository → PostgreSQL`. Все изменения плейлиста выполняются в
транзакции с блокировкой единственной строки `playlist_state`. Это последовательно
сериализует `add`, `vote`, `remove`, `play` и `next`.

Таблицы: `users`, `playlist_items`, `votes`, `playlist_state`. `score` не хранится в
строке трека — это `SUM(votes.value)`. Очередь сортируется строго по
`score DESC, created_at ASC, id ASC`. Уникальный индекс допускает только один
`playing` трек. Статусы: `queued → playing → played`; `queued → removed`.

`playbackVersion` увеличивается только после успешного `play` или `next`. Клиент
передаёт его как `expectedVersion`; устаревший запрос получает `409 VERSION_CONFLICT`.
Повторный одинаковый голос и удаление отсутствующего голоса — успешные no-op.

## HTTP API

| Метод | Путь | Назначение |
|---|---|---|
| POST | `/users` | Создать пользователя |
| GET | `/playlist` | Получить playing, очередь и версию |
| POST | `/playlist/items` | Добавить трек |
| POST/DELETE | `/playlist/items/{id}/vote` | Поставить или убрать голос |
| DELETE | `/playlist/items/{id}` | Soft-delete queued-трека автором |
| POST | `/playlist/play` | Запустить лучший queued-трек |
| POST | `/playlist/next` | Завершить playing и запустить следующий |
| GET | `/playlist/history` | Получить played-треки |
| GET | `/playlist/events` | SSE-подписка |

Ошибки имеют форму `{"error":{"code":"VERSION_CONFLICT","message":"..."}}`.
Полный перечень схем и ответов находится в OpenAPI.

```powershell
curl.exe -X POST http://localhost:8080/users -H "Content-Type: application/json" -d '{"name":"Max"}'
curl.exe -X POST http://localhost:8080/playlist/items -H "Content-Type: application/json" -d '{"userId":1,"title":"The Pot","artist":"Tool"}'
curl.exe http://localhost:8080/playlist
curl.exe -N http://localhost:8080/playlist/events
```

## SSE

После commit отправляются `item_added`, `item_removed`, `vote_changed` и
`playback_changed`. События содержат только `itemId` или `playbackVersion`, не весь
плейлист. Доставка best-effort: при подключении и после каждого события клиент должен
прочитать `GET /playlist`. Медленный подписчик отключается, чтобы не задерживать API.

## Конфигурация

`.env` читается локально, переменные окружения имеют приоритет: `SERVER_PORT`,
`DATABASE_URL`, `LOG_LEVEL`, `DB_MAX_CONNECTIONS`, `HTTP_TIMEOUT`,
`DB_OPERATION_TIMEOUT`, `DB_CONNECT_TIMEOUT`, `SHUTDOWN_TIMEOUT`. Обычный API имеет
HTTP-лимит, операции БД используют более короткий контекст; SSE от этих таймаутов
исключён. JSON-логи не содержат тела запросов и секреты.

## Тесты

Интеграционные тесты запускают реальный PostgreSQL через Testcontainers и выполняются
в контейнере (Docker Desktop должен работать в режиме Linux containers):

```powershell
docker compose --profile tools run --rm test
docker compose --profile tools run --rm test -race ./...
```

Docker должен быть доступен текущему пользователю: контейнер тестов передаёт ему Docker
socket, чтобы Testcontainers смог поднять временный PostgreSQL.

Перед демонстрацией также проверьте полный цикл: создание пользователей и треков,
изменение голосов, конкурентные `next`, историю, SSE, restart по инструкции выше и
Swagger UI. Это соответствует приёмочному чеклисту из
[`docs/playlist_go_technical_spec.md`](docs/playlist_go_technical_spec.md).

## Нагрузочный инструмент

`cmd/loadtest` готовит данные исключительно через REST API, сохраняет идентификаторы в
`loadtest-state.json`, а затем отдельно замеряет нагрузку фиксированным Worker Pool.
Подготовка не попадает в метрики. Нагрузчик тоже запускается в контейнере и хранит своё
временное состояние в отдельном именованном Docker volume.

```powershell
go run ./cmd/loadtest -mode=prepare -users=100 -items=1000 -votes=10000
go run ./cmd/loadtest -mode=run -scenario=vote -workers=100 -requests=10000
```

Контейнерный вариант без Go на хосте:

```powershell
docker compose --profile tools run --rm loadtest -mode=prepare -users=100 -items=1000 -votes=10000
docker compose --profile tools run --rm loadtest -mode=run -scenario=vote -workers=100 -requests=10000
```

Сценарии: `get`, `vote`, `next`, `mixed`. Режим `next` запускает запросы с одной
версией плейбека: ответы `409` выводятся отдельно как ожидаемая защита от конкурентного
продвижения. Итог содержит количество ответов по HTTP-статусам, успешные и технические
ошибки, RPS, среднее время и p50/p95/p99. Для hotspot-профиля подготовьте один трек и
1000 пользователей: `-users=1000 -items=1 -votes=0`.

Результаты зависят от машины, PostgreSQL и настроек пула. Общая блокировка
`playlist_state` защищает инварианты, но намеренно сериализует изменяющие операции;
сравнивать прогоны следует при одинаковом окружении, workers и количестве соединений.

### Таблица результатов

После каждого измерения перенесите строку из вывода CLI в таблицу. Подготовительная
фаза в неё не включается.

| Окружение / пул БД | Сценарий | Workers | Запросов | RPS | avg / p50 / p95 / p99 | Статусы и ошибки | Вывод |
|---|---:|---:|---:|---:|---|---|---|
| Заполнить после прогона | vote | 100 | 10000 | — | — | — | — |
