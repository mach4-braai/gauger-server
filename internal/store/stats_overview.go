package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Overview holds the dashboard's headline numbers for one window. A run or
// job is decided when it completed with a conclusion other than cancelled
// or skipped; success rates are taken over decided ones. Durations are in
// seconds and nil when nothing in the window has one.
type Overview struct {
	Runs             int64
	ReRuns           int64
	RunsDecided      int64
	RunsSucceeded    int64
	Jobs             int64
	JobsDecided      int64
	JobsSucceeded    int64
	Steps            int64
	RunP50           *float64
	RunP95           *float64
	QueueP50         *float64
	QueueP95         *float64
	JobsWithSamples  int64
	JobsFromArtifact int64
	Minutes          []LabelMinutes
}

// LabelMinutes is the job minutes, each job rounded up to a whole minute,
// run on one set of runner labels in repositories of one visibility.
type LabelMinutes struct {
	Labels  []string
	Private *bool
	Minutes int64
}

// Overview counts runs, jobs and steps in the window. A run lasts from its
// first job's start to its last job's end; queue time is a job's start
// minus its creation.
func (s *Store) Overview(ctx context.Context, f Filter) (*Overview, error) {
	var o Overview
	err := s.Pool.QueryRow(ctx, `
		WITH`+windowed+`,
		rd AS (
			SELECT extract(epoch FROM max(j.completed_at) - min(j.started_at)) AS secs
			FROM fr JOIN jobs j ON j.run_id = fr.id AND j.run_attempt = fr.attempt
			WHERE fr.status = 'completed' AND j.started_at IS NOT NULL AND j.completed_at IS NOT NULL
			GROUP BY fr.id, fr.attempt
		),
		q AS (
			SELECT extract(epoch FROM started_at - created_at) AS secs FROM fj WHERE started_at >= created_at
		)
		SELECT
			(SELECT count(*) FROM fr),
			(SELECT count(*) FROM fr WHERE attempt > 1),
			(SELECT count(*) FROM fr WHERE status = 'completed' AND coalesce(conclusion, '') NOT IN ('', 'cancelled', 'skipped')),
			(SELECT count(*) FROM fr WHERE status = 'completed' AND conclusion = 'success'),
			(SELECT count(*) FROM fj),
			(SELECT count(*) FROM fj WHERE status = 'completed' AND coalesce(conclusion, '') NOT IN ('', 'cancelled', 'skipped')),
			(SELECT count(*) FROM fj WHERE status = 'completed' AND conclusion = 'success'),
			(SELECT count(*) FROM steps s JOIN fj ON fj.id = s.job_id),
			(SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY secs) FROM rd),
			(SELECT percentile_cont(0.95) WITHIN GROUP (ORDER BY secs) FROM rd),
			(SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY secs) FROM q),
			(SELECT percentile_cont(0.95) WITHIN GROUP (ORDER BY secs) FROM q),
			(SELECT count(*) FROM fj WHERE EXISTS (SELECT 1 FROM samples m WHERE m.job_id = fj.id)),
			(SELECT count(*) FROM fj WHERE fj.artifact_ingested_at IS NOT NULL AND EXISTS (SELECT 1 FROM samples m WHERE m.job_id = fj.id))`,
		f.args()...).Scan(
		&o.Runs, &o.ReRuns, &o.RunsDecided, &o.RunsSucceeded,
		&o.Jobs, &o.JobsDecided, &o.JobsSucceeded, &o.Steps,
		&o.RunP50, &o.RunP95, &o.QueueP50, &o.QueueP95,
		&o.JobsWithSamples, &o.JobsFromArtifact)
	if err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT fj.labels, rp.private, sum(ceil(extract(epoch FROM fj.completed_at - fj.started_at) / 60))::bigint
		FROM fj
		LEFT JOIN repositories rp ON rp.full_name = fj.repository
		WHERE fj.status = 'completed' AND fj.started_at IS NOT NULL AND fj.completed_at > fj.started_at
		GROUP BY 1, 2
		ORDER BY 3 DESC`, f.args()...)
	if err != nil {
		return nil, err
	}
	o.Minutes, err = pgx.CollectRows(rows, pgx.RowToStructByPos[LabelMinutes])
	return &o, err
}

// ConclusionCount is the number of runs with one conclusion that started
// in one bucket. Conclusion is empty for runs that have not completed.
type ConclusionCount struct {
	Bucket     time.Time
	Conclusion string
	Runs       int64
}

// RunsByConclusion counts runs per UTC bucket ("hour" or "day") and
// conclusion.
func (s *Store) RunsByConclusion(ctx context.Context, f Filter, bucket string) ([]ConclusionCount, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH`+windowed+`
		SELECT date_trunc($5, coalesce(run_started_at, created_at) AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
			CASE WHEN status = 'completed' THEN coalesce(conclusion, '') ELSE '' END, count(*)
		FROM fr
		GROUP BY 1, 2
		ORDER BY 1, 2`, f.args(bucket)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ConclusionCount])
}
