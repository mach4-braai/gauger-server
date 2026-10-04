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

These three must stay separate. A Funnel listener without `FunnelOnly` also accepts tailnet traffic, so the UI must never share it.

| Port | Reach | tsnet call | Serves |
|---|---|---|---|
| `:4318` | tailnet only | `Listen` | OTLP/HTTP and job lifecycle from `tag:gauger-ci` |
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
- **Runner data.** Accept it only when both checks pass:
  - `WhoIs` says the caller is `tag:gauger-ci`.
  - The bearer token is a valid GitHub OIDC JWT: `iss` is `https://token.actions.githubusercontent.com`, `aud` is `gauger-server`, `exp` hasn't passed, and `repository_owner_id` is `287937105`.

  Each request may only write to the job its token names. Every identity in the body must match the token's `repository`, `run_id`, `run_attempt` and `check_run_id` claims. A mismatch gets `403` and stores nothing.

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

- **Lifecycle:** `POST /v1/jobs/start` and `POST /v1/jobs/done`.
- **Metrics:** OTLP to `/v1/metrics`.
- **Identity attributes on every record:** `github.run_id`, `github.run_attempt`, `github.check_run_id`, `github.repository`, `github.workflow`, `github.job` and `runner.name`.

## Data

- **Tables:** `runs`, `jobs`, `steps` and `samples`.
- **Partitions.** Partition `samples` by day.
- **Retention.** Set per install, 90 days by default. Enforce it by dropping old partitions, not with row-by-row deletes.

## UI (v1)

- **Slow steps.** p50 and p95 per step name.
- **Regressions.** Daily and per-branch step durations compared with a rolling baseline.
- **Right-sizing.** Peak memory against `MemTotal`, and CPU saturation against `nproc`. Label these as runner-level values during each step's time window, not the step's own usage.
- **Spend.**
  - Job minutes, rounded up per job, times the rate for the runner label.
  - Private repos only. Public repos on standard runners are free.

## Done when

- A real job produces run, job and step timings from webhooks, plus samples from gauger, joined in one view.
- A server restart mid-job loses nothing. Pending jobs resume, and the samples arrive later from gauger's buffer or the fallback artifact.
- From outside the tailnet, `:8443` answers and `:443` and `:4318` refuse connections.
