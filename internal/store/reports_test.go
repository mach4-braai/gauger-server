package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/runner"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

type seed struct {
	t  *testing.T
	st *store.Store
	id int64
}

// job stores a completed job with one step per duration, back to back.
func (s *seed) job(repo, branch string, labels []string, start time.Time, steps map[string]time.Duration) int64 {
	s.t.Helper()
	s.id++
	ctx := context.Background()
	j := &github.Job{ID: s.id, RunID: s.id, RunAttempt: 1, WorkflowName: "CI", Name: "build", HeadBranch: branch,
		Status: "completed", Conclusion: "success", Labels: labels, StartedAt: &start}
	at := start
	n := 0
	for name, d := range steps {
		n++
		from, to := at, at.Add(d)
		j.Steps = append(j.Steps, github.Step{Number: n, Name: name, Status: "completed", Conclusion: "success", StartedAt: &from, CompletedAt: &to})
		at = to
	}
	j.CompletedAt = &at
	err := s.st.InTx(ctx, func(tx pgx.Tx) error { return store.UpsertJob(ctx, tx, repo, j) })
	if err != nil {
		s.t.Fatal(err)
	}
	return s.id
}

func TestSlowStepsPercentiles(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	s := &seed{t: t, st: st}
	base := time.Now().Add(-24 * time.Hour)
	for i, secs := range []int{10, 20, 30, 40, 100} {
		s.job("acme/app", "main", nil, base.Add(time.Duration(i)*time.Hour), map[string]time.Duration{"make": time.Duration(secs) * time.Second})
	}
	s.job("acme/other", "main", nil, base, map[string]time.Duration{"make": time.Hour})

	rows, err := st.SlowSteps(context.Background(), store.Filter{Repository: "acme/app", Since: base.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Runs != 5 || rows[0].P50 != 30 || math.Abs(rows[0].P95-88) > 1e-9 {
		t.Fatalf("slow steps = %+v, want make with 5 runs, p50 30s, p95 88s", rows)
	}
}

func TestRegressionsComparePerBranchWithTheDaysBefore(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	s := &seed{t: t, st: st}
	today := time.Now().UTC().Truncate(24 * time.Hour).Add(time.Hour)
	for d := 1; d <= 5; d++ {
		s.job("acme/app", "main", nil, today.AddDate(0, 0, -d), map[string]time.Duration{"test": 10 * time.Second})
	}
	s.job("acme/app", "main", nil, today, map[string]time.Duration{"test": 20 * time.Second})
	s.job("acme/app", "dev", nil, today.AddDate(0, 0, -1), map[string]time.Duration{"test": 10 * time.Second})
	s.job("acme/app", "dev", nil, today, map[string]time.Duration{"test": 40 * time.Second})

	rows, err := st.Regressions(context.Background(), store.Filter{Since: today.Add(-time.Hour)}, 1.25, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("regressions = %+v, want only main today (dev has too few baseline runs)", rows)
	}
	r := rows[0]
	if r.Branch != "main" || r.Median != 20 || r.Baseline != 10 || r.BaseRuns != 5 || !r.Day.Equal(today.Truncate(24*time.Hour)) {
		t.Fatalf("regression = %+v", r)
	}
}

func TestSizingUsesRunnerSamplesInsideEachStep(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	ctx := context.Background()
	s := &seed{t: t, st: st}
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	id := s.job("acme/app", "main", nil, start, map[string]time.Duration{"compile": time.Minute})

	id0 := runner.Identity{RunID: id, RunAttempt: 1, CheckRunID: id, Repository: "acme/app"}
	pts := []runner.Point{
		{Identity: id0, Metric: store.MetricMemoryLimit, Time: start, Value: 8 << 30},
		{Identity: id0, Metric: store.MetricCPUCount, Time: start, Value: 4},
		{Identity: id0, Metric: store.MetricMemoryUsage, Series: store.SeriesMemoryUsed, Time: start.Add(10 * time.Second), Value: 2 << 30},
		{Identity: id0, Metric: store.MetricMemoryUsage, Series: store.SeriesMemoryUsed, Time: start.Add(30 * time.Second), Value: 6 << 30},
		{Identity: id0, Metric: store.MetricMemoryUsage, Series: "system.memory.state=cached", Time: start.Add(30 * time.Second), Value: 7 << 30},
		{Identity: id0, Metric: store.MetricMemoryUsage, Series: store.SeriesMemoryUsed, Time: start.Add(5 * time.Minute), Value: 7.5 * (1 << 30)},
		{Identity: id0, Metric: store.MetricCPUUtilization, Time: start.Add(10 * time.Second), Value: 0.95},
		{Identity: id0, Metric: store.MetricCPUUtilization, Time: start.Add(20 * time.Second), Value: 0.5},
		{Identity: id0, Metric: store.MetricCPUUtilization, Series: "cpu.mode=idle", Time: start.Add(15 * time.Second), Value: 0.99},
	}
	if _, err := st.InsertSamples(ctx, id, pts); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Sizing(ctx, store.Filter{Since: start.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("sizing rows = %d", len(rows))
	}
	r := rows[0]
	if *r.PeakMemory != 6<<30 || *r.MemTotal != 8<<30 || *r.PeakCPU != 0.95 || *r.CPUCount != 4 || *r.Saturated != 0.5 {
		t.Fatalf("sizing = mem %v/%v cpu %v x%v saturated %v; want the used memory and attribute-free CPU series inside the step only",
			*r.PeakMemory, *r.MemTotal, *r.PeakCPU, *r.CPUCount, *r.Saturated)
	}
}

func TestJobCountsEachSampleInstantInOneStep(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	ctx := context.Background()
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	mid, end := start.Add(10*time.Second), start.Add(20*time.Second)
	j := &github.Job{ID: 1, RunID: 1, RunAttempt: 1, Status: "completed", Conclusion: "success", StartedAt: &start, CompletedAt: &end,
		Steps: []github.Step{
			{Number: 1, Name: "build", Status: "completed", Conclusion: "success", StartedAt: &start, CompletedAt: &mid},
			{Number: 2, Name: "test", Status: "completed", Conclusion: "success", StartedAt: &mid, CompletedAt: &end},
		}}
	if err := st.InTx(ctx, func(tx pgx.Tx) error { return store.UpsertJob(ctx, tx, "acme/app", j) }); err != nil {
		t.Fatal(err)
	}
	id := runner.Identity{RunID: 1, RunAttempt: 1, CheckRunID: 1, Repository: "acme/app"}
	var pts []runner.Point
	for at := start; !at.After(end); at = at.Add(time.Second) {
		pts = append(pts,
			runner.Point{Identity: id, Metric: store.MetricCPUUtilization, Time: at, Value: 0.5},
			runner.Point{Identity: id, Metric: store.MetricMemoryUsage, Series: store.SeriesMemoryUsed, Time: at, Value: 1},
			runner.Point{Identity: id, Metric: store.MetricMemoryUsage, Series: "system.memory.state=free", Time: at, Value: 2})
	}
	if _, err := st.InsertSamples(ctx, 1, pts); err != nil {
		t.Fatal(err)
	}
	d, err := st.Job(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if d.Samples != 21 || len(d.Steps) != 2 || d.Steps[0].Samples != 10 || d.Steps[1].Samples != 11 {
		t.Fatalf("job samples %d, steps %+v; want 21 instants split 10 and 11", d.Samples, d.Steps)
	}
}

func TestSpendRoundsEachJobUp(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	ctx := context.Background()
	s := &seed{t: t, st: st}
	start := time.Now().Add(-time.Hour)
	for _, d := range []time.Duration{61 * time.Second, 10 * time.Second, 120 * time.Second} {
		s.job("acme/app", "main", []string{"ubuntu-latest"}, start, map[string]time.Duration{"x": d})
	}
	groups, err := st.SpendGroups(ctx, store.Filter{Since: start.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].Jobs != 3 || groups[0].Minutes != 5 {
		t.Fatalf("spend = %+v, want 3 jobs and 2+1+2 minutes", groups)
	}
}
