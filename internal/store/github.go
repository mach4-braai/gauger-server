package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mach4-braai/gauger-server/internal/github"
)

// Execer is a pool or a transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, fn)
}

// Credentials implements github.CredentialSource from the App the manifest
// flow stored.
func (s *Store) Credentials(ctx context.Context) (*github.Credentials, error) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if s.creds != nil {
		return s.creds, nil
	}
	var (
		c   github.Credentials
		pem string
	)
	err := s.Pool.QueryRow(ctx, `SELECT app_id, slug, html_url, webhook_secret, private_key FROM app_config WHERE id = 1`).
		Scan(&c.AppID, &c.Slug, &c.HTMLURL, &c.WebhookSecret, &pem)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, github.ErrNotConfigured
	}
	if err != nil {
		return nil, err
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pem))
	if err != nil {
		return nil, fmt.Errorf("stored App private key: %w", err)
	}
	c.PrivateKey = key
	s.creds = &c
	return s.creds, nil
}

var ErrAppExists = errors.New("a GitHub App is already configured")

// SaveApp stores the App the manifest flow created.
func (s *Store) SaveApp(ctx context.Context, a *github.AppConversion) error {
	if _, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(a.PEM)); err != nil {
		return fmt.Errorf("App private key: %w", err)
	}
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO app_config (app_id, slug, html_url, client_id, client_secret, webhook_secret, private_key)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO NOTHING`,
		a.ID, a.Slug, a.HTMLURL, a.ClientID, a.ClientSecret, a.WebhookSecret, a.PEM)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAppExists
	}
	s.credMu.Lock()
	s.creds = nil
	s.credMu.Unlock()
	return nil
}

// RecordDelivery returns false when the delivery ID was already seen.
func RecordDelivery(ctx context.Context, tx pgx.Tx, id, event string) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (id, event) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, event)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) PruneDeliveries(ctx context.Context, before time.Time) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE received_at < $1`, before)
	return err
}

// UpsertRepository records visibility and installation when known.
func UpsertRepository(ctx context.Context, tx pgx.Tx, full string, private *bool, installation int64) error {
	var inst *int64
	if installation != 0 {
		inst = &installation
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO repositories (full_name, private, installation_id) VALUES ($1, $2, $3)
		ON CONFLICT (full_name) DO UPDATE SET
			private = COALESCE(EXCLUDED.private, repositories.private),
			installation_id = COALESCE(EXCLUDED.installation_id, repositories.installation_id),
			updated_at = now()`,
		full, private, inst)
	return err
}

// Installation returns the stored installation for repo, or 0.
func (s *Store) Installation(ctx context.Context, repo string) (int64, error) {
	var id *int64
	err := s.Pool.QueryRow(ctx, `SELECT installation_id FROM repositories WHERE full_name = $1`, repo).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || id == nil {
		return 0, nil
	}
	return *id, err
}

func (s *Store) SetInstallation(ctx context.Context, repo string, installation int64) error {
	return s.InTx(ctx, func(tx pgx.Tx) error { return UpsertRepository(ctx, tx, repo, nil, installation) })
}

// InstalledRepositories returns every repository the App is installed on.
func (s *Store) InstalledRepositories(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT full_name FROM repositories WHERE installation_id IS NOT NULL ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// statusRank orders GitHub statuses so an older event never moves a run or
// job backwards.
func statusRank(s string) int {
	switch s {
	case "completed":
		return 3
	case "in_progress":
		return 2
	case "pending", "":
		return 0
	default:
		return 1
	}
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// UpsertRun stores a run attempt. It returns the stored status.
func UpsertRun(ctx context.Context, tx pgx.Tx, repo string, r *github.Run) (string, error) {
	var prev string
	err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 AND attempt = $2 FOR UPDATE`, r.ID, r.RunAttempt).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	wins := errors.Is(err, pgx.ErrNoRows) || statusRank(r.Status) >= statusRank(prev)
	var status string
	err = tx.QueryRow(ctx, `
		INSERT INTO runs (id, attempt, repository, workflow_name, head_branch, head_sha, event, status, conclusion, html_url, created_at, run_started_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (id, attempt) DO UPDATE SET
			repository     = EXCLUDED.repository,
			workflow_name  = COALESCE(EXCLUDED.workflow_name, runs.workflow_name),
			head_branch    = COALESCE(EXCLUDED.head_branch, runs.head_branch),
			head_sha       = COALESCE(EXCLUDED.head_sha, runs.head_sha),
			event          = COALESCE(EXCLUDED.event, runs.event),
			html_url       = COALESCE(EXCLUDED.html_url, runs.html_url),
			created_at     = COALESCE(runs.created_at, EXCLUDED.created_at),
			run_started_at = COALESCE(EXCLUDED.run_started_at, runs.run_started_at),
			status         = CASE WHEN $14 THEN EXCLUDED.status ELSE runs.status END,
			conclusion     = CASE WHEN $14 THEN EXCLUDED.conclusion ELSE runs.conclusion END,
			updated_at     = CASE WHEN $14 THEN COALESCE(EXCLUDED.updated_at, runs.updated_at) ELSE runs.updated_at END
		RETURNING status`,
		r.ID, r.RunAttempt, repo, nullIfEmpty(r.Name), nullIfEmpty(r.HeadBranch), nullIfEmpty(r.HeadSHA),
		nullIfEmpty(r.Event), r.Status, nullIfEmpty(r.Conclusion), nullIfEmpty(r.HTMLURL),
		r.CreatedAt, r.RunStartedAt, r.UpdatedAt, wins).Scan(&status)
	return status, err
}

// UpsertJob stores a job and, when it is at least as far along as what is
// stored, its steps.
func UpsertJob(ctx context.Context, tx pgx.Tx, repo string, j *github.Job) error {
	var prev string
	err := tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, j.ID).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	wins := errors.Is(err, pgx.ErrNoRows) || statusRank(j.Status) >= statusRank(prev)
	labels := j.Labels
	if labels == nil {
		labels = []string{}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO jobs (id, run_id, run_attempt, repository, workflow_name, name, head_branch, status, conclusion,
			labels, runner_name, runner_group_name, html_url, created_at, started_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (id) DO UPDATE SET
			run_id            = EXCLUDED.run_id,
			run_attempt       = EXCLUDED.run_attempt,
			repository        = EXCLUDED.repository,
			workflow_name     = COALESCE(EXCLUDED.workflow_name, jobs.workflow_name),
			name              = COALESCE(EXCLUDED.name, jobs.name),
			head_branch       = COALESCE(EXCLUDED.head_branch, jobs.head_branch),
			labels            = CASE WHEN cardinality(EXCLUDED.labels) > 0 THEN EXCLUDED.labels ELSE jobs.labels END,
			runner_name       = COALESCE(EXCLUDED.runner_name, jobs.runner_name),
			runner_group_name = COALESCE(EXCLUDED.runner_group_name, jobs.runner_group_name),
			html_url          = COALESCE(EXCLUDED.html_url, jobs.html_url),
			created_at        = COALESCE(jobs.created_at, EXCLUDED.created_at),
			status            = CASE WHEN $17 THEN EXCLUDED.status ELSE jobs.status END,
			conclusion        = CASE WHEN $17 THEN EXCLUDED.conclusion ELSE jobs.conclusion END,
			started_at        = CASE WHEN $17 THEN COALESCE(EXCLUDED.started_at, jobs.started_at) ELSE COALESCE(jobs.started_at, EXCLUDED.started_at) END,
			completed_at      = CASE WHEN $17 THEN COALESCE(EXCLUDED.completed_at, jobs.completed_at) ELSE jobs.completed_at END`,
		j.ID, j.RunID, j.RunAttempt, repo, nullIfEmpty(j.WorkflowName), nullIfEmpty(j.Name), nullIfEmpty(j.HeadBranch),
		j.Status, nullIfEmpty(j.Conclusion), labels, nullIfEmpty(j.RunnerName), nullIfEmpty(j.RunnerGroupName),
		nullIfEmpty(j.HTMLURL), j.CreatedAt, j.StartedAt, j.CompletedAt, wins)
	if err != nil {
		return fmt.Errorf("upsert job %d: %w", j.ID, err)
	}
	if err := scheduleFollowups(ctx, tx, j.ID); err != nil {
		return err
	}
	if !wins || len(j.Steps) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, st := range j.Steps {
		b.Queue(`
			INSERT INTO steps (job_id, number, name, status, conclusion, started_at, completed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (job_id, number) DO UPDATE SET
				name         = EXCLUDED.name,
				status       = EXCLUDED.status,
				conclusion   = EXCLUDED.conclusion,
				started_at   = COALESCE(EXCLUDED.started_at, steps.started_at),
				completed_at = COALESCE(EXCLUDED.completed_at, steps.completed_at)`,
			j.ID, st.Number, st.Name, st.Status, nullIfEmpty(st.Conclusion), st.StartedAt, st.CompletedAt)
	}
	return tx.SendBatch(ctx, b).Close()
}

// Task is a unit of persistent background work.
type Task struct {
	Kind       string
	Key        string
	Repository string
	Attempts   int
	ExpiresAt  time.Time
}

// EnqueueTask adds a task, or moves an existing one's next run earlier.
func EnqueueTask(ctx context.Context, q Execer, kind, key, repo string, next, expires time.Time) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tasks (kind, key, repository, next_at, expires_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (kind, key) DO UPDATE SET next_at = LEAST(tasks.next_at, EXCLUDED.next_at)`,
		kind, key, repo, next, expires)
	return err
}

func (s *Store) DueTasks(ctx context.Context, now time.Time, limit int) ([]Task, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT kind, key, repository, attempts, expires_at FROM tasks
		WHERE next_at <= $1 ORDER BY next_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Task])
}

func (s *Store) DeleteTask(ctx context.Context, kind, key string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM tasks WHERE kind = $1 AND key = $2`, kind, key)
	return err
}

func (s *Store) RescheduleTask(ctx context.Context, kind, key string, attempts int, next time.Time, lastErr string) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE tasks SET attempts = $3, next_at = $4, last_error = $5 WHERE kind = $1 AND key = $2`,
		kind, key, attempts, next, nullIfEmpty(lastErr))
	return err
}

func RunKey(runID int64, attempt int) string {
	return strconv.FormatInt(runID, 10) + ":" + strconv.Itoa(attempt)
}

// BackfillKey identifies a repository's backfill task by its lookback start
// date, so re-queueing the same backfill the same day coalesces into one
// task instead of piling up duplicates.
func BackfillKey(repo string, since time.Time) string {
	return since.UTC().Format("2006-01-02") + ":" + repo
}
