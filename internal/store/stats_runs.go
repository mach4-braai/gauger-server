package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RunsPageSize is the number of rows Runs returns for a page.
const RunsPageSize = 100

// runsOrder lists the columns Runs sorts by, with the SQL each sorts on.
// Expressions read the job as x.
var runsOrder = []struct{ key, expr string }{
	{"started", "x.began"},
	{"repo", "x.repository"},
	{"workflow", "x.workflow"},
	{"job", "x.name"},
	{"branch", "x.branch"},
	{"event", "x.event"},
	{"outcome", "x.outcome"},
	{"duration", "extract(epoch FROM x.completed_at - x.started_at)"},
	{"queue", "x.queue"},
	{"samples", "(SELECT count(DISTINCT m.ts) FROM samples m WHERE m.job_id = x.id)"},
}

// IsRunsSort reports whether key is a column Runs sorts by.
func IsRunsSort(key string) bool {
	for _, o := range runsOrder {
		if o.key == key {
			return true
		}
	}
	return false
}

// Job outcomes, as the Runs page groups them: the three conclusions it
// names, any other conclusion, and jobs that have not completed.
const (
	OutcomeSuccess   = "success"
	OutcomeFailure   = "failure"
	OutcomeCancelled = "cancelled"
	OutcomeOther     = "other"
	OutcomeRunning   = "running"
)

// RunsQuery narrows the jobs a Filter selects and says how to page and
// order them. Empty fields do not narrow.
type RunsQuery struct {
	// Workflow and Branch match exactly.
	Workflow string
	Branch   string
	// Outcome is one of the Outcome constants.
	Outcome string
	// Search matches, ignoring case, inside the workflow, job and branch.
	Search string
	// BucketStart and BucketEnd keep jobs that began in [start, end). The
	// chart ignores them.
	BucketStart, BucketEnd time.Time
	// Sort is a key IsRunsSort accepts. Default "started".
	Sort string
	Desc bool
	// Offset is rounded down to a page, and clamped to the last one.
	Offset int
}

// RunRow is a job of the Runs page: the columns RecentJobs returns, with
// the run it belongs to, the commit, and how long it queued. Outcome is the
// job's group, one of the Outcome constants.
type RunRow struct {
	JobRow
	RunID      int64
	RunAttempt int
	Event      string
	HeadSHA    string
	// Queue is a job's start minus its creation, in seconds.
	Queue   *float64
	Outcome string
}

// RunsPage is one page of RunRows. Total counts every job that matches;
// Offset is where the page starts.
type RunsPage struct {
	Rows   []RunRow
	Total  int
	Offset int
}

// runsFiltered extends windowed with rf, the jobs in the window that match
// a RunsQuery's workflow, branch, outcome and search. A query that uses it
// passes RunsQuery's four fields as $5 to $8.
const runsFiltered = `,
		rj AS (
			SELECT j.id, j.repository, j.run_id, j.run_attempt, j.status, coalesce(j.conclusion, '') AS conclusion,
				j.started_at, j.completed_at, j.artifact_ingested_at IS NOT NULL AS from_artifact,
				coalesce(j.workflow_name, r.workflow_name, '') AS workflow, coalesce(r.path, '') AS path,
				coalesce(j.name, '') AS name, coalesce(j.head_branch, r.head_branch, '') AS branch,
				coalesce(r.event, '') AS event, coalesce(r.head_sha, '') AS head_sha,
				coalesce(j.started_at, j.created_at, j.runner_seen_at) AS began,
				CASE WHEN j.started_at >= j.created_at THEN extract(epoch FROM j.started_at - j.created_at) END AS queue,
				CASE WHEN j.status <> 'completed' THEN 'running'
					WHEN j.conclusion IN ('success', 'failure', 'cancelled') THEN j.conclusion
					ELSE 'other' END AS outcome
			FROM fj j
			LEFT JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
		),
		rf AS (
			SELECT * FROM rj
			WHERE ($5 = '' OR workflow = $5) AND ($6 = '' OR branch = $6) AND ($7 = '' OR outcome = $7)
			  AND ($8 = '' OR strpos(lower(workflow || E'\n' || name || E'\n' || branch), lower($8)) > 0)
		)`

const inBucket = `($9::timestamptz IS NULL OR (began >= $9 AND began < $10))`

// OutcomeCount is the number of jobs with one outcome that began in one
// bucket.
type OutcomeCount struct {
	Bucket  time.Time
	Outcome string
	Jobs    int64
}

// RunsByOutcome counts the jobs q selects per UTC bucket ("hour" or "day")
// and outcome. It ignores q's bucket, so a chart can show where the
// selected one sits.
func (s *Store) RunsByOutcome(ctx context.Context, f Filter, q RunsQuery, bucket string) ([]OutcomeCount, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+runsFiltered+`
		SELECT date_trunc($9, began AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', outcome, count(*)
		FROM rf
		GROUP BY 1, 2
		ORDER BY 1, 2`, f.args(q.Workflow, q.Branch, q.Outcome, q.Search, bucket)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[OutcomeCount])
}

// Runs returns one page of the jobs q selects, newest first unless q sorts
// otherwise.
func (s *Store) Runs(ctx context.Context, f Filter, q RunsQuery) (*RunsPage, error) {
	if q.Sort == "" {
		q.Sort = "started"
	}
	var order string
	for _, o := range runsOrder {
		if o.key == q.Sort {
			order = o.expr
		}
	}
	if order == "" {
		return nil, fmt.Errorf("unknown sort %q", q.Sort)
	}
	dir := "ASC"
	if q.Desc {
		dir = "DESC"
	}
	order += " " + dir + " NULLS LAST, x.began DESC NULLS LAST, x.id DESC"

	args := f.args(q.Workflow, q.Branch, q.Outcome, q.Search, nullTime(q.BucketStart), nullTime(q.BucketEnd))
	page := &RunsPage{}
	err := s.Pool.QueryRow(ctx, `
		WITH`+windowed+runsFiltered+`
		SELECT count(*) FROM rf WHERE `+inBucket, args...).Scan(&page.Total)
	if err != nil {
		return nil, err
	}
	page.Offset = max(0, min(q.Offset, (page.Total-1)/RunsPageSize*RunsPageSize))
	page.Offset -= page.Offset % RunsPageSize

	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+runsFiltered+`,
		page AS (
			SELECT x.* FROM rf x WHERE `+inBucket+`
			ORDER BY `+order+`
			LIMIT $11 OFFSET $12
		)
		SELECT x.id, x.repository, x.workflow, x.path, x.name, x.branch, x.status, x.conclusion,
			x.started_at, x.completed_at,
			(SELECT count(DISTINCT m.ts) FROM samples m WHERE m.job_id = x.id), x.from_artifact,
			x.run_id, x.run_attempt, x.event, x.head_sha, x.queue, x.outcome
		FROM page x
		ORDER BY `+order, append(args, RunsPageSize, page.Offset)...)
	if err != nil {
		return nil, err
	}
	page.Rows, err = pgx.CollectRows(rows, pgx.RowToStructByPos[RunRow])
	return page, err
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
