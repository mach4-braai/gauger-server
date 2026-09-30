# gauger-server

Self-hosted server and web UI that joins GitHub Actions webhook timings with [gauger](https://github.com/mach4-braai/gauger) runner metrics in Postgres.

## How it works

- Joins your tailnet with `tsnet` and opens three listeners, each with its own HTTP server:

  | Port | Reach | Serves |
  |---|---|---|
  | `:4318` | tailnet only | OTLP/HTTP and job lifecycle from `tag:gauger-ci` |
  | `:443` | tailnet only, TLS | web UI |
  | `:8443` | Funnel only | GitHub webhooks |

- Joins both sources on `run_id`, `run_attempt` and `check_run_id`, and stores them in Postgres.
- Keeps samples in daily partitions and drops a whole partition once it falls outside the retention window.

## Self-hosting

The tailnet needs MagicDNS and HTTPS certificates turned on, and the node needs the `funnel` attribute.

```sh
GAUGER_TS_AUTHKEY=tskey-auth-... docker compose up -d --build
```

The auth key is only read on first start. tsnet keeps the node key in the `state` volume, so keep that volume across upgrades. Losing it means enrolling again with a new key.

| Variable | Default | |
|---|---|---|
| `GAUGER_DATABASE_URL` | | Postgres URL. Required. |
| `GAUGER_TS_AUTHKEY` | | Tailscale auth key for the first start. |
| `GAUGER_TS_DIR` | `/var/lib/gauger-server/tsnet` | tsnet state. Must persist. |
| `GAUGER_TS_HOSTNAME` | `gauger-server` | Tailnet host name. |
| `GAUGER_RETENTION_DAYS` | `90` | Days of samples to keep. |

## Development

```sh
docker run -d --name gauger-pg -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:17-alpine
GAUGER_TEST_DATABASE_URL='postgres://test:test@127.0.0.1:55432/postgres?sslmode=disable' mise run check
```

Tests that need Postgres skip when `GAUGER_TEST_DATABASE_URL` is unset.
