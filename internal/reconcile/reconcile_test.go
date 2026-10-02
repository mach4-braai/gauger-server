package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/github/githubtest"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

func ts(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

func setup(t *testing.T) (*Reconciler, *githubtest.Server, *store.Store) {
	st := storetest.Open(t, 90*24*time.Hour)
	gh := githubtest.New(t)
	return New(st, github.NewClient(gh.URL, github.StaticCredentials{C: gh.Credentials()})), gh, st
}

func TestRunTaskRepairsMissedJobDeliveries(t *testing.T) {
	r, gh, st := setup(t)
	ctx := context.Background()
	gh.Runs["100:2"] = &github.Run{ID: 100, RunAttempt: 2, Name: "CI", HeadBranch: "main", Status: "completed", Conclusion: "success",
		Repository: github.Repository{FullName: "acme/app", Private: new(false)}}
	for _, id := range []int64{1, 2} {
		gh.Jobs[id] = &github.Job{ID: id, RunID: 100, RunAttempt: 2, Name: "job", Status: "completed", Conclusion: "success",
			StartedAt: ts("2026-09-30T10:00:00Z"), CompletedAt: ts("2026-09-30T10:03:00Z"),
			Steps: []github.Step{{Number: 1, Name: "Run", Status: "completed", Conclusion: "success", StartedAt: ts("2026-09-30T10:00:00Z"), CompletedAt: ts("2026-09-30T10:03:00Z")}}}
	}
	if err := store.EnqueueTask(ctx, st.Pool, store.KindRun, "100:2", "acme/app", time.Now().Add(-time.Second), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	r.ProcessDue(ctx)

	var jobs, steps, tasks int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE run_id = 100 AND status = 'completed'`).Scan(&jobs)
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM steps`).Scan(&steps)
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE kind = 'run'`).Scan(&tasks)
	if jobs != 2 || steps != 2 || tasks != 0 {
		t.Fatalf("jobs=%d steps=%d tasks=%d, want 2 2 0", jobs, steps, tasks)
	}
	var runStatus string
	var private bool
	st.Pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = 100 AND attempt = 2`).Scan(&runStatus)
	st.Pool.QueryRow(ctx, `SELECT private FROM repositories WHERE full_name = 'acme/app'`).Scan(&private)
	if runStatus != "completed" || private {
		t.Fatalf("run status %q private=%v, want completed and public", runStatus, private)
	}
}

func TestUnfinishedRunIsPolledAgainWithBackoff(t *testing.T) {
	r, gh, st := setup(t)
	ctx := context.Background()
	gh.Runs["100:1"] = &github.Run{ID: 100, RunAttempt: 1, Status: "in_progress"}
	store.EnqueueTask(ctx, st.Pool, store.KindRun, "100:1", "acme/app", time.Now().Add(-time.Second), time.Now().Add(time.Hour))

	r.ProcessDue(ctx)

	var attempts int
	var next time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT attempts, next_at FROM tasks WHERE key = '100:1'`).Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || time.Until(next) < 4*time.Minute {
		t.Fatalf("attempts=%d next in %v, want 1 and about 5m", attempts, time.Until(next))
	}
}

func TestRateLimitedTaskWaitsForReset(t *testing.T) {
	r, gh, st := setup(t)
	ctx := context.Background()
	gh.Limited = true
	store.EnqueueTask(ctx, st.Pool, store.KindRun, "100:1", "acme/app", time.Now().Add(-time.Second), time.Now().Add(24*time.Hour))

	r.ProcessDue(ctx)

	var attempts int
	var next time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT attempts, next_at FROM tasks WHERE key = '100:1'`).Scan(&attempts, &next); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || time.Until(next) < 50*time.Minute {
		t.Fatalf("attempts=%d next in %v, want 0 and the reset about an hour out", attempts, time.Until(next))
	}
}

func TestBackoff(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{{0, time.Minute}, {1, 2 * time.Minute}, {3, 8 * time.Minute}, {10, 30 * time.Minute}} {
		if got := backoff(tc.attempts, time.Minute, 30*time.Minute); got != tc.want {
			t.Errorf("backoff(%d) = %v, want %v", tc.attempts, got, tc.want)
		}
	}
}
