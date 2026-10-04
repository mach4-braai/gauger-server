package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/runner"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

func TestSizingJobsReportsPeaksAndP95PerJob(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	ctx := context.Background()
	s := &seed{t: t, st: st}
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	labels := []string{"ubuntu-latest"}
	sampled := s.job("acme/app", "main", labels, start, map[string]time.Duration{"build": 90 * time.Second})
	s.job("acme/app", "main", labels, start, map[string]time.Duration{"build": time.Minute})
	s.job("acme/other", "main", labels, start, map[string]time.Duration{"build": time.Minute})
	s.job("acme/app", "main", labels, start.Add(-72*time.Hour), map[string]time.Duration{"build": time.Minute})

	id := runner.Identity{RunID: sampled, RunAttempt: 1, CheckRunID: sampled, Repository: "acme/app"}
	pts := []runner.Point{
		{Identity: id, Metric: store.MetricMemoryLimit, Time: start, Value: 8 << 30},
		{Identity: id, Metric: store.MetricCPUCount, Time: start, Value: 4},
		{Identity: id, Metric: store.MetricMemoryUsage, Series: store.SeriesMemoryUsed, Time: start.Add(10 * time.Second), Value: 2 << 30},
		{Identity: id, Metric: store.MetricMemoryUsage, Series: store.SeriesMemoryUsed, Time: start.Add(20 * time.Second), Value: 3 << 30},
		{Identity: id, Metric: store.MetricMemoryUsage, Series: "system.memory.state=cached", Time: start.Add(20 * time.Second), Value: 7 << 30},
		{Identity: id, Metric: store.MetricCPUUtilization, Series: "cpu.mode=idle", Time: start.Add(10 * time.Second), Value: 0.99},
	}
	for i, v := range []float64{0.1, 0.2, 0.3, 0.4, 1.0} {
		pts = append(pts, runner.Point{Identity: id, Metric: store.MetricCPUUtilization, Time: start.Add(time.Duration(i) * 10 * time.Second), Value: v})
	}
	if _, err := st.InsertSamples(ctx, sampled, pts); err != nil {
		t.Fatal(err)
	}

	jobs, total, err := st.SizingJobs(ctx, store.Filter{Since: start.Add(-time.Hour), Until: start.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total jobs = %d, want the 3 inside the window", total)
	}
	if len(jobs) != 1 {
		t.Fatalf("sized jobs = %+v, want only the sampled one", jobs)
	}
	j := jobs[0]
	if j.ID != sampled || j.Repository != "acme/app" || j.Workflow != "CI" || j.Name != "build" || j.Minutes != 2 || len(j.Labels) != 1 || j.Labels[0] != "ubuntu-latest" {
		t.Errorf("job = %+v, want job %d, CI build in acme/app on ubuntu-latest for 2 minutes", j, sampled)
	}
	if j.PeakMemory == nil || *j.PeakMemory != 3<<30 || j.MemTotal == nil || *j.MemTotal != 8<<30 || j.CPUCount == nil || *j.CPUCount != 4 {
		t.Errorf("memory = %v of %v, cpus %v; want the used series' peak of MemTotal", j.PeakMemory, j.MemTotal, j.CPUCount)
	}
	if j.PeakCPU == nil || *j.PeakCPU != 1.0 || j.P95CPU == nil || math.Abs(*j.P95CPU-0.88) > 1e-9 {
		t.Errorf("cpu peak %v p95 %v, want 1 and 0.88 from the attribute-free series", j.PeakCPU, j.P95CPU)
	}

	other, total, err := st.SizingJobs(ctx, store.Filter{Repository: "acme/other", Since: start.Add(-time.Hour), Until: start.Add(time.Hour)})
	if err != nil || len(other) != 0 || total != 1 {
		t.Errorf("acme/other: %+v of %d jobs, %v; want no sized jobs of 1", other, total, err)
	}
}
