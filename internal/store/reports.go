package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Metric names and series the reports read. gauger sends them.
const (
	MetricCPUUtilization = "system.cpu.utilization"
	MetricCPUCount       = "system.cpu.logical.count"
	MetricMemoryUsage    = "system.memory.usage"
	MetricMemoryLimit    = "system.memory.limit"
	SeriesMemoryUsed     = "system.memory.state=used"
)

// saturatedCPU is the utilization at or above which a sample counts as
// saturated.
const saturatedCPU = 0.9

const stepWindows = `
			SELECT s.*, least(s.completed_at + interval '1 second',
				min(s.started_at) OVER (PARTITION BY s.job_id ORDER BY s.number ROWS BETWEEN 1 FOLLOWING AND UNBOUNDED FOLLOWING)) AS ends_at
			FROM steps s`

type JobRow struct {
	ID           int64
	Repository   string
	Workflow     string
	Name         string
	Branch       string
	Status       string
	Conclusion   string
	StartedAt    *time.Time
	CompletedAt  *time.Time
	Samples      int64
	FromArtifact bool
}

func (s *Store) RecentJobs(ctx context.Context, f Filter, limit int) ([]JobRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT j.id, j.repository, coalesce(j.workflow_name, r.workflow_name, ''), coalesce(j.name, ''),
			coalesce(j.head_branch, r.head_branch, ''), j.status, coalesce(j.conclusion, ''),
			j.started_at, j.completed_at,
			(SELECT count(DISTINCT m.ts) FROM samples m WHERE m.job_id = j.id),
			j.artifact_ingested_at IS NOT NULL
		FROM jobs j
		LEFT JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
		WHERE coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1
		  AND ($2 = '' OR j.repository = $2)
		ORDER BY coalesce(j.started_at, j.created_at, j.runner_seen_at) DESC NULLS LAST
		LIMIT $3`, f.Since, f.Repository, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[JobRow])
}

type JobDetail struct {
	JobRow
	RunID      int64
	RunAttempt int
	RunnerName string
	Labels     []string
	HTMLURL    string
	Event      string
	HeadSHA    string
	RunnerSeen *time.Time
	RunnerDone *time.Time
	MemTotal   *float64
	CPUCount   *float64
	Steps      []StepUsage
}

// StepUsage is a step with the runner's usage during its time window.
type StepUsage struct {
	Number      int
	Name        string
	Status      string
	Conclusion  string
	StartedAt   *time.Time
	CompletedAt *time.Time
	PeakMemory  *float64
	PeakCPU     *float64
	Saturated   *float64
	Samples     int64
}

func (s *Store) Job(ctx context.Context, id int64) (*JobDetail, error) {
	var d JobDetail
	err := s.Pool.QueryRow(ctx, `
		SELECT j.id, j.repository, coalesce(j.workflow_name, r.workflow_name, ''), coalesce(j.name, ''),
			coalesce(j.head_branch, r.head_branch, ''), j.status, coalesce(j.conclusion, ''),
			j.started_at, j.completed_at,
			(SELECT count(DISTINCT m.ts) FROM samples m WHERE m.job_id = j.id),
			j.artifact_ingested_at IS NOT NULL,
			j.run_id, j.run_attempt, coalesce(j.runner_name, ''), j.labels, coalesce(j.html_url, ''),
			coalesce(r.event, ''), coalesce(r.head_sha, ''), j.runner_seen_at, j.runner_done_at,
			(SELECT max(value) FROM samples m WHERE m.job_id = j.id AND m.metric = $2),
			(SELECT max(value) FROM samples m WHERE m.job_id = j.id AND m.metric = $3)
		FROM jobs j
		LEFT JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
		WHERE j.id = $1`, id, MetricMemoryLimit, MetricCPUCount).Scan(
		&d.ID, &d.Repository, &d.Workflow, &d.Name, &d.Branch, &d.Status, &d.Conclusion,
		&d.StartedAt, &d.CompletedAt, &d.Samples, &d.FromArtifact,
		&d.RunID, &d.RunAttempt, &d.RunnerName, &d.Labels, &d.HTMLURL,
		&d.Event, &d.HeadSHA, &d.RunnerSeen, &d.RunnerDone, &d.MemTotal, &d.CPUCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		WITH w AS (`+stepWindows+`
			WHERE s.job_id = $1
		)
		SELECT w.number, w.name, w.status, coalesce(w.conclusion, ''), w.started_at, w.completed_at,
			max(m.value) FILTER (WHERE m.metric = $2 AND m.series = $3),
			max(m.value) FILTER (WHERE m.metric = $4 AND m.series = ''),
			avg(CASE WHEN m.value >= $5 THEN 1.0 ELSE 0.0 END) FILTER (WHERE m.metric = $4 AND m.series = ''),
			count(DISTINCT m.ts)
		FROM w
		LEFT JOIN samples m ON m.job_id = w.job_id AND m.ts >= w.started_at AND m.ts < w.ends_at
		GROUP BY w.number, w.name, w.status, w.conclusion, w.started_at, w.completed_at
		ORDER BY w.number`, id, MetricMemoryUsage, SeriesMemoryUsed, MetricCPUUtilization, saturatedCPU)
	if err != nil {
		return nil, err
	}
	d.Steps, err = pgx.CollectRows(rows, pgx.RowToStructByPos[StepUsage])
	return &d, err
}

type Sample struct {
	Time   time.Time
	Metric string
	Value  float64
}

// JobSeries returns CPU utilization and used memory for a job's chart.
func (s *Store) JobSeries(ctx context.Context, id int64) ([]Sample, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT ts, metric, value FROM samples
		WHERE job_id = $1 AND ((metric = $2 AND series = '') OR (metric = $3 AND series = $4))
		ORDER BY ts`, id, MetricCPUUtilization, MetricMemoryUsage, SeriesMemoryUsed)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Sample])
}

// Filter narrows a report to one repository (empty for all) since a time.
type Filter struct {
	Repository string
	Since      time.Time
}

type SlowStep struct {
	Name string
	Runs int64
	P50  float64
	P95  float64
}

// SlowSteps returns p50 and p95 duration in seconds per step name over
// successful steps.
func (s *Store) SlowSteps(ctx context.Context, f Filter) ([]SlowStep, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT s.name, count(*),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM s.completed_at - s.started_at)),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM s.completed_at - s.started_at))
		FROM steps s JOIN jobs j ON j.id = s.job_id
		WHERE s.conclusion = 'success' AND s.started_at >= $1 AND s.completed_at IS NOT NULL
		  AND ($2 = '' OR j.repository = $2)
		GROUP BY s.name
		ORDER BY 4 DESC
		LIMIT 200`, f.Since, f.Repository)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SlowStep])
}

type Regression struct {
	Repository string
	Workflow   string
	Job        string
	Step       string
	Branch     string
	Day        time.Time
	Median     float64
	Runs       int64
	Baseline   float64
	BaseRuns   int64
}

// BaselineDays is the rolling window a day's median is compared with.
const BaselineDays = 14

// Regressions compares each day's median duration per repository,
// workflow, job, step and branch with the median of the BaselineDays
// before it. It returns days at least ratio times their baseline.
func (s *Store) Regressions(ctx context.Context, f Filter, ratio, minSeconds float64) ([]Regression, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH d AS (
			SELECT j.repository, coalesce(j.workflow_name, '') AS workflow, coalesce(j.name, '') AS job,
				s.name AS step, coalesce(j.head_branch, '') AS branch,
				(s.started_at AT TIME ZONE 'UTC')::date AS day,
				extract(epoch FROM s.completed_at - s.started_at) AS secs
			FROM steps s JOIN jobs j ON j.id = s.job_id
			WHERE s.conclusion = 'success' AND s.completed_at IS NOT NULL
			  AND s.started_at >= $1::timestamptz - make_interval(days => $5)
			  AND ($2 = '' OR j.repository = $2)
		),
		daily AS (
			SELECT repository, workflow, job, step, branch, day,
				percentile_cont(0.5) WITHIN GROUP (ORDER BY secs) AS median, count(*) AS runs
			FROM d
			WHERE day >= ($1::timestamptz AT TIME ZONE 'UTC')::date
			GROUP BY 1, 2, 3, 4, 5, 6
		)
		SELECT daily.repository, daily.workflow, daily.job, daily.step, daily.branch, daily.day::timestamptz,
			daily.median, daily.runs, b.base, b.n
		FROM daily
		CROSS JOIN LATERAL (
			SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY d.secs) AS base, count(*) AS n
			FROM d
			WHERE d.repository = daily.repository AND d.workflow = daily.workflow AND d.job = daily.job
			  AND d.step = daily.step AND d.branch = daily.branch
			  AND d.day >= daily.day - $5 AND d.day < daily.day
		) b
		WHERE b.n >= 3 AND b.base > 0 AND daily.median >= $4 AND daily.median >= b.base * $3
		ORDER BY daily.median / b.base DESC
		LIMIT 200`, f.Since, f.Repository, ratio, minSeconds, BaselineDays)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Regression])
}

type Sizing struct {
	Repository string
	Workflow   string
	Job        string
	Step       string
	Runs       int64
	PeakMemory *float64
	MemTotal   *float64
	PeakCPU    *float64
	Saturated  *float64
	CPUCount   *float64
}

// Sizing reports, per step, the runner's peak memory against MemTotal and
// its CPU use against nproc during the step's time window.
func (s *Store) Sizing(ctx context.Context, f Filter) ([]Sizing, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH win AS (
			SELECT j.repository, coalesce(j.workflow_name, '') AS workflow, coalesce(j.name, '') AS job,
				w.name AS step, w.job_id, w.started_at, w.ends_at
			FROM (`+stepWindows+`
				WHERE s.started_at >= $1 AND s.completed_at IS NOT NULL
			) w JOIN jobs j ON j.id = w.job_id
			WHERE ($2 = '' OR j.repository = $2)
			  AND EXISTS (SELECT 1 FROM samples x WHERE x.job_id = w.job_id)
		)
		SELECT w.repository, w.workflow, w.job, w.step, count(DISTINCT w.job_id),
			max(m.value) FILTER (WHERE m.metric = $3 AND m.series = $4),
			max(k.mem_total),
			max(m.value) FILTER (WHERE m.metric = $5 AND m.series = ''),
			avg(CASE WHEN m.value >= $8 THEN 1.0 ELSE 0.0 END) FILTER (WHERE m.metric = $5 AND m.series = ''),
			max(k.cpus)
		FROM win w
		JOIN samples m ON m.job_id = w.job_id AND m.ts >= w.started_at AND m.ts < w.ends_at
		CROSS JOIN LATERAL (
			SELECT max(value) FILTER (WHERE metric = $6) AS mem_total, max(value) FILTER (WHERE metric = $7) AS cpus
			FROM samples WHERE job_id = w.job_id AND metric IN ($6, $7)
		) k
		GROUP BY 1, 2, 3, 4
		ORDER BY max(m.value) FILTER (WHERE m.metric = $3 AND m.series = $4) / nullif(max(k.mem_total), 0) DESC NULLS LAST
		LIMIT 300`, f.Since, f.Repository, MetricMemoryUsage, SeriesMemoryUsed, MetricCPUUtilization,
		MetricMemoryLimit, MetricCPUCount, saturatedCPU)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Sizing])
}

type SpendGroup struct {
	Repository string
	Month      time.Time
	Labels     []string
	Private    *bool
	Jobs       int64
	Minutes    int64
}

// SpendGroups sums job minutes, each job rounded up to a whole minute, per
// repository, month and runner labels.
func (s *Store) SpendGroups(ctx context.Context, f Filter) ([]SpendGroup, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT j.repository, date_trunc('month', j.completed_at AT TIME ZONE 'UTC')::timestamptz, j.labels, r.private,
			count(*), sum(ceil(extract(epoch FROM j.completed_at - j.started_at) / 60))::bigint
		FROM jobs j
		LEFT JOIN repositories r ON r.full_name = j.repository
		WHERE j.status = 'completed' AND j.started_at IS NOT NULL AND j.completed_at > j.started_at
		  AND j.completed_at >= $1 AND ($2 = '' OR j.repository = $2)
		GROUP BY 1, 2, 3, 4
		ORDER BY 2 DESC, 1`, f.Since, f.Repository)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SpendGroup])
}

// DailyDuration is the median duration of a workflow's runs, or of one of
// its jobs, in one bucket. Job is empty for the workflow.
type DailyDuration struct {
	Repository string
	Workflow   string
	Job        string
	Bucket     time.Time
	Median     float64
	Runs       int64
}

// DailyDurations returns per-bucket medians for successful jobs and for
// runs GitHub reports as completed with success. A run lasts from its
// first successful job's start to its last successful job's end. bucket
// is "day", "week" or "month"; weeks start Monday UTC. The median is
// taken over every run (or job) in the bucket, not over daily medians.
func (s *Store) DailyDurations(ctx context.Context, f Filter, bucket string) ([]DailyDuration, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH j AS (
			SELECT j.repository, coalesce(j.workflow_name, r.workflow_name, '') AS workflow, coalesce(j.name, '') AS job,
				j.run_id, j.run_attempt, coalesce(r.status, '') AS run_status, coalesce(r.conclusion, '') AS run_conclusion,
				coalesce(j.conclusion, '') AS conclusion,
				j.started_at, j.completed_at
			FROM jobs j
			LEFT JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
			WHERE j.status = 'completed' AND j.started_at >= $1 AND j.completed_at >= j.started_at
			  AND ($2 = '' OR j.repository = $2)
		),
		run AS (
			SELECT repository, workflow, min(started_at) FILTER (WHERE conclusion = 'success') AS started_at,
				max(completed_at) FILTER (WHERE conclusion = 'success') AS completed_at
			FROM j
			GROUP BY repository, workflow, run_id, run_attempt
			HAVING bool_and(run_status = 'completed' AND run_conclusion = 'success') AND bool_or(conclusion = 'success')
		)
		SELECT repository, workflow, '', date_trunc($3, started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM completed_at - started_at)), count(*)
		FROM run
		GROUP BY 1, 2, 3, 4
		UNION ALL
		SELECT repository, workflow, job, date_trunc($3, started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM completed_at - started_at)), count(*)
		FROM j
		WHERE conclusion = 'success'
		GROUP BY 1, 2, 3, 4
		ORDER BY 1, 2, 3, 4`, f.Since, f.Repository, bucket)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DailyDuration])
}

func (s *Store) Repositories(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT repository FROM jobs ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
