# CodeAgentRouter Agent Handoff

This file helps a new agent continue development on the API relay station
(中转站). The authoritative product design is
`docs/superpowers/specs/2026-08-04-relay-station-design.md`; read it before
making behavioral changes.

## What this is

A single-process Go relay that exposes an OpenAI-compatible proxy over a pool
of upstream keys. It enforces per-user quotas (hourly during working hours,
daily always), a per-minute sliding window rate limit, routes overloaded users
to a shared pool of other users' keys, logs every request to JSONL, and
aggregates monthly reports. Admin and user self-service are available through
an embedded web console.

## Local toolchain

- Go 1.26.5 is installed under `.tools/go` (gitignored). System Go is not
  installed on this machine.
- The default Go build cache (`%LOCALAPPDATA%\go-build`) is not writable in
  the sandbox. Always set `GOCACHE` to a writable location, for example:
  `$env:GOCACHE = "D:\ai\codeAgent\.tools\gocache"`.
- No third-party Go modules are used; the module is stdlib-only and builds
  offline. Keep it that way unless there is a strong reason.

## Repository layout

```text
cmd/relay/main.go            Entry point: flags, config, wiring, graceful stop
internal/config/             config.yaml parsing (small custom YAML subset)
internal/model/              Domain structs (User, AccessKey, UpstreamKey, counters)
internal/store/state.go      In-memory state, AES-GCM key encryption, debounced persist
internal/auth/               Relay access key check + web session login (PBKDF2)
internal/ratelimit/          Per-user sliding window limiter
internal/quota/              Working-hour rule + reserve/settle helpers
internal/router/             Key selection: own key first, then shared pool
internal/upstream/           OpenAI-protocol HTTP client (stream and JSON)
internal/report/             JSONL request logger + monthly report aggregation
internal/api/                HTTP layer: server.go, relay.go, admin.go, user.go
web/                         Embedded SPA (index.html, app.js, style.css)
config.yaml                  Runtime configuration
agent.md                     This file
```

## Core request flow

`POST /v1/*` -> middleware resolves the relay bearer key to a user -> handler
parses body and estimates tokens -> rate limiter -> user quota pre-reserve
(user hour/day counters) -> router picks a key (own, then pool; least used,
tie-break by in-flight) -> key counter pre-reserve -> upstream call with one
retry on 429/5xx/network failure using a different key -> response forwarded
(SSE passthrough or JSON) -> settle counters with real `usage` (fallback to
estimate) -> one JSONL log line.

Important details:

- Stream requests get `stream_options.include_usage` injected automatically.
  If upstream rejects the field with 400, the request is retried without it.
- On failure the reservation is refunded; on success the counter is adjusted
  by `actual - estimate`.
- The retry loop excludes failed key IDs so the second attempt uses another
  key. Retrying mid-stream is not possible; only pre-response failures retry.
- All counter mutations go through `store` under one global mutex. Upstream
  calls happen outside the lock, so concurrent requests stay correct without
  holding locks during I/O.

## Persistence and security

- `data/state.json` holds users, relay keys, encrypted upstream keys, and
  counters. Writes are debounced ~2s; `SIGINT`/`SIGTERM` flush immediately.
- Web console sessions are persisted in `state.json` too, so a server restart
  does not log browsers out. Tokens still expire after 24h and are removed on
  use once expired.
- Upstream API keys are AES-GCM encrypted with a key derived from
  `RELAY_ENCRYPT_KEY` (fallback: `security.encrypt_key` in config).
- Admin password comes from `RELAY_ADMIN_PASSWORD` (fallback:
  `security.admin_password`). User passwords use PBKDF2-HMAC-SHA256.
- `logs/requests-YYYY-MM-DD.jsonl` is one JSON line per request; monthly
  reports aggregate these files and cache per month until a new line is
  written.

## API surface

- Proxy: `POST /v1/chat/completions`, `POST /v1/completions`,
  `POST /v1/embeddings`, `GET /v1/models` (aggregates enabled upstream keys,
  cached 60s).
- Admin: login, users CRUD, quota, enable/disable, password reset, relay
  access keys, upstream keys, key pool status, usage, monthly report
  (`?format=csv` supported).
- User self-service: login, own upstream keys, own usage and report.

## Web console

The SPA in `web/` is vanilla JS embedded with `go:embed`; there is no npm
build step. Keep the UI self-contained and avoid adding runtime external
assets. The frontend calls the JSON APIs directly with `Authorization:
Bearer <session token>`.

## Build, test, run

```powershell
$env:GOCACHE = "D:\ai\codeAgent\.tools\gocache"
.\.tools\go\bin\gofmt.exe -w cmd internal web
.\.tools\go\bin\go.exe vet ./...
.\.tools\go\bin\go.exe test ./...
.\.tools\go\bin\go.exe build -o relay.exe ./cmd/relay

$env:RELAY_ENCRYPT_KEY = "some-long-secret"
$env:RELAY_ADMIN_PASSWORD = "admin-password"
.\relay.exe -config config.yaml
```

Open `http://localhost:8080/`; default dev admin is `admin/admin123`.

## Testing notes

- Unit tests: config YAML, token estimation, rate limiter, store counters,
  router selection, report aggregation.
- Integration tests in `internal/api/relay_test.go` run a fake upstream via
  `httptest` and cover non-stream usage settlement, SSE usage, rate-limit
  429, daily quota 429, retry to a pool key, models aggregation, admin/user
  login, and JSONL logging.
- When adding features, extend the fake upstream and integration tests; they
  are the closest thing to an E2E harness.
- Local sandbox cannot bind sockets without escalation. For live E2E, start
  the relay and a fake upstream with escalated permissions, then stop them
  afterwards.

## Known deviations from the design doc

- `tiktoken-go` was replaced with a pure-Go estimator in
  `internal/tokenize` (CJK ~1 token/char, ASCII ~4 chars/token, completion
  capped at 16384). Upstream `usage` always wins when present.
- The web console is a vanilla JS SPA instead of Vue, embedded the same way
  (`go:embed`). This avoids a frontend build chain in an offline environment.
- Counter persistence keeps window values in `state.json`; after restart the
  hour/day windows lazily reset, which the design doc accepts.

## Conventions and gotchas

- Keep the module stdlib-only and offline-buildable.
- Default to ASCII in source files; Chinese appears in UI strings and docs
  only where the file already uses it.
- `store.ReplaceUpstreamKey` preserves `APIKeyEnc` only if the caller's copy
  still contains the old ciphertext; to rotate a key, set the new encrypted
  value before calling. This is covered by the admin/user update paths.
- `report.Logger` keeps one open daily file; call `Close()` before tests
  remove temp dirs on Windows.
- `internal/config` implements a small YAML subset (nested maps, block and
  inline lists, quoted scalars). Extend the parser carefully if config grows.
- The branch prefix for new work is `codex/`.

## Possible next steps (from design doc section 15)

- Migrate counters/logs to Redis/Postgres when scale demands it.
- Live usage push (SSE/WebSocket) to the admin console.
- Model-level or time-slot-level quota and cost alerts.
- Multi-provider proxy beyond OpenAI protocol.
