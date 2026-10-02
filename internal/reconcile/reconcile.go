// Package reconcile repairs webhook gaps from the GitHub REST API. Its work
// lives in the tasks table, so it survives restarts.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
)

const (
	// RunCheckDelay is how long after an unfinished run event the first REST
	// check happens. Webhooks normally make that check a no-op.
	RunCheckDelay = 10 * time.Minute
	// TaskLifetime bounds how long any task keeps retrying.
	TaskLifetime = 7 * 24 * time.Hour
)

// Handler does one attempt at a task. It returns 0 and nil when the task is
// finished, a positive duration to run it again after that long, or an error
// to retry with backoff.
type Handler func(ctx context.Context, t store.Task) (time.Duration, error)

type Reconciler struct {
	Store  *store.Store
	GitHub *github.Client

	handlers map[string]Handler
	wake     chan struct{}
}

func New(st *store.Store, gh *github.Client) *Reconciler {
	r := &Reconciler{Store: st, GitHub: gh, wake: make(chan struct{}, 1)}
	r.handlers = map[string]Handler{
		store.KindRun:      r.runTask,
		store.KindJob:      r.jobTask,
		store.KindArtifact: r.artifactTask,
	}
	return r
}

// Wake makes the worker look for due tasks now.
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Installation returns the App installation for repo, asking GitHub the
// first time.
func (r *Reconciler) Installation(ctx context.Context, repo string) (int64, error) {
	id, err := r.Store.Installation(ctx, repo)
	if err != nil || id != 0 {
		return id, err
	}
	id, err = r.GitHub.RepoInstallation(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("find installation for %s: %w", repo, err)
	}
	return id, r.Store.SetInstallation(ctx, repo, id)
}

// SyncRunAttempt stores a run attempt and all its jobs from REST, and
// returns the run's status.
func (r *Reconciler) SyncRunAttempt(ctx context.Context, repo string, runID int64, attempt int) (string, error) {
	inst, err := r.Installation(ctx, repo)
	if err != nil {
		return "", err
	}
	run, err := r.GitHub.GetRunAttempt(ctx, inst, repo, runID, attempt)
	if err != nil {
		return "", err
	}
	jobs, err := r.GitHub.ListRunAttemptJobs(ctx, inst, repo, runID, attempt)
	if err != nil {
		return "", err
	}
	var status string
	err = r.Store.InTx(ctx, func(tx pgx.Tx) error {
		if err := store.UpsertRepository(ctx, tx, repo, run.Repository.Private, inst); err != nil {
			return err
		}
		if status, err = store.UpsertRun(ctx, tx, repo, run); err != nil {
			return err
		}
		for i := range jobs {
			if err := store.UpsertJob(ctx, tx, repo, &jobs[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return status, err
}

func (r *Reconciler) runTask(ctx context.Context, t store.Task) (time.Duration, error) {
	idStr, attemptStr, _ := strings.Cut(t.Key, ":")
	runID, err1 := strconv.ParseInt(idStr, 10, 64)
	attempt, err2 := strconv.Atoi(attemptStr)
	if err1 != nil || err2 != nil {
		return 0, nil
	}
	status, err := r.SyncRunAttempt(ctx, t.Repository, runID, attempt)
	if err != nil {
		return 0, err
	}
	if status == "completed" {
		return 0, r.Store.QueueArtifactSearches(ctx, runID, attempt)
	}
	return backoff(t.Attempts, 5*time.Minute, time.Hour), nil
}

// backoff doubles base per attempt up to max.
func backoff(attempts int, base, max time.Duration) time.Duration {
	d := base
	for range attempts {
		d *= 2
		if d >= max {
			return max
		}
	}
	return d
}

// Run processes due tasks until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		r.ProcessDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-r.wake:
		}
	}
}

// ProcessDue runs every task that is due now, one attempt each.
func (r *Reconciler) ProcessDue(ctx context.Context) {
	now := time.Now()
	tasks, err := r.Store.DueTasks(ctx, now, 32)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("load due tasks", "err", err)
		}
		return
	}
	for _, t := range tasks {
		if ctx.Err() != nil {
			return
		}
		r.process(ctx, t, now)
	}
}

func (r *Reconciler) process(ctx context.Context, t store.Task, now time.Time) {
	log := slog.With("kind", t.Kind, "key", t.Key, "repository", t.Repository, "attempts", t.Attempts)
	if now.After(t.ExpiresAt) {
		log.Warn("task expired")
		r.finish(ctx, t)
		return
	}
	h, ok := r.handlers[t.Kind]
	if !ok {
		log.Error("no handler for task kind")
		r.finish(ctx, t)
		return
	}
	tctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	after, err := h(tctx, t)
	cancel()

	var rl *github.RateLimitError
	switch {
	case err == nil && after == 0:
		r.finish(ctx, t)
	case err == nil:
		r.reschedule(ctx, t, t.Attempts+1, now.Add(after), "")
	case errors.As(err, &rl):
		log.Info("rate limited", "until", rl.Until)
		r.reschedule(ctx, t, t.Attempts, rl.Until, err.Error())
	case errors.Is(err, github.ErrNotConfigured):
		r.reschedule(ctx, t, t.Attempts, now.Add(time.Minute), err.Error())
	case github.IsNotFound(err):
		log.Warn("task target not found on GitHub", "err", err)
		r.finish(ctx, t)
	default:
		log.Warn("task failed", "err", err)
		r.reschedule(ctx, t, t.Attempts+1, now.Add(backoff(t.Attempts, 30*time.Second, 30*time.Minute)), err.Error())
	}
}

func (r *Reconciler) finish(ctx context.Context, t store.Task) {
	if err := r.Store.DeleteTask(ctx, t.Kind, t.Key); err != nil {
		slog.Error("delete task", "kind", t.Kind, "key", t.Key, "err", err)
	}
}

func (r *Reconciler) reschedule(ctx context.Context, t store.Task, attempts int, next time.Time, lastErr string) {
	if err := r.Store.RescheduleTask(ctx, t.Kind, t.Key, attempts, next, lastErr); err != nil {
		slog.Error("reschedule task", "kind", t.Kind, "key", t.Key, "err", err)
	}
}
