# A stats page for gauger-server

The idea is one detailed page, like `omp stats`. That page is built around headline totals, breakdowns per provider, model, folder and tool, time series, and two derived views called gain and frustration. omp gets there by reading raw session logs into rollups in its own `stats.db`. gauger-server already has its raw data in Postgres, so the question is which of those views have a CI equivalent, and which numbers we can produce today without collecting anything new.

All the numbers below come from read-only queries on the live database on 2026-10-02, after the 90-day backfill.

## What we store

| Table | Rows | What it holds | Coverage |
|---|---|---|---|
| `runs` | 851 | One row per run attempt. Workflow name and `path`, event, branch, SHA, status, conclusion, `created_at`, `run_started_at` | 6 repos, back to 2026-07-06. The backfill only fetched the latest attempt of each run |
| `jobs` | 2,687 | Name, labels, runner name and group, conclusion, `created_at`, `started_at`, `completed_at`, plus the gauger markers `runner_seen_at`, `runner_done_at`, `artifact_ingested_at` | Same as runs. Every job has both `created_at` and `started_at`, so queue time works everywhere |
| `steps` | 25,548 | Number, name, conclusion, start and end | Same as jobs |
| `samples` | about 166,000 points | gauger metrics per job and timestamp | 168 jobs (6% of jobs), only since 2026-09-30, and kept for 90 days |
| `repositories` | 6 | Visibility and installation | Complete |
| `webhook_deliveries` | 2,189 | Event type and when it arrived | Since 2026-09-30 |
| `tasks` | live queue | Reconciler work. Rows are deleted when they finish | No history |

These are the metrics gauger sends, with their attributes:

| Metric | Series | Read by a report today |
|---|---|---|
| `system.cpu.utilization` | total, and 7 `cpu.mode` values (user, system, iowait, steal, nice, interrupt, idle) | Total, and every mode but idle on the job page |
| `system.memory.usage` | `used`, `cached`, `buffers`, `free` | `used`, `cached` and `buffers` (job page) |
| `system.memory.limit`, `system.cpu.logical.count` | one | Yes |
| `system.linux.memory.available` | one | No |
| `system.disk.io`, `system.disk.operations` | read and write per device (`nvme0n1`, `sda`) | `system.disk.io` on the job page |
| `system.network.io` | 252 series, per interface and direction | Job page, per interface without `lo` and `tailscale0` |

The job page reads most of this. What no page reads yet: `free` memory, `system.linux.memory.available` and `system.disk.operations`.

## What the page could show

Every section below works with the data we have. Each one lists the source and a number from today's data where there is one.

### 1. Headline cards

Like omp's totals row, filtered by repository, days and event.

- Runs, jobs and steps in the window.
- Job minutes and estimated spend, using the existing `spend` rates.
- Success rate, counting runs by `runs.conclusion`, and also jobs.
- Median and p95 run duration.
- Median and p95 queue time. Today that's 3 s and 31 s.
- gauger coverage: the share of jobs that have samples (168 of 2,687), with how many came from the fallback artifact (68).

### 2. Time series

One SVG chart per metric, per day or week, built like the daily chart.

- Runs per day, stacked by conclusion.
- Job minutes per day, stacked by runner label. This answers "what is costing us more this week".
- Spend per day.
- Queue time p50 and p95 per day.
- Failure rate per day.

### 3. Breakdowns

These are the CI version of omp's per-model, per-folder and per-tool tables. Each one is a sortable table with a small bar per row, and each row links into the existing reports.

| Breakdown | Columns |
|---|---|
| Repository | runs, minutes, spend, success rate, p95 duration |
| Workflow (by `path`, as in #37) | the same, plus runs per day |
| Job | the same, plus queue p95 |
| Runner label | jobs, minutes, spend, queue p95. Today: `ubuntu-latest` has 1,523 min with a 30 s queue p95, and `macos-latest` has 294 min with a 46 s queue p95 |
| Event | `pull_request` 293 runs, `dynamic` 280, `push` 166, `workflow_dispatch` 64, `schedule` 44, `release` 4 |
| Branch | minutes and failure rate per branch. This shows how much `master` costs compared with PR branches |

### 4. Where the time goes

This uses steps, ranked by total minutes rather than p95. It's the closest thing to omp's tool view. Today's top 3 are `Run mise run check` (517 min), `Wait for the late OIDC fetches` (239 min) and `Perform CodeQL Analysis` (196 min).

- Step names have to be normalised first. `jdx/mise-action@9e7f…` and `jdx/mise-action@7e36…` are the same step and show up as two rows of 133 min each. Stripping `@<sha>` fixes that.
- Group steps into setup and real work. `Set up job`, `actions/checkout`, cache restore, `mise-action` and `Complete job` count as setup. The share of minutes spent before the first real step is a good number to watch.

### 5. Waiting

Queue time is `started_at - created_at` per job. It works for every job, including backfilled ones.

- Queue p50 and p95 per label and per hour of day. Hosted macOS and Windows runners wait longer.
- Time from a run's `created_at` to its first job starting.

### 6. Reliability and waste

This is the CI version of omp's frustration view.

- **Wasted minutes.** 252 cancelled job-minutes and 158 failed job-minutes, against 2,547 successful ones. Priced, that becomes "money spent on runs that didn't produce a result".
- **Re-runs.** 12 of 851 runs have `attempt > 1`. Group them by workflow and job.
- **Flaky candidates.** The same `head_sha` with a failed job and then a passing one, or a job that fails on one attempt and passes on the next.
- **Failure hotspots.** Failure rate per job and per step, using `steps.conclusion` to find the step that failed.

### 7. Runner resources

This only covers jobs with gauger samples, so the page has to say so.

- **CPU split.** user, system, iowait and steal are already stored. High iowait means the job waits on disk. Steal means a noisy neighbour on the host. Neither is visible anywhere today.
- **Memory headroom.** Peak `used` against `MemTotal` per job and label. Right-sizing does this per step already. A per-label summary would answer "do we need the bigger runner".
- **Disk.** Bytes read and written per job, by device.
- **Network.** Bytes in and out per job, which shows dependency downloads and cache restores. The 252 series need grouping by interface. Real interfaces would need to be kept apart from loopback and `tailscale0`.
- **Saving estimates.** Jobs that never pass 50% CPU or memory on a 4-core runner could run on a smaller label. Price the difference with the `spend` rates. This is roughly what omp's gain view does: an estimate of what could be saved.

### 8. Activity

- A heatmap of runs by hour and weekday. The busiest UTC hours today are 13:00 (101 runs), 12:00 (86) and 14:00 (83).
- Peak concurrency: the most jobs running at once per day, worked out from the start and end of each job. This matters for org concurrency limits.

### 9. Server health

- Webhook deliveries per day and per event type.
- The task queue by kind, with its oldest due time and any errors. That's where backfill progress would show up.
- gauger ingest: batches per day and how many jobs completed with no gauger data.

## What we can't show yet

| Want | Missing | What it would take |
|---|---|---|
| Who triggered a run, the PR number, the commit title | `actor`, `display_title`, `pull_requests` aren't stored | Add columns to `runs`. The webhook and REST payloads already include them |
| Earlier attempts of a re-run | The backfill only got the latest attempt | `run` tasks for attempts 1 to n-1 when `run_attempt > 1` |
| Backfill and task history | Tasks are deleted when they finish | A `task_log` table, or counters per kind and day |
| REST budget left | The rate limiter only exists in memory | Store `x-ratelimit-remaining` after each call |
| GitHub's own billable minutes | Not fetched | `GET /actions/runs/{id}/timing`, one call per run |
| Cache hit rates, test counts, runner image version | Only in job logs | Download logs, or have gauger report them |
| Resource stats before 2026-09-30, or for repos without gauger | No samples were collected | None. They only start when gauger runs |

## Caveats in the data

- Samples are kept for 90 days. Runs, jobs and steps are kept forever. Resource trends will stop at 90 days while timing trends keep going.
- Dynamic workflows (CodeQL, Dependabot) need the `path` grouping from #37. Without it, every breakdown splits per pull request.
- Skipped jobs have no times. They count toward totals but not toward durations.
- Spend is an estimate: minutes rounded up, multiplied by the published rate. It won't match GitHub's invoice for included minutes or larger runners without `GAUGER_RUNNER_RATES`.
- All days and hours are UTC.

## Building it

[ADR 0002](adr/0002-datastar-dashboard.md) records the decisions. In short:

- **Rendering.** The dashboard lives at `/stats/<page>`, rendered by Go with templ. Charts are SVG drawn on the server, ported from omp's `Chart.tsx`, with each slot's tooltip rendered on the server too. Datastar, one vendored 33 KB file, swaps the page in place when a filter changes and shows tooltips on hover. With JavaScript off, the filter form still works with a full reload.
- **Queries.** At 2,700 jobs and 25,000 steps, live aggregate queries return in milliseconds, so pages query `runs`, `jobs`, `steps` and `samples` directly. If that changes, add a `daily_rollups` table that the maintenance loop fills, keyed by day, repository, workflow path, job and label. That's the same approach omp takes with `stats.db`.
- **Page shape.** One page per view, like omp's sidebar, sharing the range, repository and event filters. The Overview at `/stats/` has the headline cards from section 1 and runs per bucket stacked by conclusion. Each later page replaces one of the template reports, which redirects to it.
- **Local preview.** `mise run dashboard -seed` serves the dashboard on 127.0.0.1 from `GAUGER_DATABASE_URL`, filled with the test fixtures when the database is empty.

## Suggested first slice

1. Headline cards, time series and breakdowns (sections 1 to 3). They only need `runs` and `jobs`.
2. Step normalisation and "where the time goes" (section 4).
3. Queue time and waste (sections 5 and 6).
4. Runner resources using the stored series nobody reads yet (section 7).
5. Server health (section 9). That covers backfill progress.

## Open questions

- Should `/stats` replace the nav entry for Recent jobs as the home page, or sit next to it?
- Should the waste and saving estimates be priced in dollars when most of the repos are public and free on standard runners?
- Is a 90-day sample retention enough once resource trends are on the page?
