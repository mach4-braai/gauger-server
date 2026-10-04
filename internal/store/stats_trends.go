package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// StepDay is the median duration of one step over the successful runs that
// started on a UTC day.
type StepDay struct {
	Day    time.Time
	Median float64
	Runs   int64
}

// StepDaily returns a step's median duration per UTC day from since on,
// over the same occurrences Regressions groups: successful steps of one
// repository, workflow, job and branch. An empty branch matches jobs
// without one.
func (s *Store) StepDaily(ctx context.Context, since time.Time, repo, workflow, job, step, branch string) ([]StepDay, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT date_trunc('day', s.started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM s.completed_at - s.started_at)), count(*)
		FROM steps s JOIN jobs j ON j.id = s.job_id
		WHERE s.conclusion = 'success' AND s.completed_at IS NOT NULL AND s.started_at >= $1
		  AND j.repository = $2 AND coalesce(j.workflow_name, '') = $3 AND coalesce(j.name, '') = $4
		  AND s.name = $5 AND coalesce(j.head_branch, '') = $6
		GROUP BY 1
		ORDER BY 1`, since, repo, workflow, job, step, branch)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[StepDay])
}
