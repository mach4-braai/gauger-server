package store_test

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

func TestParseAttrs(t *testing.T) {
	if got := store.ParseAttrs(""); len(got) != 0 {
		t.Errorf("ParseAttrs(\"\") = %v, want none", got)
	}
	got := store.ParseAttrs("network.interface.name=eth0,network.io.direction=receive")
	want := map[string]string{"network.interface.name": "eth0", "network.io.direction": "receive"}
	if !maps.Equal(got, want) {
		t.Errorf("ParseAttrs = %v, want %v", got, want)
	}
}

func TestCounterRateAndIncrease(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	points := []store.SeriesPoint{
		{Time: at, Value: 100},
		{Time: at.Add(10 * time.Second), Value: 600},
		{Time: at.Add(20 * time.Second), Value: 50},
		{Time: at.Add(30 * time.Second), Value: 350},
	}
	rates := store.Rate(points)
	if len(rates) != 2 || rates[0].Value != 50 || !rates[0].Time.Equal(at.Add(10*time.Second)) || rates[1].Value != 30 {
		t.Errorf("Rate = %v, want 50/s then 30/s, skipping the step where the counter fell", rates)
	}
	if got := store.Increase(points); got != 500+50+300 {
		t.Errorf("Increase = %v, want 850, counting the fall as a restart from zero", got)
	}
}

func TestStepSpansDoNotOverlapWithinASecond(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	sec := func(n int) *time.Time { v := at.Add(time.Duration(n) * time.Second); return &v }
	steps := []store.StepUsage{
		{Number: 1, StartedAt: sec(0), CompletedAt: sec(5)},
		{Number: 2, StartedAt: sec(5), CompletedAt: sec(5)},
		{Number: 3, StartedAt: sec(5), CompletedAt: sec(9)},
		{Number: 4},
		{Number: 5, StartedAt: sec(12), CompletedAt: sec(12)},
		{Number: 6, StartedAt: sec(12)},
	}
	spans := store.StepSpans(steps, *sec(30))
	want := map[int][2]time.Duration{
		1: {0, 5}, 2: {5, 5}, 3: {5, 10}, 5: {12, 12}, 6: {12, 30},
	}
	if len(spans) != len(want) {
		t.Fatalf("spans = %v, want %d (a step that never started has none)", spans, len(want))
	}
	for i, sp := range spans {
		w := want[sp.Number]
		if got := [2]time.Duration{sp.Start.Sub(at) / time.Second, sp.End.Sub(at) / time.Second}; got != w {
			t.Errorf("step %d spans %v, want %v", sp.Number, got, w)
		}
		if i > 0 && sp.Start.Before(spans[i-1].End) {
			t.Errorf("step %d starts at %v before step %d ends at %v", sp.Number, sp.Start, spans[i-1].Number, spans[i-1].End)
		}
	}
}

// TestGaugerSeriesMatchStepUsage recomputes each step's usage from the
// series and the step spans, and expects what Job reports.
func TestGaugerSeriesMatchStepUsage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	ctx := context.Background()

	for _, id := range []int64{fx.SampledJob, fx.ArtifactJob, fx.ArtifactOnlyJob} {
		d, err := st.Job(ctx, id)
		if err != nil || d == nil {
			t.Fatalf("Job(%d) = %v, %v", id, d, err)
		}
		all, err := st.GaugerSeries(ctx, id, fx.UnsampledJob)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := all[fx.UnsampledJob]; ok {
			t.Errorf("a job without samples has series %v", all[fx.UnsampledJob])
		}
		series := all[id]

		metrics := map[string]int{}
		var cpu, mem []store.SeriesPoint
		for _, g := range series {
			metrics[g.Metric]++
			switch {
			case g.Metric == store.MetricCPUUtilization && g.Series == "":
				cpu = g.Points
			case g.Metric == store.MetricMemoryUsage && g.Series == store.SeriesMemoryUsed:
				mem = g.Points
			}
			if !slices.IsSortedFunc(g.Points, func(a, b store.SeriesPoint) int { return a.Time.Compare(b.Time) }) {
				t.Errorf("job %d series %q is not in time order", id, g.Series)
			}
		}
		for metric, n := range map[string]int{
			store.MetricCPUUtilization: 8, store.MetricMemoryUsage: 4, store.MetricMemoryLimit: 1, store.MetricCPUCount: 1,
			store.MetricDiskIO: 2, store.MetricNetworkIO: 4, "system.linux.memory.available": 1,
		} {
			if metrics[metric] != n {
				t.Errorf("job %d has %d %s series, want %d", id, metrics[metric], metric, n)
			}
		}
		var modes []string
		for _, g := range series {
			if m := g.Attr(store.AttrCPUMode); m != "" {
				modes = append(modes, m)
			}
		}
		slices.Sort(modes)
		if want := []string{"idle", "interrupt", "iowait", "nice", "steal", "system", "user"}; !slices.Equal(modes, want) {
			t.Errorf("job %d cpu modes = %v, want %v", id, modes, want)
		}

		until := *d.CompletedAt
		for _, sp := range store.StepSpans(d.Steps, until) {
			var usage store.StepUsage
			for _, s := range d.Steps {
				if s.Number == sp.Number {
					usage = s
				}
			}
			var peakMem, peakCPU, saturated float64
			var inCPU, samples int
			stamps := map[time.Time]bool{}
			for _, p := range mem {
				if !p.Time.Before(sp.Start) && p.Time.Before(sp.End) {
					peakMem = max(peakMem, p.Value)
					stamps[p.Time] = true
				}
			}
			for _, p := range cpu {
				if !p.Time.Before(sp.Start) && p.Time.Before(sp.End) {
					peakCPU = max(peakCPU, p.Value)
					inCPU++
					if p.Value >= 0.9 {
						saturated++
					}
					stamps[p.Time] = true
				}
			}
			samples = len(stamps)
			if int64(samples) != usage.Samples {
				t.Errorf("job %d step %d: %d sample instants, StepUsage has %d", id, sp.Number, samples, usage.Samples)
			}
			if inCPU == 0 {
				if usage.PeakCPU != nil || usage.Saturated != nil || usage.PeakMemory != nil {
					t.Errorf("job %d step %d has no samples but StepUsage = %+v", id, sp.Number, usage)
				}
				continue
			}
			if usage.PeakMemory == nil || *usage.PeakMemory != peakMem {
				t.Errorf("job %d step %d: peak memory from series %v, StepUsage %v", id, sp.Number, peakMem, usage.PeakMemory)
			}
			if usage.PeakCPU == nil || *usage.PeakCPU != peakCPU {
				t.Errorf("job %d step %d: peak CPU from series %v, StepUsage %v", id, sp.Number, peakCPU, usage.PeakCPU)
			}
			if want := saturated / float64(inCPU); usage.Saturated == nil || *usage.Saturated != want {
				t.Errorf("job %d step %d: time at 90%% CPU from series %v, StepUsage %v", id, sp.Number, want, usage.Saturated)
			}
		}
	}
}
