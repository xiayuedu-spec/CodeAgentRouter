# CodeAgentRouter

An API relay station (中转站) built from the design document at
`docs/superpowers/specs/2026-08-04-relay-station-design.md`. It exposes an
OpenAI-compatible proxy over a shared pool of upstream keys with per-user
quota control, a working-hour hourly limit, a sliding-window rate limiter,
request logging and monthly reports.

## Quick start

```bash
go build -o relay.exe ./cmd/relay
RELAY_ENCRYPT_KEY=... RELAY_ADMIN_PASSWORD=... ./relay.exe -config config.yaml
```

If Go is not on PATH, the local toolchain at `.tools/go/bin` can be used
instead. State is persisted to `data/state.json` (upstream keys encrypted with
AES-GCM) and request logs go to `logs/requests-YYYY-MM-DD.jsonl`.

Open http://localhost:8080/ for the web console. Login as `admin` with the
configured password to create users, issue relay access keys and register
upstream provider keys.

## Proxy API

- `POST /v1/chat/completions`
- `POST /v1/completions`
- `POST /v1/embeddings`
- `GET /v1/models`

Clients authenticate with `Authorization: Bearer <relay access key>`. Stream
requests get `stream_options.include_usage` injected automatically so the
relay can settle quota with real usage; providers that reject the field are
retried without it and fall back to token estimation.

## Admin API

`POST /admin/login` returns a session token. Other `/admin/*` endpoints manage
users, quotas, relay access keys, upstream keys, usage and monthly reports
(JSON or `?format=csv`). Users can also log in at `POST /user/login` to manage
their own upstream keys and view their own usage and reports.

## Configuration

See `config.yaml`. The encryption key and admin password can be supplied via
environment variables (`RELAY_ENCRYPT_KEY`, `RELAY_ADMIN_PASSWORD`) instead of
the config file fallbacks.
