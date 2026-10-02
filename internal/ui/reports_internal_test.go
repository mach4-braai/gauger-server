package ui

import (
	"fmt"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"
)

func TestDailyChartBarHeightsMatchMedians(t *testing.T) {
	labels := []string{"10-01", "10-02"}
	rows := []dailyRow{
		{Repository: "acme/app", Workflow: "CI", Cells: []dailyCell{{Median: 100, Runs: 3}, {}}},
		{Repository: "acme/app", Workflow: "Deploy", Cells: []dailyCell{{Median: 50, Runs: 1}, {Median: 25, Runs: 2}}},
	}
	out := string(dailyChart(labels, rows, workflowLabel(false)))

	heights := regexp.MustCompile(`height="([\d.]+)"`).FindAllStringSubmatch(out, -1)
	if len(heights) != 3 {
		t.Fatalf("got %d bars, want 3 (one cell has no runs and should be empty)", len(heights))
	}
	heightOf := func(i int) float64 {
		f, err := strconv.ParseFloat(heights[i][1], 64)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	h100, h50, h25 := heightOf(0), heightOf(1), heightOf(2)
	if h100 <= h50 || h50 <= h25 {
		t.Fatalf("heights = %.1f, %.1f, %.1f, want strictly decreasing with medians 100 > 50 > 25", h100, h50, h25)
	}
	if ratio := h100 / h50; ratio < 1.9 || ratio > 2.1 {
		t.Errorf("CI/Deploy day1 height ratio = %.2f, want ~2 (100 vs 50)", ratio)
	}

	if !regexp.MustCompile(`CI · 10-01 · 1m40s · 3 runs`).MatchString(out) {
		t.Errorf("missing hover title for CI day1, got:\n%s", out)
	}
	if !regexp.MustCompile(`<span class="legend-item">.*CI.*Deploy`).MatchString(out) {
		t.Errorf("legend missing both workflows, got:\n%s", out)
	}
}

func TestDailyChartWithAllReposPrefixesLabel(t *testing.T) {
	rows := []dailyRow{{Repository: "acme/app", Workflow: "CI", Cells: []dailyCell{{Median: 10, Runs: 1}}}}
	out := string(dailyChart([]string{"10-01"}, rows, workflowLabel(true)))
	if !regexp.MustCompile(`acme/app · CI`).MatchString(out) {
		t.Errorf("want %q label when repo filter is all, got:\n%s", "acme/app · CI", out)
	}
}

func TestDailyChartEmptyWithoutRuns(t *testing.T) {
	if out := dailyChart([]string{"10-01"}, nil, workflowLabel(false)); out != "" {
		t.Errorf("want no chart with no rows, got %q", out)
	}
}

func TestBucketNameFallsBackToDay(t *testing.T) {
	for _, v := range []string{"", "fortnight", "DAY", "Week"} {
		r := httptest.NewRequest("GET", "/daily?bucket="+v, nil)
		if got := bucketName(r); got != "day" {
			t.Errorf("bucketName(%q) = %q, want %q", v, got, "day")
		}
	}
	for _, v := range []string{"day", "week", "month"} {
		r := httptest.NewRequest("GET", "/daily?bucket="+v, nil)
		if got := bucketName(r); got != v {
			t.Errorf("bucketName(%q) = %q, want %q", v, got, v)
		}
	}
}

func TestBucketStartsCapsAtSixtyBuckets(t *testing.T) {
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)

	month := bucketStarts("month", 365, now)
	if len(month) > 13 {
		t.Errorf("month buckets for days=365 = %d, want at most 13", len(month))
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); len(month) == 0 || !month[len(month)-1].Equal(want) {
		t.Errorf("last month bucket = %v, want %v", month, want)
	}

	week := bucketStarts("week", 3650, now)
	if len(week) != maxDailyBuckets {
		t.Errorf("week buckets for days=3650 = %d, want cap %d", len(week), maxDailyBuckets)
	}
	for _, s := range week {
		if s.Weekday() != time.Monday {
			t.Errorf("week bucket %v is not a Monday", s)
		}
	}
}

func TestBucketLabels(t *testing.T) {
	if got := dailyBuckets["day"].label(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)); got != "10-02" {
		t.Errorf("day label = %q, want %q", got, "10-02")
	}
	monday := time.Date(2025, 9, 29, 0, 0, 0, 0, time.UTC)
	_, wantWeek := monday.ISOWeek()
	if got, want := dailyBuckets["week"].label(monday), fmt.Sprintf("W%02d", wantWeek); got != want {
		t.Errorf("week label = %q, want %q", got, want)
	}
	if got := dailyBuckets["month"].label(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); got != "2026-10" {
		t.Errorf("month label = %q, want %q", got, "2026-10")
	}
}
