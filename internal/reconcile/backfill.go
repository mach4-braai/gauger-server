package reconcile

import (
	"context"
	"strings"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
)

// BackfillWindow bounds the date range of one created query, keeping its
// result count under GitHub's cap of 1,000 runs per query.
const BackfillWindow = 7 * 24 * time.Hour

// backfillTask lists a repository's completed runs from GitHub back to the
// task key's lookback date and queues a run task for each one's latest
// attempt. Runs still in progress are left for their own webhook or repair.
func (r *Reconciler) backfillTask(ctx context.Context, t store.Task) (time.Duration, error) {
	dateStr, _, _ := strings.Cut(t.Key, ":")
	since, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return 0, nil
	}
	inst, err := r.Installation(ctx, t.Repository)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	for from := since; from.Before(now); from = from.Add(BackfillWindow) {
		to := from.Add(BackfillWindow - 24*time.Hour)
		if to.After(now) {
			to = now
		}
		runs, err := r.GitHub.ListRuns(ctx, inst, t.Repository, from, to)
		if err != nil {
			return 0, err
		}
		for i := range runs {
			if runs[i].Status != "completed" {
				continue
			}
			key := store.RunKey(runs[i].ID, runs[i].RunAttempt)
			if err := store.EnqueueTask(ctx, r.Store.Pool, store.KindRun, key, t.Repository,
				time.Now(), time.Now().Add(TaskLifetime)); err != nil {
				return 0, err
			}
		}
	}
	return 0, nil
}
