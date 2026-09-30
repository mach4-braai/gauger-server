# gauger-server

Self-hosted server and web UI that joins GitHub Actions webhook timings with [gauger](https://github.com/mach4-braai/gauger) runner metrics in Postgres.

## How it works

- Joins your tailnet with `tsnet` and opens three listeners, each with its own HTTP server:

  | Port | Reach | Serves |
  |---|---|---|
  | `:4318` | tailnet only | OTLP/HTTP and job lifecycle from `tag:gauger-ci` |
  | `:443` | tailnet only, TLS | web UI |
  | `:8443` | Funnel only | GitHub webhooks |

- Stores `workflow_run` and `workflow_job` webhooks after checking `X-Hub-Signature-256`, and ignores a repeated `X-GitHub-Delivery`. Older events never move a run or job back to an earlier status.
- Repairs missed deliveries from the REST API. Every run gets a check 10 minutes after its last unfinished event and a minute after it completes. The check stores the run attempt and all its jobs. This work lives in Postgres, so it survives a restart.
- Spends at most 5,000 REST requests an hour per installation, and waits out `x-ratelimit-reset` and `retry-after`.
- Joins both sources on `run_id`, `run_attempt` and `check_run_id`, and stores them in Postgres.
- Keeps samples in daily partitions and drops a whole partition once it falls outside the retention window.

## Self-hosting

The tailnet needs MagicDNS and HTTPS certificates turned on, and the node needs the `funnel` attribute.

```sh
GAUGER_TS_AUTHKEY=tskey-auth-... docker compose up -d --build
```

The auth key is only read on first start. tsnet keeps the node key in the `state` volume, so keep that volume across upgrades. Losing it means enrolling again with a new key.

### GitHub App

Open `https://<host>.<tailnet>.ts.net/setup` from a device on the tailnet and create the App there. GitHub creates it from a manifest with `Actions: read` and `Metadata: read`, the `workflow_run` and `workflow_job` events, and the webhook URL `https://<host>.<tailnet>.ts.net:8443/webhooks/github` with a generated secret. The server stores the App ID, private key and webhook secret in Postgres. Then install the App on the repositories you want to measure.

To bring your own App instead, set all three `GAUGER_GITHUB_*` App variables below.

| Variable | Default | |
|---|---|---|
| `GAUGER_DATABASE_URL` | | Postgres URL. Required. |
| `GAUGER_TS_AUTHKEY` | | Tailscale auth key for the first start. |
| `GAUGER_TS_DIR` | `/var/lib/gauger-server/tsnet` | tsnet state. Must persist. |
| `GAUGER_TS_HOSTNAME` | `gauger-server` | Tailnet host name. |
| `GAUGER_RETENTION_DAYS` | `90` | Days of samples to keep. |
| `GAUGER_GITHUB_APP_ID` | | App ID, when not using `/setup`. |
| `GAUGER_GITHUB_APP_PRIVATE_KEY_FILE` | | Path to the App's PEM private key. |
| `GAUGER_GITHUB_WEBHOOK_SECRET` | | The App's webhook secret. |
| `GAUGER_GITHUB_API_URL` | `https://api.github.com` | REST API base. |
| `GAUGER_GITHUB_URL` | `https://github.com` | Web base for the manifest flow. |

## Development

```sh
docker run -d --name gauger-pg -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:17-alpine
GAUGER_TEST_DATABASE_URL='postgres://test:test@127.0.0.1:55432/postgres?sslmode=disable' mise run check
```

Tests that need Postgres skip when `GAUGER_TEST_DATABASE_URL` is unset.
