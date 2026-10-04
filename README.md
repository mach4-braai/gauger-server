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

The UI is a dashboard under `/stats/`, one page per view, behind a sidebar. Every page filters by range (24h, 7d, 30d, 90d or all), repository and event. A filter change or a legend toggle updates the page in place and the URL with it, so a pasted URL shows the same view. With JavaScript off the filters reload the page. A page refreshes itself when the server writes new data, and a chip in the header shows whether that stream is live. Times are UTC. See [ADR 0002](docs/adr/0002-datastar-dashboard.md).

`/setup` creates the GitHub App and queues backfills. The old report URLs (`/`, `/jobs/{id}`, `/steps`, `/regressions`, `/daily`, `/sizing` and `/spend`) redirect to their dashboard page and keep their query string.

- **Overview** at `/stats/`. Cards for runs, jobs, steps, job minutes, estimated spend, run and job success rate, run duration and queue time p50 and p95, and gauger coverage, above a chart of runs per hour or day stacked by conclusion.
- **Runs** at `/stats/runs`. A chart of jobs per hour or day stacked by conclusion, over a table of the jobs, 100 to a page. A search box, a conclusion filter, a workflow or branch filter and a click on a chart bucket narrow the table, and any column sorts it.
- **Job** at `/stats/jobs/{id}`. A timeline of the queue wait and each step, CPU, memory, disk and network charts from gauger, and runner usage per step. Memory shows the used, cached and buffers states, and CPU the busy share split by mode. Disk and network show a rate per second.
- **Trends** at `/stats/trends`. A chart of the median duration per day, week or month, with one series per workflow, or per job once a workflow is picked, and the run count on the right axis. A workflow run lasts from its first job's start to its last job's end and counts only when GitHub reports it succeeded. Workflows group by file, so dynamic ones such as CodeQL stay one series. There are at most 60 buckets, and weeks start Monday UTC. A regressions table compares each day's median step duration per branch with the median of the 14 days before it, and selecting a day charts that step against its baseline. The event filter does not apply to this page.
- **Steps** at `/stats/steps`. Minutes, p50 and p95 per step name, with the `@ref` of action steps removed so versions of one action count together. A bar list ranks the steps by total minutes, and a split shows setup minutes (set up job, complete job, post steps, checkout, cache restore and `mise-action`) against work minutes. Each row links to the job of its slowest run.
- **Spend** at `/stats/spend`. Job minutes, rounded up per job, times the rate for the runner label. Standard runners in public repositories and self-hosted runners are free. Rates default to GitHub's published prices, and `GAUGER_RUNNER_RATES` adds larger runners. Minutes on labels with no rate show as unknown, not $0. The page has cards for spend and for billable, free and unknown-rate minutes, a chart per bucket stacked by label that toggles to minutes, tables per repository, workflow and job with a trend, and a month by repository by label table.
- **Sizing** at `/stats/sizing`. It counts, per runner label, the sampled jobs that went over 50% of the CPU or of the memory, and flags jobs that never did as candidates for the next smaller label. A candidate's saving is its minutes at its label's rate minus the same minutes at the smaller label. It is blank when the minutes are free or no smaller label is known. `internal/spend` holds the order of sizes. A step table shows peak memory against `MemTotal` and CPU against `nproc`. These are runner-level values during the step's time window, not the step's own usage.
- **Breakdown** at `/stats/breakdown`. Runs, jobs, minutes, spend, success rate (failure rate for branches), duration p50 and p95 and queue p95, grouped by repository, workflow, job, event or branch, with a share bar of job minutes.
- **Capacity** at `/stats/capacity`. Jobs, minutes, spend and queue time per set of runner labels, minutes and queue time per bucket by label, queue p95 by hour of day, runs by weekday and hour, and the most jobs running at once.
- **Resources** at `/stats/resources`. CPU by mode, memory against the runner's limit, disk and network bytes, the top jobs by disk and network, and a CPU and memory trend, grouped by runner label or by workflow. It counts only jobs with gauger samples and says how many there are. Samples are kept for `GAUGER_RETENTION_DAYS`, so a longer range reads from the oldest sample kept.
- **Failures** at `/stats/failures`. Failed and cancelled runs per bucket with the failure rate, failing jobs and failing steps, and the latest failed jobs with the first step that failed in each. Cancelled runs and jobs are not failures.
- **Waste** at `/stats/waste`. Minutes and spend on cancelled and failed jobs, re-runs, and flaky candidates, which are jobs that failed and then passed on the same commit.
- **Health** at `/stats/health`. Webhook deliveries by event type, the reconciler's task queue by kind, and gauger coverage per bucket, split by whether the samples arrived live, from the fallback artifact or not at all.

## Contract with gauger

This contract is shared with [gauger](https://github.com/mach4-braai/gauger). Change it in both repos together.

- Send to `https://<host>.<tailnet>.ts.net:10000` over public HTTPS. Every request carries `Authorization: Bearer <GitHub OIDC JWT>` with audience `gauger-server`, and the token is the only credential.
- `POST /v1/jobs/start` and `POST /v1/jobs/done` take a JSON object whose keys are the identity attributes below. Values may be strings or numbers. The reply is `{"job_id": <id>}`.
- `POST /v1/metrics` takes OTLP/HTTP metrics as protobuf or JSON, optionally gzip-compressed. Gauge and sum points are stored. Repeated points (same job, metric, attributes and time) are ignored, so replaying a buffer is safe.
- Identity attributes go on the resource or on each data point: `github.run_id`, `github.run_attempt`, `github.check_run_id`, `github.repository`, `github.workflow`, `github.job` and `runner.name`. `github.check_run_id` may be empty; the server then uses the token's claim.
- The identity must name the job the token was issued to. A `403` means it named another one. Keep the data buffered and retry, so it ends up in the fallback artifact.
- A `429` with `Retry-After` means the job or address is over its rate. Keep the data buffered and retry.
- The fallback artifact `gauger-<check_run_id>` holds one file per unsent batch, each an `ExportMetricsServiceRequest` in protobuf.
- The dashboard reads these metric shapes. A client that sends another shape gets wrong numbers, so changing one is a contract change.
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

`/setup` also has a form to queue a backfill of the last N days (90 by default) for every repository with an installation, so the dashboard shows history from before the App existed.

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
