package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// wasted selects the jobs that spent minutes without a result: completed
// jobs that were cancelled or failed after they started.
const wasted = `
		wj AS (
			SELECT * FROM fj
			WHERE status = 'completed' AND conclusion IN ('cancelled', 'failure')
			  AND started_at IS NOT NULL AND completed_at > started_at
		)`

// WastedMinutes is the minutes of cancelled or failed jobs, each job
// rounded up to a whole minute, run on one set of runner labels in
// repositories of one visibility.
type WastedMinutes struct {
	Labels     []string
	Private    *bool
	Conclusion string
	Minutes    int64
}

// WastedMinutes sums the window's cancelled and failed job minutes.
func (s *Store) WastedMinutes(ctx context.Context, f Filter) ([]WastedMinutes, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`,`+wasted+`
		SELECT wj.labels, rp.private, wj.conclusion, sum(ceil(extract(epoch FROM wj.completed_at - wj.started_at) / 60))::bigint
		FROM wj
		LEFT JOIN repositories rp ON rp.full_name = wj.repository
		GROUP BY 1, 2, 3
		ORDER BY 4 DESC`, f.args()...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[WastedMinutes])
}

// WastedBucket is the wasted minutes of one conclusion in one bucket.
type WastedBucket struct {
	Bucket     time.Time
	Conclusion string
	Minutes    int64
}

// WastedByBucket sums wasted minutes per UTC bucket ("hour" or "day") and
// conclusion, by the bucket each job started in.
func (s *Store) WastedByBucket(ctx context.Context, f Filter, bucket string) ([]WastedBucket, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`,`+wasted+`
		SELECT date_trunc($5, wj.started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', wj.conclusion,
			sum(ceil(extract(epoch FROM wj.completed_at - wj.started_at) / 60))::bigint
		FROM wj
		GROUP BY 1, 2
		ORDER BY 1, 2`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[WastedBucket])
}

// ReRunCount is the number of run attempts after the first that started in
// the window.
func (s *Store) ReRunCount(ctx context.Context, f Filter) (int64, error) {
	var n int64
	err := s.Pool.QueryRow(ctx, `
		WITH`+windowed+`
		SELECT count(*) FROM fr WHERE attempt > 1`, f.args()...).Scan(&n)
	return n, err
}

// JobRef names one job so a page can link to it. ID is zero when the job
// was never stored.
type JobRef struct {
	ID         int64
	Conclusion string
}

// ReRun counts the re-run attempts of one job of one workflow. Earlier is
// the job in the attempt before the latest re-run and Latest is the
// latest re-run. Earlier.ID is zero when that attempt was not stored.
type ReRun struct {
	Repository string
	Workflow   string
	Job        string
	Attempts   int64
	Runs       int64
	Earlier    JobRef
	Latest     JobRef
}

// ReRuns lists jobs that ran again in a later attempt of their run, most
// re-runs first.
func (s *Store) ReRuns(ctx context.Context, f Filter) ([]ReRun, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`,
		rj AS (
			SELECT id, run_id, run_attempt, repository, coalesce(workflow_name, '') AS workflow, coalesce(name, '') AS name,
				coalesce(conclusion, status) AS conclusion,
				coalesce(started_at, created_at, runner_seen_at) AS ts
			FROM fj WHERE run_attempt > 1
		),
		g AS (
			SELECT repository, workflow, name, count(*) AS attempts, count(DISTINCT run_id) AS runs
			FROM rj GROUP BY 1, 2, 3
		),
		l AS (
			SELECT DISTINCT ON (repository, workflow, name) *
			FROM rj ORDER BY repository, workflow, name, ts DESC, id DESC
		)
		SELECT g.repository, g.workflow, g.name, g.attempts, g.runs,
			coalesce(e.id, 0), coalesce(e.conclusion, ''), l.id, l.conclusion
		FROM g
		JOIN l USING (repository, workflow, name)
		LEFT JOIN LATERAL (
			SELECT p.id, coalesce(p.conclusion, p.status) AS conclusion FROM jobs p
			WHERE p.run_id = l.run_id AND p.run_attempt = l.run_attempt - 1 AND coalesce(p.name, '') = l.name
			ORDER BY p.id LIMIT 1
		) e ON true
		ORDER BY g.attempts DESC, g.repository, g.workflow, g.name`, f.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReRun
	for rows.Next() {
		var r ReRun
		if err := rows.Scan(&r.Repository, &r.Workflow, &r.Job, &r.Attempts, &r.Runs,
			&r.Earlier.ID, &r.Earlier.Conclusion, &r.Latest.ID, &r.Latest.Conclusion); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Flaky is a job that failed and then passed on the same commit, in a
// later attempt or a later run. Failed is the last failure before the
// first pass.
type Flaky struct {
	Repository string
	Workflow   string
	Job        string
	SHA        string
	Branch     string
	Failures   int64
	Failed     FlakyJob
	Passed     FlakyJob
}

// FlakyJob is one side of a Flaky candidate.
type FlakyJob struct {
	ID      int64
	RunID   int64
	Attempt int
	At      time.Time
}

// FlakyCandidates lists jobs with a failure followed by a pass on the same
// head_sha, repository, workflow and job name, latest pass first. A job
// that only ever fails, or passes before it fails, is not a candidate.
func (s *Store) FlakyCandidates(ctx context.Context, f Filter) ([]Flaky, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`,
		c AS (
			SELECT fj.id, fj.run_id, fj.run_attempt, fj.repository, coalesce(fj.workflow_name, '') AS workflow, fj.name,
				fj.conclusion, fj.completed_at, r.head_sha, coalesce(r.head_branch, '') AS branch
			FROM fj JOIN runs r ON r.id = fj.run_id AND r.attempt = fj.run_attempt
			WHERE fj.status = 'completed' AND fj.conclusion IN ('failure', 'success')
			  AND fj.completed_at IS NOT NULL AND coalesce(r.head_sha, '') <> '' AND coalesce(fj.name, '') <> ''
		),
		pairs AS (
			SELECT DISTINCT ON (p.repository, p.workflow, p.name, p.head_sha)
				p.repository, p.workflow, p.name, p.head_sha, p.branch,
				x.id AS failed_id, x.run_id AS failed_run, x.run_attempt AS failed_attempt, x.completed_at AS failed_at,
				p.id AS passed_id, p.run_id AS passed_run, p.run_attempt AS passed_attempt, p.completed_at AS passed_at
			FROM c p
			JOIN c x ON x.repository = p.repository AND x.workflow = p.workflow AND x.name = p.name AND x.head_sha = p.head_sha
			WHERE p.conclusion = 'success' AND x.conclusion = 'failure' AND x.completed_at < p.completed_at
			ORDER BY p.repository, p.workflow, p.name, p.head_sha, p.completed_at, x.completed_at DESC, x.id DESC
		)
		SELECT pairs.repository, pairs.workflow, pairs.name, pairs.head_sha, pairs.branch,
			(SELECT count(*) FROM c x
				WHERE x.repository = pairs.repository AND x.workflow = pairs.workflow AND x.name = pairs.name
				  AND x.head_sha = pairs.head_sha AND x.conclusion = 'failure' AND x.completed_at < pairs.passed_at),
			failed_id, failed_run, failed_attempt, failed_at,
			passed_id, passed_run, passed_attempt, passed_at
		FROM pairs
		ORDER BY passed_at DESC, passed_id DESC`, f.args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Flaky
	for rows.Next() {
		var c Flaky
		if err := rows.Scan(&c.Repository, &c.Workflow, &c.Job, &c.SHA, &c.Branch, &c.Failures,
			&c.Failed.ID, &c.Failed.RunID, &c.Failed.Attempt, &c.Failed.At,
			&c.Passed.ID, &c.Passed.RunID, &c.Passed.Attempt, &c.Passed.At); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
