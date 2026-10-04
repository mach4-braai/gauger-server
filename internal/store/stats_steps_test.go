package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

// stepSpec is one step of a job built by storeSteps.
type stepSpec struct {
	name       string
	d          time.Duration
	conclusion string
}

// storeSteps stores a completed job whose steps run back to back from
// start, numbered from 1.
func storeSteps(t *testing.T, st *store.Store, id int64, repo string, start time.Time, steps ...stepSpec) {
	t.Helper()
	ctx := context.Background()
	j := &github.Job{ID: id, RunID: id, RunAttempt: 1, WorkflowName: "CI", Name: "build", HeadBranch: "main",
		Status: "completed", Conclusion: "success", StartedAt: &start}
	at := start
	for i, s := range steps {
		from, to := at, at.Add(s.d)
		step := github.Step{Number: i + 1, Name: s.name, Status: "completed", Conclusion: cmpOr(s.conclusion, "success")}
		if s.conclusion != "skipped" {
			step.StartedAt, step.CompletedAt = &from, &to
			at = to
		}
		j.Steps = append(j.Steps, step)
	}
	j.CompletedAt = &at
	if err := st.InTx(ctx, func(tx pgx.Tx) error { return store.UpsertJob(ctx, tx, repo, j) }); err != nil {
		t.Fatal(err)
	}
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func stepFilter(since time.Time) store.Filter {
	return store.Filter{Since: since.Add(-time.Hour), Until: since.Add(24 * time.Hour)}
}

func stepByName(t *testing.T, ss *store.StepStats, name string) store.StepStat {
	t.Helper()
	for _, s := range ss.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in %+v", name, ss.Steps)
	return store.StepStat{}
}

func TestStepsStripTheRefFromActionSteps(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	start := time.Now().Add(-2 * time.Hour)
	storeSteps(t, st, 1, "acme/app", start,
		stepSpec{name: "jdx/mise-action@aaa", d: 2 * time.Minute},
		stepSpec{name: "Run jdx/mise-action@9e7f3c", d: 3 * time.Minute},
		stepSpec{name: "Post Run actions/checkout@v4", d: time.Second},
		stepSpec{name: "Run make@all", d: 10 * time.Second},
		stepSpec{name: "Email me@example.com", d: 10 * time.Second})
	storeSteps(t, st, 2, "acme/app", start.Add(time.Hour),
		stepSpec{name: "jdx/mise-action@bbb", d: 4 * time.Minute},
		stepSpec{name: "Run jdx/mise-action@7e36aa", d: 5 * time.Minute})

	ss, err := st.Steps(context.Background(), stepFilter(start), "day")
	if err != nil {
		t.Fatal(err)
	}
	if m := stepByName(t, ss, "jdx/mise-action"); m.Runs != 2 || m.Seconds != 360 {
		t.Errorf("jdx/mise-action = %d runs, %.0fs; want @aaa and @bbb as one row of 2 runs, 360s", m.Runs, m.Seconds)
	}
	if m := stepByName(t, ss, "Run jdx/mise-action"); m.Runs != 2 || m.Seconds != 480 {
		t.Errorf("Run jdx/mise-action = %d runs, %.0fs; want the two refs as one row of 2 runs, 480s", m.Runs, m.Seconds)
	}
	stepByName(t, ss, "Post Run actions/checkout")
	stepByName(t, ss, "Run make@all")
	stepByName(t, ss, "Email me@example.com")
	if len(ss.Steps) != 5 {
		t.Errorf("steps = %+v, want 5 rows", ss.Steps)
	}
}

func TestStepsPercentilesMatchPercentileCont(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	ctx := context.Background()
	s := &seed{t: t, st: st}
	base := time.Now().Add(-24 * time.Hour)
	var slowest int64
	for i, secs := range []int{10, 20, 30, 40, 100} {
		id := s.job("acme/app", "main", nil, base.Add(time.Duration(i)*time.Hour), map[string]time.Duration{"make": time.Duration(secs) * time.Second})
		if secs == 100 {
			slowest = id
		}
	}
	s.job("acme/other", "main", nil, base, map[string]time.Duration{"make": time.Hour})

	f := stepFilter(base)
	f.Repository = "acme/app"
	ss, err := st.Steps(ctx, f, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss.Steps) != 1 {
		t.Fatalf("steps = %+v, want make only", ss.Steps)
	}
	got := ss.Steps[0]
	var p50, p95 float64
	err = st.Pool.QueryRow(ctx, `
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM s.completed_at - s.started_at)),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM s.completed_at - s.started_at))
		FROM steps s JOIN jobs j ON j.id = s.job_id
		WHERE s.name = 'make' AND j.repository = 'acme/app'`).Scan(&p50, &p95)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runs != 5 || got.P50 != p50 || got.P95 != p95 || got.P50 != 30 || math.Abs(got.P95-88) > 1e-9 {
		t.Fatalf("make = %+v, want 5 runs with p50 %v and p95 %v (30s and 88s)", got, p50, p95)
	}
	if got.Seconds != 200 || got.SlowestSeconds != 100 || got.JobID != slowest || got.StepNumber != 1 {
		t.Fatalf("make = %+v, want 200s in total and the slowest occurrence job %d step 1 at 100s", got, slowest)
	}
}

func TestStepsSlowestTieBreakPicksOneOccurrence(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour)
	end := start.Add(15 * time.Second)
	for _, j := range []*github.Job{
		numberedJob(9001, "acme/app", "main", "CI", "build", start, end, 3, "deploy"),
		numberedJob(9002, "acme/app", "main", "CI", "build", start, end, 7, "deploy"),
	} {
		if err := st.InTx(ctx, func(tx pgx.Tx) error { return store.UpsertJob(ctx, tx, "acme/app", j) }); err != nil {
			t.Fatal(err)
		}
	}

	ss, err := st.Steps(ctx, stepFilter(start), "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss.Steps) != 1 {
		t.Fatalf("steps = %+v, want one row for deploy", ss.Steps)
	}
	r := ss.Steps[0]
	if (r.JobID != 9001 || r.StepNumber != 3) && (r.JobID != 9002 || r.StepNumber != 7) {
		t.Fatalf("slowest job %d step %d, want a job and step from the same occurrence", r.JobID, r.StepNumber)
	}
}

func TestStepsSetupAndWorkAddUpToTheTotal(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	start := time.Now().UTC().Truncate(time.Hour).Add(-4*time.Hour + 10*time.Minute)
	for i := range 3 {
		storeSteps(t, st, int64(10+i), "acme/app", start.Add(time.Duration(i)*time.Hour),
			stepSpec{name: "Set up job", d: 2 * time.Second},
			stepSpec{name: "Run actions/checkout@" + string(rune('a'+i)), d: 3 * time.Second},
			stepSpec{name: "Run jdx/mise-action@" + string(rune('a'+i)), d: 15 * time.Second},
			stepSpec{name: "Run mise run check", d: time.Duration(60+i) * time.Second, conclusion: []string{"success", "failure", "cancelled"}[i]},
			stepSpec{name: "Post Run actions/checkout@" + string(rune('a'+i)), d: time.Second},
			stepSpec{name: "Run skipped thing", d: time.Minute, conclusion: "skipped"},
			stepSpec{name: "Complete job", d: time.Second})
	}

	ss, err := st.Steps(context.Background(), stepFilter(start), "hour")
	if err != nil {
		t.Fatal(err)
	}
	var total, setup, work, bucketed float64
	for _, s := range ss.Steps {
		total += s.Seconds
		if s.Setup {
			setup += s.Seconds
		} else {
			work += s.Seconds
		}
	}
	for _, b := range ss.Buckets {
		bucketed += b.Seconds
	}
	var want float64
	err = st.Pool.QueryRow(context.Background(),
		`SELECT sum(extract(epoch FROM completed_at - started_at)) FROM steps WHERE started_at IS NOT NULL AND completed_at IS NOT NULL`).Scan(&want)
	if err != nil {
		t.Fatal(err)
	}
	if total != want || setup+work != total || bucketed != total {
		t.Fatalf("total %.0fs, setup %.0fs + work %.0fs, buckets %.0fs; want all equal to %.0fs", total, setup, work, bucketed, want)
	}
	if setup != 3*(2+3+15+1+1) || work != 60+61+62 {
		t.Fatalf("setup %.0fs and work %.0fs, want %d and %d", setup, work, 3*22, 60+61+62)
	}
	m := stepByName(t, ss, "Run mise run check")
	if m.Runs != 3 || m.Failures != 1 {
		t.Fatalf("Run mise run check = %+v, want 3 runs and 1 failure with the skipped step left out", m)
	}
	for _, s := range ss.Steps {
		if s.Name == "Run skipped thing" {
			t.Fatalf("skipped step counted: %+v", s)
		}
	}
	if len(ss.Buckets) != 3*6 {
		t.Errorf("buckets = %d, want 6 names in each of 3 hours", len(ss.Buckets))
	}
}

func TestStepsOnlyCountTheWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	now := time.Now()
	storeSteps(t, st, 1, "acme/app", now.Add(-time.Hour), stepSpec{name: "Run make", d: time.Minute})
	storeSteps(t, st, 2, "acme/app", now.Add(-10*24*time.Hour), stepSpec{name: "Run make", d: time.Hour})
	storeSteps(t, st, 3, "acme/web", now.Add(-time.Hour), stepSpec{name: "Run make", d: time.Hour})

	ss, err := st.Steps(context.Background(), store.Filter{Repository: "acme/app", Since: now.Add(-24 * time.Hour), Until: now}, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(ss.Steps) != 1 || ss.Steps[0].Runs != 1 || ss.Steps[0].Seconds != 60 {
		t.Fatalf("steps = %+v, want one run of 60s in acme/app in the last day", ss.Steps)
	}
	none, err := st.Steps(context.Background(), store.Filter{Since: now.Add(24 * time.Hour), Until: now.Add(48 * time.Hour)}, "day")
	if err != nil || len(none.Steps) != 0 || len(none.Buckets) != 0 {
		t.Fatalf("steps in an empty window = %+v, %v; want none", none, err)
	}
}

func TestIsSetupStep(t *testing.T) {
	for name, want := range map[string]bool{
		"Set up job":                     true,
		"Complete job":                   true,
		"Post Run actions/checkout":      true,
		"Post Run jdx/mise-action":       true,
		"Post cache":                     true,
		"Run actions/checkout":           true,
		"actions/checkout":               true,
		"Run actions/cache/restore":      true,
		"Run actions/cache":              true,
		"Run jdx/mise-action":            true,
		"jdx/mise-action":                true,
		"Run mise run check":             false,
		"Run actions/setup-go":           false,
		"Wait for the late OIDC fetches": false,
		"Perform CodeQL Analysis":        false,
		"Run actions/cache/save":         false,
	} {
		if got := store.IsSetupStep(name); got != want {
			t.Errorf("IsSetupStep(%q) = %v, want %v", name, got, want)
		}
	}
}
