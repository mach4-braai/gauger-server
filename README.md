# gauger-server

Self-hosted server and web UI that joins GitHub Actions webhook timings with [gauger](https://github.com/mach4-braai/gauger) runner metrics in Postgres.

## How it works

- Joins your tailnet with `tsnet` and opens four listeners, each with its own HTTP server:

  | Port | Reach | Serves |
  |---|---|---|
  | `:10000` | Funnel only | OTLP/HTTP and job lifecycle from GitHub Actions runners |
  | `:4318` | tailnet only | OTLP/HTTP and job lifecycle from `tag:gauger-ci`, for gauger releases that still join the tailnet |
  | `:443` | tailnet only, TLS | web UI |
  | `:8443` | Funnel only | GitHub webhooks |

- Stores `workflow_run` and `workflow_job` webhooks after checking `X-Hub-Signature-256`, and ignores a repeated `X-GitHub-Delivery`. Older events never move a run or job back to an earlier status.
- Repairs missed deliveries from the REST API. Every run gets a check 10 minutes after its last unfinished event and a minute after it completes. The check stores the run attempt and all its jobs. This work lives in Postgres, so it survives a restart.
- Spends at most 5,000 REST requests an hour per installation, and waits out `x-ratelimit-reset` and `retry-after`.
- Accepts runner data when the bearer token is a GitHub Actions OIDC token with `aud` `gauger-server`, an unexpired `exp` and the configured `repository_owner_id`. On `:10000` the token is the only credential. On `:4318`, `WhoIs` must also say the peer is `tag:gauger-ci`. Each request is checked on its own, so gauger can switch tokens mid-job. The server logs the public runner URL, `https://<host>.<tailnet>.ts.net:10000`, at startup.
- Binds each runner request to the job its token was issued to. Every identity in the body must match the token's `repository`, `run_id`, `run_attempt` and `check_run_id` claims, or the request gets `403` and nothing is stored.
- Rate limits runner data with `429` and `Retry-After`. Each job, by its token's `check_run_id`, gets a request a second with a burst of 120, enough to replay 10 minutes of buffered batches. Each client address, or IPv6 /64, gets 20 failed authentications, then one every 6 seconds; past that it gets `429` before its token is checked.
- Backfills history from before the App was installed. A backfill task per repository lists completed runs with `GET /actions/runs?created=...` and queues a `run` task for each one's latest attempt; GitHub returns only the latest attempt, so earlier attempts of a re-run are not backfilled. `/setup` queues a backfill of the last N days (90 by default) for every repository with an installation. Running the same backfill twice adds no rows: runs, jobs and steps upsert on their IDs, and tasks upsert on `(kind, key)`.
- Stores a job as `pending` on `start` or its first batch, then polls `GET /actions/jobs/{id}` with backoff until it completes and records the step timings. If the job completes without `done`, it looks for the `gauger-<check_run_id>` artifact until it appears or 7 days pass. When a run completes, it starts the same search for every completed job of the run without `done` or an ingested artifact, including jobs whose gauger never reached the server.
- Joins both sources on `run_id`, `run_attempt` and `check_run_id`. Without `check_run_id` in the body, it uses the token's `check_run_id` claim.
- Keeps samples in daily partitions and drops a whole partition once it falls outside the retention window.

## Web UI

- **Dashboard** at `/stats/`, filtered by range (24h, 7d, 30d, 90d or all), repository and event. The Overview has run, job and step counts, job minutes and estimated spend, run and job success rates, run duration and queue time p50 and p95, and gauger coverage, above a chart of runs per hour or day stacked by conclusion. Filter changes and legend toggles update the page in place and the URL with it; with JavaScript off the filters reload the page. Times are UTC. See [ADR 0002](docs/adr/0002-datastar-dashboard.md).
- **Recent jobs**, filtered by repository and days like the other reports, link to a job view with run, job and step timings from GitHub, a CPU and memory chart from gauger, and runner usage per step.
- **Slow steps.** p50 and p95 duration per step name.
- **Regressions.** Each day's median step duration per branch against the median of the 14 days before it.
- **Daily timing.** Median duration per bucket, day, week or month, for each workflow and each job, one column per bucket, with a bar chart above the tables. Filters narrow it to one workflow (one bar per job) or one job (one bar per bucket); changing the repository clears a workflow or job that does not exist in it. At most 60 buckets; weeks start Monday UTC. A workflow run lasts from its first job's start to its last job's end.
- **Right-sizing.** Peak memory against `MemTotal` and CPU against `nproc` per step. These are runner-level values during the step's time window, not the step's own usage.
- **Spend.** Job minutes, rounded up per job, times the rate for the runner label. Standard runners in public repositories and self-hosted runners are free. Rates default to GitHub's published prices; add larger runners with `GAUGER_RUNNER_RATES`.

## Contract with gauger

This contract is shared with [gauger](https://github.com/mach4-braai/gauger). Change it in both repos together.

- Send to `https://<host>.<tailnet>.ts.net:10000` over public HTTPS. Every request carries `Authorization: Bearer <GitHub OIDC JWT>` with audience `gauger-server`, and the token is the only credential.
- `POST /v1/jobs/start` and `POST /v1/jobs/done` take a JSON object whose keys are the identity attributes below. Values may be strings or numbers. The reply is `{"job_id": <id>}`.
- `POST /v1/metrics` takes OTLP/HTTP metrics as protobuf or JSON, optionally gzip-compressed. Gauge and sum points are stored. Repeated points (same job, metric, attributes and time) are ignored, so replaying a buffer is safe.
- Identity attributes go on the resource or on each data point: `github.run_id`, `github.run_attempt`, `github.check_run_id`, `github.repository`, `github.workflow`, `github.job` and `runner.name`. `github.check_run_id` may be empty; the server then uses the token's claim.
- The identity must name the job the token was issued to. A `403` means it named another one. Keep the data buffered and retry, so it ends up in the fallback artifact.
- A `429` with `Retry-After` means the job or address is over its rate. Keep the data buffered and retry.
- The fallback artifact `gauger-<check_run_id>` holds one file per unsent batch, each an `ExportMetricsServiceRequest` in protobuf.
- The reports read these metric shapes. A client that sends another shape gets wrong numbers, so changing one is a contract change.
  - Every point of one sample has the same timestamp. Sample counts are distinct timestamps.
  - `system.cpu.utilization`: one point per sample with no attributes, the busy share of all CPUs from 0 to 1. Peak CPU is its `max`, and time at ≥90% CPU is the share of its points at or above 0.9. Points with a `cpu.mode` attribute (`user`, `system`, `iowait`, `steal`, `nice`, `interrupt`) are the same share split by mode. The job page stacks them.
  - `system.memory.usage` with `system.memory.state=used`, in bytes. Peak memory is its `max`. The job page also stacks the `cached` and `buffers` states.
  - `system.memory.limit`: `MemTotal` in bytes.
  - `system.cpu.logical.count`: `nproc`.
  - `system.disk.io` with `disk.io.direction` (`read` or `write`) and `system.device`, and `system.network.io` with `network.io.direction` (`receive` or `transmit`) and `network.interface.name`: cumulative byte counters. The job page plots their rate per second and leaves out `lo` and `tailscale0`.
  - Other metrics are stored as sent.

## Self-hosting

The tailnet needs MagicDNS and HTTPS certificates turned on, and the node needs the `funnel` attribute.

```sh
GAUGER_VERSION=$(git rev-parse --short=12 HEAD) GAUGER_TS_AUTHKEY=tskey-auth-... docker compose up -d --build
```

The UI navbar shows `GAUGER_VERSION`, or `dev` if it is unset.

The auth key is only read on first start. tsnet keeps the node key in the `state` volume, so keep that volume across upgrades. Losing it means enrolling again with a new key.

### GitHub App

Open `https://<host>.<tailnet>.ts.net/setup` from a device on the tailnet and create the App there. GitHub creates it from a manifest with `Actions: read` and `Metadata: read`, the `workflow_run` and `workflow_job` events, and the webhook URL `https://<host>.<tailnet>.ts.net:8443/webhooks/github` with a generated secret. The server stores the App ID, private key and webhook secret in Postgres. Then install the App on the repositories you want to measure.

`/setup` also has a form to queue a backfill of the last N days (90 by default) for every repository with an installation, so reports show history from before the App existed.

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

The dashboard's pages are templ files. Run `go tool templ generate` after changing a `.templ` file and commit the generated `*_templ.go`; `mise run check` fails when they are out of date.

To view the dashboard without a tailnet, point `mise run dashboard` at a database. `-seed` fills an empty one with the test fixtures, 90 days of runs across four repositories:

```sh
docker exec gauger-pg psql -U test -c 'CREATE DATABASE dashboard'
GAUGER_DATABASE_URL='postgres://test:test@127.0.0.1:55432/dashboard?sslmode=disable' mise run dashboard -seed
```

It serves only `/stats/` and `/static/` on `http://127.0.0.1:8090`, and refuses an `-addr` that is not a loopback IP.
