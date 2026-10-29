# Playlist

## Local PostgreSQL and migrations

1. Copy `.env.example` to `.env` and adjust credentials if needed.
2. Start PostgreSQL: `docker compose up -d postgres`.
3. Apply migrations: `docker compose --profile tools run --rm migrate`.
4. To roll back the initial migration on an empty database, run:
   `docker compose --profile tools run --rm migrate -path /migrations -database "postgres://playlist:playlist@postgres:5432/playlist?sslmode=disable" down 1`.

The `postgres_data` named volume keeps database data when the container is restarted.
