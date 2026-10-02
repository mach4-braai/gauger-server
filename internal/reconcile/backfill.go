package reconcile

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
)

// BackfillWindow bounds the date range of one created query. Most
// repositories' weekly run count stays well under GitHub's cap of 1,000 runs
// per query; listRunsInRange splits further when one doesn't.
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
	expires := now.Add(TaskLifetime)
	for from := since; from.Before(now); from = from.Add(BackfillWindow) {
		to := from.Add(BackfillWindow - 24*time.Hour)
		if to.After(now) {
			to = now
		}
		runs, err := r.listRunsInRange(ctx, inst, t.Repository, from, to)
		if err != nil {
			return 0, err
		}
		for i := range runs {
			if runs[i].Status != "completed" {
				continue
			}
			key := store.RunKey(runs[i].ID, runs[i].RunAttempt)
			if err := store.EnqueueTask(ctx, r.Store.Pool, store.KindRun, key, t.Repository, now, expires); err != nil {
				return 0, err
			}
		}
	}
	return 0, nil
}

// listRunsInRange lists runs in [from, to], splitting the range in half by
// day when GitHub's total_count for it is over github.MaxRunsPerQuery, down
// to a single day. A single day that alone exceeds the cap is logged and
// used as returned: GitHub never returns more than github.MaxRunsPerQuery
// runs for one query no matter how the range is split further.
func (r *Reconciler) listRunsInRange(ctx context.Context, inst int64, repo string, from, to time.Time) ([]github.Run, error) {
	runs, total, err := r.GitHub.ListRuns(ctx, inst, repo, from, to)
	if err != nil {
		return nil, err
	}
	if total <= github.MaxRunsPerQuery {
		return runs, nil
	}
	days := int(to.Sub(from).Hours()/24) + 1
	if days <= 1 {
		slog.Warn("backfill day exceeds GitHub's run cap; some runs were skipped",
			"repository", repo, "date", from.Format("2006-01-02"), "total_count", total)
		return runs, nil
	}
	mid := from.AddDate(0, 0, days/2)
	first, err := r.listRunsInRange(ctx, inst, repo, from, mid.AddDate(0, 0, -1))
	if err != nil {
		return nil, err
	}
	second, err := r.listRunsInRange(ctx, inst, repo, mid, to)
	if err != nil {
		return nil, err
	}
	return append(first, second...), nil
}
