package stats

import (
	"context"
	"fmt"
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
)

func TestTrendBucketNamedFallsBackToDay(t *testing.T) {
	for _, v := range []string{"", "fortnight", "DAY", "Week"} {
		if got := trendBucketNamed(v).name; got != "day" {
			t.Errorf("trendBucketNamed(%q) = %q, want %q", v, got, "day")
		}
	}
	for _, v := range []string{"day", "week", "month"} {
		if got := trendBucketNamed(v).name; got != v {
			t.Errorf("trendBucketNamed(%q) = %q, want %q", v, got, v)
		}
	}
}

func TestTrendStartsCapAtSixtyBuckets(t *testing.T) {
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)

	month := trendStarts(trendBucketNamed("month"), now.AddDate(0, 0, -365), now)
	if len(month) > 13 {
		t.Errorf("month buckets for a year = %d, want at most 13", len(month))
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); len(month) == 0 || !month[len(month)-1].Equal(want) {
		t.Errorf("last month bucket = %v, want %v", month, want)
	}

	week := trendStarts(trendBucketNamed("week"), now.AddDate(0, 0, -3650), now)
	if len(week) != maxDailyBuckets {
		t.Errorf("week buckets for ten years = %d, want cap %d", len(week), maxDailyBuckets)
	}
	for _, s := range week {
		if s.Weekday() != time.Monday {
			t.Errorf("week bucket %v is not a Monday", s)
		}
	}

	for _, name := range []string{"day", "week", "month"} {
		all := trendStarts(trendBucketNamed(name), time.Time{}, now)
		if len(all) != maxDailyBuckets || !all[len(all)-1].Equal(trendBucketNamed(name).trunc(now)) {
			t.Errorf("%s buckets with no start = %d ending %v, want %d ending at now's bucket", name, len(all), all[len(all)-1], maxDailyBuckets)
		}
	}

	day := trendStarts(trendBucketNamed("day"), now.Add(-24*time.Hour), now)
	if len(day) != 2 || !day[0].Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day buckets for 24 hours = %v, want yesterday and today", day)
	}
}

func TestTrendBucketLabels(t *testing.T) {
	if got := trendBucketNamed("day").tick(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); got != "10-02" {
		t.Errorf("day label = %q, want %q", got, "10-02")
	}
	monday := time.Date(2025, 9, 29, 0, 0, 0, 0, time.UTC)
	_, wantWeek := monday.ISOWeek()
	if got, want := trendBucketNamed("week").tick(monday), fmt.Sprintf("W%02d", wantWeek); got != want {
		t.Errorf("week label = %q, want %q", got, want)
	}
	if got := trendBucketNamed("month").tick(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); got != "2026-10" {
		t.Errorf("month label = %q, want %q", got, "2026-10")
	}
}

func TestRangeForDays(t *testing.T) {
	for days, want := range map[int]string{1: "24h", 2: "7d", 7: "7d", 14: "30d", 30: "30d", 45: "90d", 90: "90d", 91: "all", 3650: "all"} {
		if got := RangeForDays(days); got != want {
			t.Errorf("RangeForDays(%d) = %q, want %q", days, got, want)
		}
	}
}

func TestTrendSeriesAreTheStoresMedians(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	s := &Server{Store: st, Now: func() time.Time { return now }}
	ctx := context.Background()

	for _, url := range []string{
		"/stats/trends?range=90d&bucket=week",
		"/stats/trends?range=30d&repo=acme%2Fapi&workflow=CI",
		"/stats/trends?range=30d&repo=acme%2Fapi&workflow=CI&job=test",
	} {
		r := httptest.NewRequest("GET", url, nil)
		f, _ := s.filter(r)
		v, err := s.trendsView(r, f)
		if err != nil {
			t.Fatal(err)
		}
		b := trendBucketNamed(r.URL.Query().Get("bucket"))
		starts := trendStarts(b, f.Since, f.Until)
		rows, err := st.DailyDurations(ctx, store.Filter{Repository: f.Repository, Since: starts[0]}, b.name, v.Workflow, v.Job)
		if err != nil {
			t.Fatal(err)
		}

		want := map[string][]float64{}
		for _, d := range rows {
			if (d.Job != "") != (v.Workflow != "") {
				continue
			}
			key := trendRow{Repository: d.Repository, Workflow: d.Workflow, Job: d.Job, Path: d.Path}.key()
			if want[key] == nil {
				want[key] = make([]float64, len(starts))
				for i := range want[key] {
					want[key][i] = math.NaN()
				}
			}
			for i, start := range starts {
				if start.Equal(d.Bucket) {
					want[key][i] = d.Median
				}
			}
		}
		if len(want) == 0 {
			t.Fatalf("%s: DailyDurations returned nothing; the case checks nothing", url)
		}
		for _, series := range v.Chart.Series {
			if series.Key == "runs" {
				continue
			}
			w := want[series.Key]
			delete(want, series.Key)
			if len(series.Values) != len(w) {
				t.Fatalf("%s: series %s has %d values, want %d", url, series.Label, len(series.Values), len(w))
			}
			for i, got := range series.Values {
				if got != w[i] && !(math.IsNaN(got) && math.IsNaN(w[i])) {
					t.Errorf("%s: series %s bucket %d = %v, want %v", url, series.Label, i, got, w[i])
				}
			}
		}
		if len(want) > 0 {
			t.Errorf("%s: the chart has no series for %v", url, want)
		}
	}
}
