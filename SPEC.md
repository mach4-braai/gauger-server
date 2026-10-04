# gauger-server spec

Self-hosted Go server and web UI. It joins GitHub Actions webhook timings with [gauger](https://github.com/mach4-braai/gauger) runner metrics and stores both in Postgres.

## Shape

- **One Go binary** serves the API, the ingest endpoints and the web UI.
- **Storage** is Postgres.
- **Config** comes from env vars. Ship a `docker compose` file with Postgres so anyone can self-host it.
- **tsnet.** Joins the tailnet as `tag:gauger-server` using `AuthKey` (infra output `gauger_server_auth_key`) on first start.
  - The tsnet state `Dir` must persist across restarts, because the auth key only works once.
  - Tagged nodes have key expiry turned off, so the node doesn't need to re-authenticate later.

## Listeners

These four must stay separate. A Funnel listener without `FunnelOnly` also accepts tailnet traffic, so the UI must never share it.

| Port | Reach | tsnet call | Serves |
|---|---|---|---|
| `:10000` | public only | `ListenFunnel("tcp", ":10000", tsnet.FunnelOnly())` | OTLP/HTTP and job lifecycle from GitHub Actions runners |
| `:4318` | tailnet only | `Listen` | OTLP/HTTP and job lifecycle from `tag:gauger-ci`, until no released gauger uses it |
| `:443` | tailnet only | `ListenTLS` | web UI |
| `:8443` | public only | `ListenFunnel("tcp", ":8443", tsnet.FunnelOnly())` | GitHub webhooks |

## GitHub App

- **Permissions:** `Actions: read`, `Metadata: read`.
- **Events:** `workflow_run`, `workflow_job`.
- **Webhook URL:** `https://<host>.<tailnet>.ts.net:8443/webhooks/github`. Protect it with a webhook secret.
- **Self-hosting.** Support the App Manifest flow, so each self-hoster creates their own App with the permissions above already set.

## Ingest rules

- **Webhooks.**
  - Verify `X-Hub-Signature-256` before parsing.
  - Deduplicate on `X-GitHub-Delivery`.
  - Assume some deliveries go missing, and repair from REST.
- **Runner data.** Accept it only when these checks pass:
  - The bearer token is a valid GitHub OIDC JWT: `iss` is `https://token.actions.githubusercontent.com`, `aud` is `gauger-server`, `exp` hasn't passed, and `repository_owner_id` is `287937105`. On `:10000` this is the only check.
  - On `:4318`, `WhoIs` also says the caller is `tag:gauger-ci`.

  Each request may only write to the job its token names. Every identity in the body must match the token's `repository`, `run_id`, `run_attempt` and `check_run_id` claims. A mismatch gets `403` and stores nothing.

  Limit the rate per job, keyed on the `check_run_id` claim after authentication, and per client address before the token is verified, counting failed authentications. Answer `429` with `Retry-After`, which gauger retries without dropping the batch. On a Funnel listener the client address is the Funnel connection's source, not the relay.

  Runners send a fresh token every few minutes, so expect a new token partway through a job.
- **Pending jobs.**
  - On `start`, or on the first batch, store the job as `pending`.
  - Poll `GET /actions/jobs/{id}` with backoff until `status=completed`, then record step timings.
  - Pending state must survive a restart.
  - If the samples never arrive, look for the fallback artifact `gauger-<check_run_id>` until it shows up or its 7 days expire.
- **Job matching.**
  - Join on `run_id`, `run_attempt` and `check_run_id`.
  - If the body has no `check_run_id`, use the token's `check_run_id` claim.
- **Rate limits.** Allow at least 5,000 requests an hour per installation. Back off on `x-ratelimit-*` and `retry-after`.
- **Webhook handler.** Choose between embedding the OTel `githubreceiver` (alpha) and writing your own handler. Record why in `docs/adr/`.

## Contract with gauger

This contract is shared. Change it in both repos together.

- **Endpoint:** `https://<host>.<tailnet>.ts.net:10000`, public HTTPS through Funnel. The GitHub OIDC token is the only credential.
- **Lifecycle:** `POST /v1/jobs/start` and `POST /v1/jobs/done`.
- **Metrics:** OTLP to `/v1/metrics`.
- **Identity attributes on every record:** `github.run_id`, `github.run_attempt`, `github.check_run_id`, `github.repository`, `github.workflow`, `github.job` and `runner.name`. They must name the job the token was issued to, or the request gets `403`.
- **Rate limits:** `429` with `Retry-After` when a job or client address is over its rate.

## Data

- **Tables:** `runs`, `jobs`, `steps` and `samples`.
- **Partitions.** Partition `samples` by day.
- **Retention.** Set per install, 90 days by default. Enforce it by dropping old partitions, not with row-by-row deletes.

## UI

The UI is a dashboard under `/stats/`, rendered by Go with templ and Datastar. `docs/adr/0002-datastar-dashboard.md` has the decisions.

- **Filters.** Every page shares a range (`24h`, `7d`, `30d`, `90d` or `all`), a repository and an event. They live in the query string, so a URL reproduces a view. A change updates the page in place. With JavaScript off, the filter form reloads it.
- **Time.** Buckets, hours and weekdays are UTC.
- **Live.** A page re-renders over one SSE stream when the server writes to the database.
- **Setup.** `/setup` and the manifest flow stay on `html/template`, and link to `/stats/`.
- **Old URLs.** `/`, `/jobs/{id}`, `/steps`, `/regressions`, `/daily`, `/sizing` and `/spend` redirect with 302 to their dashboard page and keep their query.

Pages:

| Page | Path | Shows |
|---|---|---|
| Overview | `/stats/` | Runs, jobs, steps, job minutes and estimated spend. Run and job success rate. Run duration and queue time p50 and p95. gauger coverage. Runs per bucket stacked by conclusion. |
| Runs | `/stats/runs` | Jobs per bucket stacked by conclusion, over a sortable, searchable table of jobs, 100 a page. |
| Job | `/stats/jobs/{id}` | One job: timeline of queue wait and steps, CPU, memory, disk and network charts from gauger, and runner usage per step. |
| Trends | `/stats/trends` | Median workflow and job duration per day, week or month, and a regressions table that compares each day's median step duration per branch with the 14 days before it. |
| Steps | `/stats/steps` | p50, p95 and total minutes per step name, and setup minutes against work minutes. |
| Spend | `/stats/spend` | Job minutes, rounded up per job, times the rate for the runner label, per repository, workflow, job and month. |
| Sizing | `/stats/sizing` | Peak memory against `MemTotal`, and CPU saturation against `nproc`, per runner label, job and step. Candidates for a smaller runner and their saving. |
| Breakdown | `/stats/breakdown` | Runs, jobs, minutes, spend, success rate, duration and queue time by repository, workflow, job, event or branch. |
| Capacity | `/stats/capacity` | Minutes and queue time per runner label, queue p95 by hour of day, runs by weekday and hour, and peak concurrency. |
| Resources | `/stats/resources` | CPU by mode, memory headroom, disk and network per runner label or workflow, for jobs with gauger samples. |
| Failures | `/stats/failures` | Failed and cancelled runs and the failure rate, failing jobs and steps, and the latest failed jobs with their first failed step. |
| Waste | `/stats/waste` | Minutes and spend on cancelled and failed jobs, re-runs, and jobs that failed and then passed on the same commit. |
| Health | `/stats/health` | Webhook deliveries by event type, the reconciler's task queue, and gauger coverage per bucket. |

Rules the pages follow:

- **Sizing and Resources.** Label CPU and memory as runner-level values during each step's time window, not the step's own usage.
- **Spend.** Private repositories pay for standard runners. Public repositories on standard runners and self-hosted runners are free. A label with no known rate shows its minutes as unknown, not as $0.
- **Resources.** Count only jobs with samples, and say so. Samples older than the retention period are gone, so a longer range reads from the oldest sample kept.

## Done when

- A real job produces run, job and step timings from webhooks, plus samples from gauger, joined in one view.
- A server restart mid-job loses nothing. Pending jobs resume, and the samples arrive later from gauger's buffer or the fallback artifact.
- From outside the tailnet, `:8443` and `:10000` answer and `:443` and `:4318` refuse connections.
