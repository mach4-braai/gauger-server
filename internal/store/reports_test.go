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
	var slowest int64
	for i, secs := range []int{10, 20, 30, 40, 100} {
		id := s.job("acme/app", "main", nil, base.Add(time.Duration(i)*time.Hour), map[string]time.Duration{"make": time.Duration(secs) * time.Second})
		if secs == 100 {
			slowest = id
		}
	}
	s.job("acme/other", "main", nil, base, map[string]time.Duration{"make": time.Hour})

	rows, err := st.SlowSteps(context.Background(), store.Filter{Repository: "acme/app", Since: base.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Runs != 5 || rows[0].P50 != 30 || math.Abs(rows[0].P95-88) > 1e-9 {
		t.Fatalf("slow steps = %+v, want make with 5 runs, p50 30s, p95 88s", rows)
	}
	if rows[0].JobID != slowest || rows[0].StepNumber != 1 {
		t.Fatalf("slow step job = %d step %d, want the slowest occurrence job %d step 1", rows[0].JobID, rows[0].StepNumber, slowest)
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
	slowest := s.job("acme/app", "main", nil, today, map[string]time.Duration{"test": 30 * time.Second})
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
	if r.Branch != "main" || r.Median != 25 || r.Baseline != 10 || r.BaseRuns != 5 || !r.Day.Equal(today.Truncate(24*time.Hour)) {
		t.Fatalf("regression = %+v", r)
	}
	if r.JobID != slowest || r.StepNumber != 1 {
		t.Fatalf("regression job = %d step %d, want the slowest occurrence job %d step 1", r.JobID, r.StepNumber, slowest)
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

	start2 := start.Add(2 * time.Hour)
	id2 := s.job("acme/app", "main", nil, start2, map[string]time.Duration{"compile": time.Minute})
	id1 := runner.Identity{RunID: id2, RunAttempt: 1, CheckRunID: id2, Repository: "acme/app"}
	pts2 := []runner.Point{
		{Identity: id1, Metric: store.MetricMemoryLimit, Time: start2, Value: 8 << 30},
		{Identity: id1, Metric: store.MetricCPUCount, Time: start2, Value: 4},
		{Identity: id1, Metric: store.MetricMemoryUsage, Series: store.SeriesMemoryUsed, Time: start2.Add(10 * time.Second), Value: 1 << 30},
	}
	if _, err := st.InsertSamples(ctx, id2, pts2); err != nil {
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
	if r.JobID != id || r.StepNumber != 1 {
		t.Fatalf("sizing job = %d step %d, want the peak-memory occurrence job %d step 1", r.JobID, r.StepNumber, id)
	}
}

func TestDailyDurationsPerWorkflowAndJob(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	ctx := context.Background()
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2).Add(10 * time.Hour)
	at := func(d time.Time, secs int) *time.Time { x := d.Add(time.Duration(secs) * time.Second); return &x }
	type job struct {
		name, status, conclusion string
		from, to                 int
	}
	const untimed = -1
	run := func(id int64, start time.Time, status, conclusion string, jobs ...job) {
		t.Helper()
		err := st.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := store.UpsertRun(ctx, tx, "acme/app", &github.Run{ID: id, RunAttempt: 1, Name: "CI", Status: status, Conclusion: conclusion}); err != nil {
				return err
			}
			for i, j := range jobs {
				gj := &github.Job{ID: id*10 + int64(i), RunID: id, RunAttempt: 1, WorkflowName: "CI", Name: j.name,
					Status: j.status, Conclusion: j.conclusion}
				if j.from != untimed {
					gj.StartedAt = at(start, j.from)
					if j.status == "completed" {
						gj.CompletedAt = at(start, j.to)
					}
				}
				if err := store.UpsertJob(ctx, tx, "acme/app", gj); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run(1, day, "completed", "success", job{"a", "completed", "success", 0, 120}, job{"b", "completed", "success", 60, 300})
	run(2, day, "completed", "success", job{"a", "completed", "success", 0, 240}, job{"b", "completed", "skipped", 0, 0})
	run(3, day, "completed", "failure", job{"a", "completed", "success", 0, 60}, job{"b", "completed", "failure", 0, 500})
	run(4, day, "in_progress", "", job{"a", "completed", "success", 0, 30}, job{"b", "in_progress", "", 0, 0})
	run(5, day.AddDate(0, 0, 1), "completed", "success", job{"a", "completed", "success", 0, 100})
	run(6, day, "completed", "failure", job{"a", "completed", "success", 0, 80}, job{"b", "completed", "failure", untimed, 0})

	rows, err := st.DailyDurations(ctx, store.Filter{Since: day.AddDate(0, 0, -1)})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.DailyDuration{}
	for _, r := range rows {
		got[r.Job+" "+r.Day.UTC().Format("01-02")] = r
	}
	d0, d1 := day.Format("01-02"), day.AddDate(0, 0, 1).Format("01-02")
	for key, want := range map[string]struct {
		median float64
		runs   int64
	}{
		" " + d0:  {270, 2},
		" " + d1:  {100, 1},
		"a " + d0: {80, 5},
		"b " + d0: {240, 1},
		"a " + d1: {100, 1},
	} {
		if r, ok := got[key]; !ok || r.Median != want.median || r.Runs != want.runs {
			t.Errorf("%q = %+v, want median %v over %d runs", key, r, want.median, want.runs)
		}
	}
	if len(rows) != 5 {
		t.Errorf("rows = %+v, want 5", rows)
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
