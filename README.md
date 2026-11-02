# The common room — Go

A single-file Go port of [demo-php-psql-redis](https://github.com/InfrastryDemo/demo-php-psql-redis/tree/75bc924d6ad8c0c8c05ae36786836ceaae9e09ec). All application code, HTML, CSS, JavaScript, and schema setup live in `main.go`. Uses Go's standard HTTP server plus PostgreSQL and Redis drivers. No framework or frontend build.

## Run

```sh
docker compose up --build -d --wait
```

Open **http://localhost:8090**. The viewer count includes you, so it starts at 1 when you're alone. Open a private window to see the count increase. Tabs in one browser share an identity.

The messages table is created automatically. PostgreSQL data survives container restarts and `docker compose down` in a named volume. Copy `.env.example` to `.env` to change the host port or initial database credentials; Compose loads it automatically. Existing database volumes keep their original credentials.

```sh
docker compose logs -f app
docker compose down
```

## Behavior

- Polls every 2 seconds for the newest 100 messages and total active browsers, **including you**.
- Redis tracks presence atomically using its clock; browsers idle for 20 seconds expire. Tabs share a session cookie and count once.
- PostgreSQL permanently stores messages with timestamps and derived anonymous visitor labels. The schema and Redis keys match the PHP demo.
- Supports Unicode and line breaks, up to 1,000 characters, with one post per visitor every 2 seconds.
- Preserves the original interface, keyboard shortcut (Ctrl/Cmd+Enter), character counter, automatic reconnection, and drafts during connection failures.
- Keeps HttpOnly/SameSite cookies, CSRF checks, JSON validation, request-size limits, parameterized SQL, text-only message rendering, and security headers. Set `COOKIE_SECURE=1` behind HTTPS.

| Endpoint | Purpose |
| --- | --- |
| `GET /` | Page and anonymous session cookie |
| `GET /api/state` | Heartbeat, total `viewers` including you, and latest messages; requires cookie |
| `POST /api/messages` | JSON `{"body":"Hello"}`; requires cookie and matching `X-CSRF-Token` |
| `GET /health` | Success only when PostgreSQL, the messages table, and Redis work |

## Without Docker

With Go 1.26+ and PostgreSQL/Redis running:

```sh
go run main.go
```

Create the database and user first, or supply existing credentials through environment variables. The app creates its own table. `.env` is for Compose; native runs read the process environment.

| Variable | Default |
| --- | --- |
| `PORT` | `8090` (Go HTTP listener) |
| `PGHOST` / `PGPORT` | `127.0.0.1` / `5432` |
| `PGDATABASE` | `common_room` |
| `PGUSER` / `PGPASSWORD` | `common_room` / `local-demo-password` |
| `PGSSLMODE` | `prefer` |
| `REDIS_URL` | `redis://127.0.0.1:6379` |
| `COOKIE_SECURE` | `0` |
| `APP_PORT` | `8090` (Compose host port) |

`REDIS_URL` accepts credentials, a database path, `rediss://` for TLS, and the original `tcp://` / `tls://` scheme aliases. Compose supplies PostgreSQL settings from `POSTGRES_DB`, `POSTGRES_USER`, and `POSTGRES_PASSWORD`, keeps data services internal, and binds the web port to loopback.

To build a standalone binary or run schema setup only:

```sh
go build .
go run main.go migrate
```

## Verify

```sh
go vet ./...
go build ./...
```

The HTTP integration check uses Python 3's standard library. Run it against a **fresh disposable stack**; it creates test messages and expects no other visitors. Do not open the test port in a browser during the run.

```sh
APP_PORT=8081 docker compose -p common-room-go-test up --build -d --wait
TEST_BASE_URL=http://127.0.0.1:8081 python3 tests/integration.py
APP_PORT=8081 docker compose -p common-room-go-test down -v
```

It checks sessions, two-browser presence, tab deduplication, idle expiry, messages across sessions, Unicode, ordering, the 100-message limit, CSRF, validation, rate limiting, and static assets. It leaves test records in the disposable database; the final command removes that project's containers and volume.
