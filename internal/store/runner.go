package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/runner"
)

const (
	KindRun      = "run"
	KindJob      = "job"
	KindArtifact = "artifact"
	KindBackfill = "backfill"
)

// ArtifactGrace is how long after a job completes the fallback artifact
// search starts, giving gauger's final flush and done time to land.
const ArtifactGrace = 2 * time.Minute

// scheduleFollowups adds the tasks a runner-reported job needs: a REST poll
// until it completes, then an artifact search if gauger never said done.
func scheduleFollowups(ctx context.Context, tx pgx.Tx, jobID int64) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO tasks (kind, key, repository, next_at, expires_at)
		SELECT $2, id::text, repository, now() + interval '30 seconds', now() + interval '7 days'
		FROM jobs WHERE id = $1 AND runner_seen_at IS NOT NULL AND status <> 'completed'
		ON CONFLICT DO NOTHING`, jobID, KindJob)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO tasks (kind, key, repository, next_at, expires_at)
		SELECT $2, id::text, repository, completed_at + $3 * interval '1 second', completed_at + interval '7 days'
		FROM jobs
		WHERE id = $1 AND status = 'completed' AND completed_at IS NOT NULL
		  AND runner_seen_at IS NOT NULL AND runner_done_at IS NULL AND artifact_ingested_at IS NULL
		ON CONFLICT DO NOTHING`, jobID, KindArtifact, int(ArtifactGrace.Seconds()))
	return err
}

// QueueArtifactSearches adds an artifact search for each completed job of
// the run attempt that gauger never said done for, that has no ingested
// artifact and whose artifact has not expired yet.
func (s *Store) QueueArtifactSearches(ctx context.Context, runID int64, attempt int) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO tasks (kind, key, repository, next_at, expires_at)
		SELECT $1, id::text, repository, greatest(now(), completed_at + $4 * interval '1 second'), completed_at + interval '7 days'
		FROM jobs
		WHERE run_id = $2 AND run_attempt = $3 AND status = 'completed' AND completed_at > now() - interval '7 days'
		  AND runner_done_at IS NULL AND artifact_ingested_at IS NULL
		ON CONFLICT DO NOTHING`, KindArtifact, runID, attempt, int(ArtifactGrace.Seconds()))
	return err
}

// MarkRunnerSeen records that gauger reported on a job, creating it as
// pending when no webhook or REST data has arrived yet.
func (s *Store) MarkRunnerSeen(ctx context.Context, jobID int64, id runner.Identity, done bool) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO jobs (id, run_id, run_attempt, repository, workflow_name, name, runner_name, status, runner_seen_at, runner_done_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', now(), CASE WHEN $8 THEN now() END)
			ON CONFLICT (id) DO UPDATE SET
				runner_seen_at = COALESCE(jobs.runner_seen_at, now()),
				runner_done_at = CASE WHEN $8 THEN COALESCE(jobs.runner_done_at, now()) ELSE jobs.runner_done_at END,
				runner_name    = COALESCE(jobs.runner_name, EXCLUDED.runner_name)`,
			jobID, id.RunID, id.RunAttempt, id.Repository, nullIfEmpty(id.Workflow), nullIfEmpty(id.Job),
			nullIfEmpty(id.RunnerName), done)
		if err != nil {
			return err
		}
		return scheduleFollowups(ctx, tx, jobID)
	})
}

// InsertSamples stores points for a job and ignores ones already stored, so
// replays from gauger's buffer or the artifact add nothing twice. It returns
// how many points fell outside the retention window and were dropped.
func (s *Store) InsertSamples(ctx context.Context, jobID int64, points []runner.Point) (rejected int64, err error) {
	now := time.Now()
	oldest := s.RetentionStart(now)
	newest := now.Add(24 * time.Hour)
	var (
		ts      []time.Time
		metric  []string
		series  []string
		values  []float64
		daysSet = map[time.Time]bool{}
	)
	for _, p := range points {
		if p.Time.Before(oldest) || p.Time.After(newest) {
			rejected++
			continue
		}
		ts = append(ts, p.Time)
		metric = append(metric, p.Metric)
		series = append(series, p.Series)
		values = append(values, p.Value)
		daysSet[dayStart(p.Time)] = true
	}
	for d := range daysSet {
		if err := s.EnsurePartition(ctx, d); err != nil {
			return 0, err
		}
	}
	if len(ts) == 0 {
		return rejected, nil
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO samples (job_id, ts, metric, series, value)
		SELECT $1, * FROM unnest($2::timestamptz[], $3::text[], $4::text[], $5::float8[])
		ON CONFLICT DO NOTHING`, jobID, ts, metric, series, values)
	if err != nil {
		return 0, fmt.Errorf("insert samples: %w", err)
	}
	return rejected, nil
}

// JobState is what the job and artifact tasks need to know about a job.
type JobState struct {
	RunID        int64
	Status       string
	CompletedAt  *time.Time
	RunnerDoneAt *time.Time
}

func (s *Store) JobState(ctx context.Context, jobID int64) (*JobState, error) {
	var j JobState
	err := s.Pool.QueryRow(ctx, `SELECT run_id, status, completed_at, runner_done_at FROM jobs WHERE id = $1`, jobID).
		Scan(&j.RunID, &j.Status, &j.CompletedAt, &j.RunnerDoneAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &j, err
}

func (s *Store) MarkArtifactIngested(ctx context.Context, jobID int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE jobs SET artifact_ingested_at = now() WHERE id = $1`, jobID)
	return err
}

func JobKey(jobID int64) string { return strconv.FormatInt(jobID, 10) }
