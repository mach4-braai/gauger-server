package store_test

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*max(1, math.Abs(a), math.Abs(b)) }

func relations(plan any, into map[string]bool) {
	switch v := plan.(type) {
	case map[string]any:
		if name, ok := v["Relation Name"].(string); ok {
			into[name] = true
		}
		for _, child := range v {
			relations(child, into)
		}
	case []any:
		for _, child := range v {
			relations(child, into)
		}
	}
}

func TestResourcesQueriesTouchOnlyPartitionsInRange(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)

	f := store.Filter{Since: fx.Now.Add(-7 * 24 * time.Hour), Until: fx.Now}
	inRange := map[string]bool{}
	var outside int
	all := partitions(t, st)
	for _, name := range all {
		day, err := time.Parse("20060102", strings.TrimPrefix(name, "samples_p"))
		if err != nil {
			t.Fatal(err)
		}
		if !day.Before(f.Since.Truncate(24*time.Hour)) && day.Before(f.Until) {
			inRange[name] = true
		} else {
			outside++
		}
	}
	if len(inRange) == 0 || outside < 10 {
		t.Fatalf("fixtures have %d partitions in range and %d outside it, want some in range and many outside", len(inRange), outside)
	}

	for _, by := range []string{store.ByLabel, store.ByWorkflow} {
		sqls, args := store.ResourcesStatements(f, f.Since, by, "day")
		for i, sql := range sqls {
			var raw []byte
			if err := st.Pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args[i]...).Scan(&raw); err != nil {
				t.Fatalf("explain statement %d: %v", i, err)
			}
			var plan any
			if err := json.Unmarshal(raw, &plan); err != nil {
				t.Fatal(err)
			}
			scanned := map[string]bool{}
			relations(plan, scanned)
			for name := range scanned {
				if strings.HasPrefix(name, "samples_p") && !inRange[name] {
					t.Errorf("statement %d by %s scans %s, outside %s to %s", i, by, name, f.Since, f.Until)
				}
			}
			if !slices.ContainsFunc(all, func(name string) bool { return scanned[name] }) {
				t.Errorf("statement %d by %s scans no samples partition", i, by)
			}
		}
	}
}

func TestResourcesUncountedMatchesCountedInterface(t *testing.T) {
	for _, name := range store.ResourcesUncounted {
		if store.CountedInterface(name) {
			t.Errorf("%s is left out of the traffic queries but CountedInterface counts it", name)
		}
	}
	for _, name := range []string{"eth0", "ens5", "docker0"} {
		if !store.CountedInterface(name) {
			t.Errorf("CountedInterface rejects %s", name)
		}
	}
}

// TestResourcesGroupEqualsItsOnlyJob filters to the one job on its labels
// and workflow, so its group must equal what the job's own series give.
func TestResourcesGroupEqualsItsOnlyJob(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	f := store.Filter{Repository: "oss/gauger", Event: "workflow_dispatch", Since: fx.Now.Add(-30 * 24 * time.Hour), Until: fx.Now}

	all, err := st.GaugerSeries(ctx, fx.ResourcesJob)
	if err != nil {
		t.Fatal(err)
	}
	series := all[fx.ResourcesJob]
	find := func(metric, key, value string) store.GaugerSeries {
		for _, g := range series {
			if g.Metric == metric && g.Attr(key) == value {
				return g
			}
		}
		t.Fatalf("job has no %s series with %s=%s", metric, key, value)
		return store.GaugerSeries{}
	}
	meanPeak := func(points []store.SeriesPoint) (mean, peak float64) {
		for _, p := range points {
			mean += p.Value
			peak = max(peak, p.Value)
		}
		return mean / float64(len(points)), peak
	}

	for _, by := range []string{store.ByLabel, store.ByWorkflow} {
		r, err := st.Resources(ctx, f, by, "hour")
		if err != nil {
			t.Fatal(err)
		}
		if r.Jobs != 1 || r.Sampled != 1 || r.FromArtifact != 0 || len(r.Groups) != 1 {
			t.Fatalf("by %s: jobs %d, sampled %d, from artifact %d, %d groups; want 1, 1, 0 and 1", by, r.Jobs, r.Sampled, r.FromArtifact, len(r.Groups))
		}
		g := r.Groups[0]
		if want := map[string]string{store.ByLabel: "self-hosted, gpu", store.ByWorkflow: "oss/gauger / Train"}[by]; g.Key != want || g.Jobs != 1 {
			t.Errorf("by %s: group %q with %d jobs, want %q with 1", by, g.Key, g.Jobs, want)
		}

		var modes []string
		for _, m := range g.Modes {
			modes = append(modes, m.Mode)
			mean, peak := meanPeak(find(store.MetricCPUUtilization, store.AttrCPUMode, m.Mode).Points)
			if !near(m.Mean, mean) || !near(m.Peak, peak) {
				t.Errorf("by %s: %s mean %v peak %v, want %v and %v", by, m.Mode, m.Mean, m.Peak, mean, peak)
			}
		}
		if want := []string{"idle", "interrupt", "iowait", "nice", "steal", "system", "user"}; !slices.Equal(modes, want) {
			t.Errorf("by %s: cpu modes %v, want %v", by, modes, want)
		}
		if mean, peak := meanPeak(find(store.MetricCPUUtilization, "", "").Points); g.Total == nil || !near(g.Total.Mean, mean) || !near(g.Total.Peak, peak) {
			t.Errorf("by %s: total cpu %+v, want mean %v peak %v", by, g.Total, mean, peak)
		}

		var states []string
		for _, s := range g.States {
			states = append(states, s.State)
			mean, peak := meanPeak(find(store.MetricMemoryUsage, store.AttrMemoryState, s.State).Points)
			if !near(s.Mean, mean) || !near(s.Peak, peak) {
				t.Errorf("by %s: memory %s mean %v peak %v, want %v and %v", by, s.State, s.Mean, s.Peak, mean, peak)
			}
		}
		if want := []string{"buffers", "cached", "free", "used"}; !slices.Equal(states, want) {
			t.Errorf("by %s: memory states %v, want %v", by, states, want)
		}
		used := find(store.MetricMemoryUsage, store.AttrMemoryState, "used").Points
		usedMean, usedPeak := meanPeak(used)
		limit := find(store.MetricMemoryLimit, "", "").Points[0].Value
		available := math.Inf(1)
		for _, p := range find("system.linux.memory.available", "", "").Points {
			available = min(available, p.Value)
		}
		switch {
		case g.PeakShare == nil || g.MeanShare == nil || g.Limit == nil || g.MinAvailable == nil:
			t.Fatalf("by %s: memory headroom is incomplete: %+v", by, g)
		case !near(*g.PeakShare, usedPeak/limit), !near(*g.MeanShare, usedMean/limit), !near(*g.Limit, limit), !near(*g.MinAvailable, available):
			t.Errorf("by %s: headroom peak %v mean %v limit %v available %v, want %v %v %v %v",
				by, *g.PeakShare, *g.MeanShare, *g.Limit, *g.MinAvailable, usedPeak/limit, usedMean/limit, limit, available)
		}

		disk := func(dir string) []store.SeriesPoint {
			for _, g := range series {
				if g.Metric == store.MetricDiskIO && g.Attr(store.AttrDiskDirection) == dir && g.Attr(store.AttrDevice) == "nvme0n1" {
					return g.Points
				}
			}
			t.Fatalf("no %s series", dir)
			return nil
		}
		net := func(dir string) []store.SeriesPoint {
			for _, g := range series {
				if g.Metric == store.MetricNetworkIO && g.Attr(store.AttrNetworkDirection) == dir && g.Attr(store.AttrInterface) == "eth0" {
					return g.Points
				}
			}
			t.Fatalf("no %s series", dir)
			return nil
		}
		if len(r.Disk) != 1 || r.Disk[0].Name != "nvme0n1" || !near(r.Disk[0].In, store.Increase(disk("read"))) || !near(r.Disk[0].Out, store.Increase(disk("write"))) {
			t.Errorf("by %s: disk = %+v, want nvme0n1 read %v written %v", by, r.Disk, store.Increase(disk("read")), store.Increase(disk("write")))
		}
		if len(r.Network) != 1 || r.Network[0].Name != "eth0" || !near(r.Network[0].In, store.Increase(net("receive"))) || !near(r.Network[0].Out, store.Increase(net("transmit"))) {
			t.Errorf("by %s: network = %+v, want eth0 only, in %v out %v", by, r.Network, store.Increase(net("receive")), store.Increase(net("transmit")))
		}
		if len(r.TopDisk) != 1 || r.TopDisk[0].ID != fx.ResourcesJob || r.TopDisk[0].Name != "train" || r.TopDisk[0].Workflow != "Train" || r.TopDisk[0].Repository != "oss/gauger" {
			t.Errorf("by %s: top disk = %+v, want the training job", by, r.TopDisk)
		}
		if len(r.TopNetwork) != 1 || !near(r.TopNetwork[0].In, store.Increase(net("receive"))) {
			t.Errorf("by %s: top network = %+v", by, r.TopNetwork)
		}

		hourly := map[time.Time][]float64{}
		for _, p := range find(store.MetricCPUUtilization, "", "").Points {
			hour := p.Time.Truncate(time.Hour)
			hourly[hour] = append(hourly[hour], p.Value)
		}
		if len(r.Timeline) != len(hourly) {
			t.Fatalf("by %s: %d timeline buckets, want %d", by, len(r.Timeline), len(hourly))
		}
		for _, b := range r.Timeline {
			sum := 0.0
			for _, v := range hourly[b.Bucket] {
				sum += v
			}
			if b.CPU == nil || b.Memory == nil || !near(*b.CPU, sum/float64(len(hourly[b.Bucket]))) {
				t.Errorf("by %s: bucket %s cpu %v, want %v", by, b.Bucket, b.CPU, sum/float64(len(hourly[b.Bucket])))
			}
		}
	}
}

func TestResourcesTopJobsRankByBytesWithoutLoopback(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	r, err := st.Resources(ctx, store.Filter{Since: fx.Now.Add(-90 * 24 * time.Hour), Until: fx.Now}, store.ByLabel, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.TopDisk) != 10 || len(r.TopNetwork) != 10 {
		t.Fatalf("top lists have %d disk and %d network jobs, want 10 each", len(r.TopDisk), len(r.TopNetwork))
	}
	for what, top := range map[string][]store.ResourceJob{"disk": r.TopDisk, "network": r.TopNetwork} {
		var ids []int64
		for i, j := range top {
			ids = append(ids, j.ID)
			if i > 0 && j.In+j.Out > top[i-1].In+top[i-1].Out {
				t.Errorf("top %s job %d moves more than the one above it", what, j.ID)
			}
		}
		got, err := st.GaugerSeries(ctx, ids...)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range top {
			var in, out float64
			for _, g := range got[j.ID] {
				switch {
				case what == "disk" && g.Metric == store.MetricDiskIO && g.Attr(store.AttrDiskDirection) == "read":
					in += store.Increase(g.Points)
				case what == "disk" && g.Metric == store.MetricDiskIO && g.Attr(store.AttrDiskDirection) == "write":
					out += store.Increase(g.Points)
				case what == "network" && g.Metric == store.MetricNetworkIO && store.CountedInterface(g.Attr(store.AttrInterface)):
					if g.Attr(store.AttrNetworkDirection) == "receive" {
						in += store.Increase(g.Points)
					} else {
						out += store.Increase(g.Points)
					}
				}
			}
			if !near(j.In, in) || !near(j.Out, out) {
				t.Errorf("top %s job %d in %v out %v, want %v and %v", what, j.ID, j.In, j.Out, in, out)
			}
		}
	}
}

func TestResourcesBeforeRetentionHasNoSamples(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)

	now := fx.Now.Add(100 * 24 * time.Hour)
	r, err := st.Resources(ctx, store.Filter{Until: now}, store.ByLabel, "day")
	if err != nil {
		t.Fatal(err)
	}
	if r.Jobs == 0 {
		t.Error("the window holds no jobs, so it proves nothing about their samples")
	}
	if r.Sampled != 0 || r.Oldest != nil || len(r.Groups) != 0 || len(r.Timeline) != 0 {
		t.Errorf("sampled %d, oldest %v, %d groups, %d buckets; want none past retention", r.Sampled, r.Oldest, len(r.Groups), len(r.Timeline))
	}
	if want := st.RetentionStart(now); !r.Since.Equal(want) {
		t.Errorf("samples are read from %s, want retention's start %s", r.Since, want)
	}
}

func TestResourcesEmptyWindow(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)

	r, err := st.Resources(ctx, store.Filter{Repository: "nobody/nothing", Since: fx.Now.Add(-7 * 24 * time.Hour), Until: fx.Now}, store.ByWorkflow, "day")
	if err != nil {
		t.Fatal(err)
	}
	if r.Jobs != 0 || r.Sampled != 0 || len(r.Groups) != 0 {
		t.Errorf("an unknown repository has %d jobs, %d sampled and %d groups", r.Jobs, r.Sampled, len(r.Groups))
	}
	if r.Oldest == nil {
		t.Error("the oldest sample kept does not depend on the filter, but is empty")
	}
}

func TestResourcesOldestSampleIsTheEarliestKept(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)

	var want time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT min(ts) FROM samples`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	r, err := st.Resources(ctx, store.Filter{Since: fx.Now.Add(-7 * 24 * time.Hour), Until: fx.Now}, store.ByLabel, "day")
	if err != nil {
		t.Fatal(err)
	}
	if r.Oldest == nil || !r.Oldest.Equal(want) {
		t.Errorf("oldest = %v, want %v", r.Oldest, want)
	}
}
