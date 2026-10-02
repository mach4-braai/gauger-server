package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
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
