package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/github/githubtest"
	"github.com/mach4-braai/gauger-server/internal/store"
)

func TestBackfillQueuesCompletedRunsAcrossPages(t *testing.T) {
	r, gh, st := setup(t)
	ctx := context.Background()
	repo := "acme/app"
	now := time.Now().UTC()

	var wantQueued int
	for i := 1; i <= 150; i++ {
		status := "completed"
		if i%2 == 0 {
			status = "in_progress"
		} else {
			wantQueued++
		}
		gh.Runs[fmt.Sprintf("%d:1", i)] = &github.Run{
			ID: int64(i), RunAttempt: 1, Status: status, CreatedAt: &now,
			Repository: github.Repository{FullName: repo},
		}
	}

	since := now.AddDate(0, 0, -1)
	task := store.Task{Kind: store.KindBackfill, Key: store.BackfillKey(repo, since), Repository: repo}

	if after, err := r.backfillTask(ctx, task); err != nil || after != 0 {
		t.Fatalf("backfillTask = %v, %v, want 0, nil", after, err)
	}

	if calls := gh.CallCount("/repos/acme/app/actions/runs"); calls != 2 {
		t.Fatalf("list calls = %d, want 2 (two pages of 100 runs)", calls)
	}
	var queued int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind = 'run'`).Scan(&queued)
	if queued != wantQueued {
		t.Fatalf("queued run tasks = %d, want %d (completed runs only)", queued, wantQueued)
	}

	// A second backfill over the same window must not queue duplicate run
	// tasks: EnqueueTask upserts on (kind, key).
	if _, err := r.backfillTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	var queuedAgain int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind = 'run'`).Scan(&queuedAgain)
	if queuedAgain != wantQueued {
		t.Fatalf("queued run tasks after second backfill = %d, want %d unchanged", queuedAgain, wantQueued)
	}
}

func TestBackfillBadKeyFinishesWithoutError(t *testing.T) {
	r, _, _ := setup(t)
	ctx := context.Background()
	task := store.Task{Kind: store.KindBackfill, Key: "not-a-date:acme/app", Repository: "acme/app"}
	if after, err := r.backfillTask(ctx, task); err != nil || after != 0 {
		t.Fatalf("backfillTask = %v, %v, want 0, nil", after, err)
	}
}

func TestBackfillTaskRunsThroughProcessDue(t *testing.T) {
	r, gh, st := setup(t)
	ctx := context.Background()
	repo := "acme/app"
	now := time.Now().UTC()
	gh.Runs["500:1"] = &github.Run{ID: 500, RunAttempt: 1, Status: "completed", CreatedAt: &now, Repository: github.Repository{FullName: repo}}
	gh.Runs["501:1"] = &github.Run{ID: 501, RunAttempt: 1, Status: "in_progress", CreatedAt: &now, Repository: github.Repository{FullName: repo}}

	since := now.AddDate(0, 0, -1)
	key := store.BackfillKey(repo, since)
	if err := store.EnqueueTask(ctx, st.Pool, store.KindBackfill, key, repo, time.Now().Add(-time.Second), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	r.ProcessDue(ctx)

	var backfillLeft int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind = 'backfill'`).Scan(&backfillLeft)
	if backfillLeft != 0 {
		t.Fatalf("backfill tasks left = %d, want 0 (finished)", backfillLeft)
	}
	var runQueued int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind = 'run' AND key = '500:1'`).Scan(&runQueued)
	if runQueued != 1 {
		t.Fatalf("run task for completed run = %d, want 1", runQueued)
	}
	var inProgressQueued int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind = 'run' AND key = '501:1'`).Scan(&inProgressQueued)
	if inProgressQueued != 0 {
		t.Fatalf("run task for in-progress run = %d, want 0", inProgressQueued)
	}
}

func TestBackfillSplitsWindowOverGitHubRunCap(t *testing.T) {
	r, gh, _ := setup(t)
	ctx := context.Background()
	repo := "acme/busy"
	today := time.Now().UTC()
	yesterday := today.AddDate(0, 0, -1)

	seed := func(day time.Time, startID int64, n int) {
		for i := range n {
			id := startID + int64(i)
			created := day
			gh.Runs[fmt.Sprintf("%d:1", id)] = &github.Run{
				ID: id, RunAttempt: 1, Status: "completed", CreatedAt: &created,
				Repository: github.Repository{FullName: repo},
			}
		}
	}
	seed(yesterday, 1, 520)
	seed(today, 10000, 520)

	runs, err := r.listRunsInRange(ctx, githubtest.Installation, repo, yesterday, today)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1040 {
		t.Fatalf("runs = %d, want 1040 (splitting must not drop any)", len(runs))
	}
	// One short-circuited whole-range probe (total over the cap, so it
	// stops after page 1) plus a full paginated listing per day once split.
	if calls := gh.CallCount("/repos/acme/busy/actions/runs"); calls < 3 {
		t.Fatalf("list calls = %d, want at least 3 (probe + per-day split)", calls)
	}
}

func TestBackfillSingleDayOverCapLogsAndContinues(t *testing.T) {
	r, gh, _ := setup(t)
	ctx := context.Background()
	repo := "acme/busiest"
	today := time.Now().UTC()
	for i := range 1100 {
		id := int64(20000 + i)
		created := today
		gh.Runs[fmt.Sprintf("%d:1", id)] = &github.Run{
			ID: id, RunAttempt: 1, Status: "completed", CreatedAt: &created,
			Repository: github.Repository{FullName: repo},
		}
	}

	runs, err := r.listRunsInRange(ctx, githubtest.Installation, repo, today, today)
	if err != nil {
		t.Fatal(err)
	}
	// A single day over the cap can't be split further, so listRunsInRange
	// logs a warning and uses whatever ListRuns already fetched: one page,
	// since ListRuns itself stops once total_count is over the cap.
	if len(runs) != 100 {
		t.Fatalf("runs = %d, want 100 (one page, logged and used as-is)", len(runs))
	}
}
