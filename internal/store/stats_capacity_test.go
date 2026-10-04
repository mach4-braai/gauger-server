package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

func TestLoadByBucketPeakConcurrency(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	day := storetest.CapacityDay(fx.Now)
	f := store.Filter{Repository: storetest.CapacityRepo, Since: day, Until: fx.Now}

	daily, err := st.LoadByBucket(context.Background(), f, "day")
	if err != nil {
		t.Fatal(err)
	}
	// Day 0 has two overlapping jobs and a separate one. Day 2's two jobs
	// end and start at the same instant.
	want := []int64{2, 1, 1}
	if len(daily) != len(want) {
		t.Fatalf("got %d day buckets, want %d: %+v", len(daily), len(want), daily)
	}
	for i, w := range want {
		if !daily[i].Bucket.Equal(day.AddDate(0, 0, i)) || daily[i].Peak != w {
			t.Errorf("day %d: bucket %s peak %d, want %s peak %d", i, daily[i].Bucket, daily[i].Peak, day.AddDate(0, 0, i), w)
		}
	}

	// Queue time on day 0 is 5 s, 300 s and 20 s.
	if d := daily[0]; d.QueueP50 == nil || *d.QueueP50 != 20 || d.QueueP95 == nil || math.Abs(*d.QueueP95-272) > 1e-6 {
		t.Errorf("day 0 queue p50, p95 = %v, %v; want 20, 272", fmtp(d.QueueP50), fmtp(d.QueueP95))
	}

	hourly, err := st.LoadByBucket(context.Background(), f, "hour")
	if err != nil {
		t.Fatal(err)
	}
	peaks := map[int64]store.BucketLoad{}
	for _, h := range hourly {
		peaks[h.Bucket.Unix()] = h
	}
	for _, c := range []struct {
		at   time.Time
		peak int64
	}{
		{day.Add(10 * time.Hour), 2},
		{day.Add(11 * time.Hour), 0},
		{day.Add(14 * time.Hour), 1},
		{day.AddDate(0, 0, 1).Add(12 * time.Hour), 1},
		{day.AddDate(0, 0, 2).Add(9 * time.Hour), 1},
	} {
		got, ok := peaks[c.at.Unix()]
		if !ok || got.Peak != c.peak {
			t.Errorf("hour %s: peak %d (present %v), want %d", c.at.Format(time.RFC3339), got.Peak, ok, c.peak)
		}
	}
	// Nothing starts in the 12:00 hour of day 1, so it has a peak and no queue time.
	if h := peaks[day.AddDate(0, 0, 1).Add(12*time.Hour).Unix()]; h.QueueP50 != nil || h.QueueP95 != nil {
		t.Errorf("hour without a start has queue p50, p95 = %v, %v", fmtp(h.QueueP50), fmtp(h.QueueP95))
	}
}

func fmtp(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func TestLoadByBucketWithoutJobs(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	now := time.Now().UTC()
	got, err := st.LoadByBucket(context.Background(), store.Filter{Since: now.Add(-24 * time.Hour), Until: now}, "hour")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d buckets from an empty database, want none", len(got))
	}
}

func TestCapacityFixtureAggregates(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	ctx := context.Background()
	day := storetest.CapacityDay(fx.Now)
	f := store.Filter{Repository: storetest.CapacityRepo, Since: day, Until: fx.Now}

	labels, err := st.LabelStats(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 2 || labels[0].Name() != "ubuntu-latest" || labels[1].Name() != "macos-latest" {
		t.Fatalf("labels = %+v, want ubuntu-latest then macos-latest", labels)
	}
	// ubuntu: 20 + 10 + 155 + 30 + 30 minutes over 5 jobs; macos: one 30 minute job.
	if u := labels[0]; u.Jobs != 5 || u.Minutes != 245 || len(u.Billing) != 1 || u.Billing[0].Minutes != 245 {
		t.Errorf("ubuntu-latest = %+v", u)
	}
	if m := labels[1]; m.Jobs != 1 || m.Minutes != 30 || m.QueueP95 == nil || *m.QueueP95 != 300 {
		t.Errorf("macos-latest = %+v", m)
	}

	hourly, err := st.QueueByHour(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"ubuntu-latest@9": 9.75, "ubuntu-latest@10": 5, "ubuntu-latest@11": 5, "ubuntu-latest@14": 20, "macos-latest@10": 300}
	if len(hourly) != len(want) {
		t.Errorf("got %d label and hour rows, want %d: %+v", len(hourly), len(want), hourly)
	}
	for _, h := range hourly {
		key := fmt.Sprintf("%s@%d", store.LabelName(h.Labels), h.Hour)
		if w, ok := want[key]; !ok || math.Abs(h.P95-w) > 1e-6 {
			t.Errorf("%s queue p95 = %v, want %v (listed %v)", key, h.P95, w, ok)
		}
	}

	delay, err := st.RunStartDelay(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if delay.P50 == nil || *delay.P50 != 5 || delay.P95 == nil || math.Abs(*delay.P95-18) > 1e-6 {
		t.Errorf("run start p50, p95 = %v, %v; want 5, 18", fmtp(delay.P50), fmtp(delay.P95))
	}

	grid, err := st.RunsByWeekdayHour(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	var runs int64
	for _, c := range grid {
		runs += c.Runs
		if c.Weekday < 0 || c.Weekday > 6 || c.Hour < 0 || c.Hour > 23 {
			t.Errorf("cell %+v is outside the week", c)
		}
	}
	if runs != 5 {
		t.Errorf("weekday and hour cells hold %d runs, want 5", runs)
	}
}
