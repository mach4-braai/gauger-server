# gauger-server

Self-hosted server and web UI that joins GitHub Actions webhook timings with [gauger](https://github.com/mach4-braai/gauger) runner metrics in Postgres.

## How it works

- Joins your tailnet with `tsnet` and serves two listeners:
  - Public, through Tailscale Funnel: GitHub `workflow_run` and `workflow_job` webhooks, checked against `X-Hub-Signature-256`.
  - Tailnet only: OTLP from `tag:gauger-ci` runners, with the GitHub OIDC token checked for `repository_owner`.
- Joins both sources on `run_id`, `run_attempt` and `runner_name`, and stores them in Postgres. Retention defaults to 90 days.
- The web UI shows slow steps, regressions, runner sizing and Actions spend.

## Status

Design only. No code yet.
