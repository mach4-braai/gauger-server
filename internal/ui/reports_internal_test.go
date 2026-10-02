package ui

import (
	"regexp"
	"strconv"
	"testing"
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
