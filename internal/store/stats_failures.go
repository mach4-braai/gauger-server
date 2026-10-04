package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// RecentFailureLimit is how many failed jobs Failures lists.
const RecentFailureLimit = 25

// FailureBucket counts the runs and jobs that started in one UTC bucket.
// Decided is the completed ones that were not cancelled or skipped, the
// denominator of a failure rate.
type FailureBucket struct {
	Bucket        time.Time
	RunsFailed    int64
	RunsCancelled int64
	RunsDecided   int64
	JobsFailed    int64
	JobsCancelled int64
	JobsDecided   int64
}

// JobFailureStat counts one job name of one workflow in one repository.
// Runs are the jobs that completed other than cancelled or skipped.
// LatestJobID is the most recent failed job.
type JobFailureStat struct {
	Repository  string
	Workflow    string
	Name        string
	Failures    int64
	Cancelled   int64
	Runs        int64
	LatestJobID int64
}

// StepFailureStat counts one normalised step name over the steps that ran
// and were not cancelled. The Latest fields locate the most recent failed
// occurrence.
type StepFailureStat struct {
	Name             string
	Failures         int64
	Runs             int64
	LatestJobID      int64
	LatestStep       int
	LatestJobHTMLURL string
}

// FailedJob is a job with conclusion failure. StepNumber and StepName are
// its first failed step, zero and empty when no step recorded a failure.
type FailedJob struct {
	JobID      int64
	RunID      int64
	Repository string
	Workflow   string
	Name       string
	Branch     string
	At         time.Time
	HTMLURL    string
	StepNumber int
	StepName   string
}

// FailureStats holds the failures page's data. Jobs and Steps list only
// names with a failure, most failures first. Recent holds the latest
// RecentFailureLimit failed jobs.
type FailureStats struct {
	Buckets []FailureBucket
	Jobs    []JobFailureStat
	Steps   []StepFailureStat
	Recent  []FailedJob
}

// failureDecided is the SQL condition for a completed run or job whose
// conclusion is not cancelled or skipped.
const failureDecided = `status = 'completed' AND coalesce(conclusion, '') NOT IN ('', 'cancelled', 'skipped')`

// Failures returns the window's failed and cancelled runs and jobs per UTC
// bucket ("hour" or "day"), the jobs and steps that fail most, and the
// latest failed jobs with their first failed step.
func (s *Store) Failures(ctx context.Context, f Filter, bucket string) (*FailureStats, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`,
		rb AS (
			SELECT date_trunc($5, coalesce(run_started_at, created_at) AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS bucket,
				count(*) FILTER (WHERE status = 'completed' AND conclusion = 'failure') AS failed,
				count(*) FILTER (WHERE status = 'completed' AND conclusion = 'cancelled') AS cancelled,
				count(*) FILTER (WHERE `+failureDecided+`) AS decided
			FROM fr GROUP BY 1
		),
		jb AS (
			SELECT date_trunc($5, coalesce(started_at, created_at, runner_seen_at) AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS bucket,
				count(*) FILTER (WHERE status = 'completed' AND conclusion = 'failure') AS failed,
				count(*) FILTER (WHERE status = 'completed' AND conclusion = 'cancelled') AS cancelled,
				count(*) FILTER (WHERE `+failureDecided+`) AS decided
			FROM fj GROUP BY 1
		)
		SELECT bucket, coalesce(rb.failed, 0), coalesce(rb.cancelled, 0), coalesce(rb.decided, 0),
			coalesce(jb.failed, 0), coalesce(jb.cancelled, 0), coalesce(jb.decided, 0)
		FROM rb FULL JOIN jb USING (bucket)
		ORDER BY bucket`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	buckets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[FailureBucket])
	if err != nil {
		return nil, err
	}

	rows, err = s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT repository, coalesce(workflow_name, ''), coalesce(name, ''),
			count(*) FILTER (WHERE status = 'completed' AND conclusion = 'failure'),
			count(*) FILTER (WHERE status = 'completed' AND conclusion = 'cancelled'),
			count(*) FILTER (WHERE `+failureDecided+`),
			(array_agg(id ORDER BY coalesce(completed_at, started_at, created_at, runner_seen_at) DESC NULLS LAST, id DESC)
				FILTER (WHERE status = 'completed' AND conclusion = 'failure'))[1]
		FROM fj
		GROUP BY 1, 2, 3
		HAVING count(*) FILTER (WHERE status = 'completed' AND conclusion = 'failure') > 0
		ORDER BY 4 DESC, 1, 2, 3`, f.args()...)
	if err != nil {
		return nil, err
	}
	jobs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[JobFailureStat])
	if err != nil {
		return nil, err
	}

	rows, err = s.Pool.Query(ctx, `
		WITH`+windowed+ranSteps+`
		SELECT name,
			count(*) FILTER (WHERE conclusion = 'failure'),
			count(*) FILTER (WHERE conclusion IS DISTINCT FROM 'cancelled'),
			(array_agg(job_id ORDER BY started_at DESC, job_id DESC, number DESC) FILTER (WHERE conclusion = 'failure'))[1],
			(array_agg(number ORDER BY started_at DESC, job_id DESC, number DESC) FILTER (WHERE conclusion = 'failure'))[1],
			coalesce((array_agg(html_url ORDER BY started_at DESC, job_id DESC, number DESC) FILTER (WHERE conclusion = 'failure'))[1], '')
		FROM ran
		GROUP BY name
		HAVING count(*) FILTER (WHERE conclusion = 'failure') > 0
		ORDER BY 2 DESC, name`, f.args()...)
	if err != nil {
		return nil, err
	}
	steps, err := pgx.CollectRows(rows, pgx.RowToStructByPos[StepFailureStat])
	if err != nil {
		return nil, err
	}

	rows, err = s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT fj.id, fj.run_id, fj.repository, coalesce(fj.workflow_name, ''), coalesce(fj.name, ''),
			coalesce(fj.head_branch, ''),
			coalesce(fj.completed_at, fj.started_at, fj.created_at, fj.runner_seen_at) AS at,
			coalesce(fj.html_url, ''), coalesce(cause.number, 0), coalesce(cause.name, '')
		FROM fj
		LEFT JOIN LATERAL (
			SELECT st.number, `+stepName("st.name")+` AS name
			FROM steps st
			WHERE st.job_id = fj.id AND st.conclusion = 'failure'
			ORDER BY st.number
			LIMIT 1
		) cause ON true
		WHERE fj.status = 'completed' AND fj.conclusion = 'failure'
		ORDER BY at DESC NULLS LAST, fj.id DESC
		LIMIT $5`, f.args(RecentFailureLimit)...)
	if err != nil {
		return nil, err
	}
	recent, err := pgx.CollectRows(rows, pgx.RowToStructByPos[FailedJob])
	if err != nil {
		return nil, err
	}
	return &FailureStats{Buckets: buckets, Jobs: jobs, Steps: steps, Recent: recent}, nil
}
