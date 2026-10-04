package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SpendRow is the job minutes of one job name in one bucket, on one set of
// runner labels in repositories of one visibility. Each job is rounded up
// to a whole minute. Only completed jobs that started in the window count.
// Pricing is left to the caller, which holds the rates.
type SpendRow struct {
	Bucket     time.Time
	Repository string
	Workflow   string
	Path       string
	Job        string
	Labels     []string
	Private    *bool
	Jobs       int64
	Minutes    int64
}

// SpendRows returns the window's job minutes per UTC bucket ("hour" or
// "day"), repository, workflow, job name and runner labels. Summing them
// per repository, workflow or job gives that table's totals.
func (s *Store) SpendRows(ctx context.Context, f Filter, bucket string) ([]SpendRow, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`,`+jobWorkflows+`
		SELECT date_trunc($5, fj.started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
			fj.repository, jw.workflow, jw.path, coalesce(fj.name, ''), fj.labels, rp.private,
			count(*), sum(ceil(extract(epoch FROM fj.completed_at - fj.started_at) / 60))::bigint
		FROM fj
		JOIN jw ON jw.id = fj.id
		LEFT JOIN repositories rp ON rp.full_name = fj.repository
		WHERE fj.status = 'completed' AND fj.started_at IS NOT NULL AND fj.completed_at > fj.started_at
		GROUP BY 1, 2, 3, 4, 5, 6, 7
		ORDER BY 1, 2, 3, 5`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SpendRow])
}

// SpendMonths sums the window's job minutes per repository, UTC month of
// completion and runner labels, newest month first. It groups like
// SpendGroups, within the dashboard's window.
func (s *Store) SpendMonths(ctx context.Context, f Filter) ([]SpendGroup, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT fj.repository, date_trunc('month', fj.completed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
			fj.labels, rp.private,
			count(*), sum(ceil(extract(epoch FROM fj.completed_at - fj.started_at) / 60))::bigint
		FROM fj
		LEFT JOIN repositories rp ON rp.full_name = fj.repository
		WHERE fj.status = 'completed' AND fj.started_at IS NOT NULL AND fj.completed_at > fj.started_at
		GROUP BY 1, 2, 3, 4
		ORDER BY 2 DESC, 1, 3`, f.args()...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SpendGroup])
}
