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
- Accepts runner data on `:4318` only when `WhoIs` says the peer is `tag:gauger-ci` and the bearer token is a GitHub Actions OIDC token with `aud` `gauger-server`, an unexpired `exp` and the configured `repository_owner_id`. Each request is checked on its own, so gauger can switch tokens mid-job.
- Stores a job as `pending` on `start` or its first batch, then polls `GET /actions/jobs/{id}` with backoff until it completes and records the step timings. If the job completes without `done`, it looks for the `gauger-<check_run_id>` artifact until it appears or 7 days pass. When a run completes, it lists the run's artifacts once and reads the `gauger-<check_run_id>` artifact of every job without `done`, including jobs whose gauger never reached the server.
- Joins both sources on `run_id`, `run_attempt` and `check_run_id`. Without `check_run_id`, it matches `runner.name` to the job that runner was running.
- Keeps samples in daily partitions and drops a whole partition once it falls outside the retention window.

## Web UI

- **Recent jobs** link to a job view with run, job and step timings from GitHub, a CPU and memory chart from gauger, and runner usage per step.
- **Slow steps.** p50 and p95 duration per step name.
- **Regressions.** Each day's median step duration per branch against the median of the 14 days before it.
- **Right-sizing.** Peak memory against `MemTotal` and CPU against `nproc` per step. These are runner-level values during the step's time window, not the step's own usage.
- **Spend.** Job minutes, rounded up per job, times the rate for the runner label. Standard runners in public repositories and self-hosted runners are free. Rates default to GitHub's published prices; add larger runners with `GAUGER_RUNNER_RATES`.

## Contract with gauger

This contract is shared with [gauger](https://github.com/mach4-braai/gauger). Change it in both repos together.

- Every request carries `Authorization: Bearer <GitHub OIDC JWT>` with audience `gauger-server`.
- `POST /v1/jobs/start` and `POST /v1/jobs/done` take a JSON object whose keys are the identity attributes below. Values may be strings or numbers. An optional `time` (RFC 3339) says when the event happened; it defaults to when the request arrives. The reply is `{"job_id": <id>}`.
- `POST /v1/metrics` takes OTLP/HTTP metrics as protobuf or JSON, optionally gzip-compressed. Gauge and sum points are stored. Repeated points (same job, metric, attributes and time) are ignored, so replaying a buffer is safe.
- Identity attributes go on the resource or on each data point: `github.run_id`, `github.run_attempt`, `github.check_run_id`, `github.repository`, `github.workflow`, `github.job` and `runner.name`. `github.check_run_id` may be empty when `runner.name` is set.
- A `503` with `Retry-After` means the server does not know the job yet or GitHub is rate limiting it. Keep the data buffered and retry.
- The fallback artifact `gauger-<check_run_id>` holds one file per unsent batch, each an `ExportMetricsServiceRequest` in protobuf.
- The UI reads these metrics: `system.cpu.utilization` (0 to 1, whole runner), `system.cpu.logical.count` (`nproc`), `system.memory.usage` with `system.memory.state=used` (bytes), and `system.memory.limit` (`MemTotal` in bytes). Other metrics are stored as sent.

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
| `GAUGER_RUNNER_TAG` | `tag:gauger-ci` | Tailscale tag runners must carry. |
| `GAUGER_OIDC_AUDIENCE` | `gauger-server` | Required `aud` on runner tokens. |
| `GAUGER_OIDC_REPOSITORY_OWNER_ID` | `287937105` | Required `repository_owner_id`. Set it to your org's ID. |
| `GAUGER_RUNNER_RATES` | | Extra runner labels and USD per minute, such as `linux-8-core=0.022,gpu=0.052`. |

## Development

```sh
docker run -d --name gauger-pg -e POSTGRES_USER=test -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:17-alpine
GAUGER_TEST_DATABASE_URL='postgres://test:test@127.0.0.1:55432/postgres?sslmode=disable' mise run check
```

Tests that need Postgres skip when `GAUGER_TEST_DATABASE_URL` is unset.
