package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// windowed starts every dashboard query. fr holds the run attempts and fj
// the jobs inside a Filter's window, repository and event. A run belongs
// to the window it started in, a job to the one it started, was created or
// was first seen by gauger in. Pass Filter.args as $1 to $4; a query's own
// parameters start at $5.
const windowed = `
		fr AS (
			SELECT r.* FROM runs r
			WHERE coalesce(r.run_started_at, r.created_at) >= $1 AND coalesce(r.run_started_at, r.created_at) < $2
			  AND ($3 = '' OR r.repository = $3) AND ($4 = '' OR r.event = $4)
		),
		fj AS (
			SELECT j.* FROM jobs j
			LEFT JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
			WHERE coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1
			  AND coalesce(j.started_at, j.created_at, j.runner_seen_at) < $2
			  AND ($3 = '' OR j.repository = $3) AND ($4 = '' OR r.event = $4)
		)`

// args returns the filter as windowed's parameters followed by more.
func (f Filter) args(more ...any) []any {
	return append([]any{f.Since, f.Until, f.Repository, f.Event}, more...)
}

// Events returns the distinct events that triggered a run.
func (s *Store) Events(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT event FROM runs WHERE event <> '' ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
