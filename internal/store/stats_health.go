package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeliveryRetention is how far back webhook_deliveries reaches; the
// maintenance loop prunes older rows.
const DeliveryRetention = deliveryDedupWindow

// DeliveryCount is the number of webhook deliveries of one event type
// that arrived in one bucket.
type DeliveryCount struct {
	Bucket     time.Time
	Event      string
	Deliveries int64
}

// DeliveriesByEvent counts the deliveries received in [since, until) per
// UTC bucket ("hour" or "day") and event type. Deliveries carry no
// repository, so the dashboard's repository and event filters don't apply.
func (s *Store) DeliveriesByEvent(ctx context.Context, since, until time.Time, bucket string) ([]DeliveryCount, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT date_trunc($3, received_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', event, count(*)
		FROM webhook_deliveries
		WHERE received_at >= $1 AND received_at < $2
		GROUP BY 1, 2
		ORDER BY 1, 2`, since, until, bucket)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DeliveryCount])
}

// TaskKind summarises the queued tasks of one kind. Oldest is the
// earliest next_at, which is in the past for tasks that are due.
// LastError is the error of the task retried most recently, nil when no
// task of the kind has failed.
type TaskKind struct {
	Kind        string
	Queued      int64
	Due         int64
	Oldest      time.Time
	MaxAttempts int32
	LastError   *string
}

// TaskQueue summarises the reconciler's queue per task kind, as of now.
// Rows leave the queue when they finish, so this is a snapshot.
func (s *Store) TaskQueue(ctx context.Context, now time.Time) ([]TaskKind, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT t.kind, count(*), count(*) FILTER (WHERE t.next_at <= $1), min(t.next_at), max(t.attempts),
			(SELECT e.last_error FROM tasks e
			 WHERE e.kind = t.kind AND e.last_error IS NOT NULL
			 ORDER BY e.next_at DESC, e.key LIMIT 1)
		FROM tasks t
		GROUP BY t.kind
		ORDER BY count(*) DESC, t.kind`, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[TaskKind])
}

// CoverageCount is the number of jobs that started in one bucket and
// whose gauger samples, if any, came the same way. Source is "live" for
// samples gauger reported, "artifact" for samples read from the fallback
// artifact and "none" for jobs without samples.
type CoverageCount struct {
	Bucket time.Time
	Source string
	Jobs   int64
}

// CoverageByBucket counts the completed jobs in the window per UTC
// bucket ("hour" or "day") and the source of their samples.
func (s *Store) CoverageByBucket(ctx context.Context, f Filter, bucket string) ([]CoverageCount, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT date_trunc($5, coalesce(started_at, created_at, runner_seen_at) AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
			CASE
				WHEN NOT EXISTS (SELECT 1 FROM samples m WHERE m.job_id = fj.id) THEN 'none'
				WHEN artifact_ingested_at IS NOT NULL THEN 'artifact'
				ELSE 'live'
			END, count(*)
		FROM fj
		WHERE status = 'completed'
		GROUP BY 1, 2
		ORDER BY 1, 2`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[CoverageCount])
}
