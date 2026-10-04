package ui_test

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

const capacityJobWindow = `coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1 AND coalesce(j.started_at, j.created_at, j.runner_seen_at) < $2
	AND ($3 = '' OR j.repository = $3)
	AND ($4 = '' OR EXISTS (SELECT 1 FROM runs r WHERE r.id = j.run_id AND r.attempt = j.run_attempt AND r.event = $4))`

const capacityRunWindow = `coalesce(r.run_started_at, r.created_at) >= $1 AND coalesce(r.run_started_at, r.created_at) < $2
	AND ($3 = '' OR r.repository = $3) AND ($4 = '' OR r.event = $4)`

// capacityRow is one row of the label table as the page shows it.
type capacityRow map[string]string

// capacityTable reads the label table, keyed by label.
func capacityTable(page string) map[string]capacityRow {
	out := map[string]capacityRow{}
	rows := regexp.MustCompile(`(?s)<tr data-label="([^"]*)">(.*?)</tr>`)
	cells := regexp.MustCompile(`data-col="([a-z0-9-]+)"[^>]*>([^<]*)</td>`)
	for _, m := range rows.FindAllStringSubmatch(page, -1) {
		row := capacityRow{}
		for _, c := range cells.FindAllStringSubmatch(m[2], -1) {
			row[c[1]] = c[2]
		}
		out[m[1]] = row
	}
	return out
}

// expectedCapacityTable computes the label table with its own SQL.
func expectedCapacityTable(t *testing.T, st *store.Store, args []any) map[string]capacityRow {
	t.Helper()
	ctx := context.Background()
	rows, err := st.Pool.Query(ctx, `
		SELECT array_to_string(j.labels, ', '), count(*),
			coalesce(sum(ceil(extract(epoch FROM j.completed_at - j.started_at) / 60)) FILTER (WHERE j.status = 'completed' AND j.completed_at > j.started_at), 0)::bigint,
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM j.started_at - j.created_at)) FILTER (WHERE j.started_at >= j.created_at),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM j.started_at - j.created_at)) FILTER (WHERE j.started_at >= j.created_at)
		FROM jobs j WHERE `+capacityJobWindow+` GROUP BY j.labels`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]capacityRow{}
	for rows.Next() {
		var name string
		var jobs, minutes int64
		var p50, p95 *float64
		if err := rows.Scan(&name, &jobs, &minutes, &p50, &p95); err != nil {
			t.Fatal(err)
		}
		secs := func(v *float64) string {
			if v == nil {
				return "–"
			}
			return chart.Seconds(*v)
		}
		out[name] = capacityRow{
			"jobs": chart.Integer(float64(jobs)), "minutes": chart.Integer(float64(minutes)),
			"queue-p50": secs(p50), "queue-p95": secs(p95),
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// Spend is priced per label and repository visibility.
	priced, err := st.Pool.Query(ctx, `
		SELECT j.labels, p.private, sum(ceil(extract(epoch FROM j.completed_at - j.started_at) / 60))::bigint
		FROM jobs j LEFT JOIN repositories p ON p.full_name = j.repository
		WHERE j.status = 'completed' AND j.completed_at > j.started_at AND `+capacityJobWindow+` GROUP BY 1, 2`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer priced.Close()
	cost := map[string]float64{}
	unpriced := map[string]int64{}
	for priced.Next() {
		var labels []string
		var private *bool
		var minutes int64
		if err := priced.Scan(&labels, &private, &minutes); err != nil {
			t.Fatal(err)
		}
		p := spend.Rates(spend.DefaultRates).Price(labels, private, minutes)
		cost[strings.Join(labels, ", ")] += p.Cost
		if !p.Known {
			unpriced[strings.Join(labels, ", ")] += minutes
		}
	}
	if err := priced.Err(); err != nil {
		t.Fatal(err)
	}
	for name, row := range out {
		row["spend"] = chart.USD(cost[name])
		if unpriced[name] > 0 && row["minutes"] == chart.Integer(float64(unpriced[name])) {
			row["spend"] = "–"
		}
	}
	return out
}

func TestCapacityLabelTableMatchesSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range []struct {
		name, url   string
		now, since  time.Time
		repo, event string
		wantLabels  []string
	}{
		{name: "full range", url: "/stats/capacity?range=all", now: now, wantLabels: []string{"ubuntu-latest", "macos-latest", "ubuntu-24.04-arm", "self-hosted, linux"}},
		{name: "one repository", url: "/stats/capacity?range=90d&repo=acme%2Fapi", now: now, since: now.Add(-90 * 24 * time.Hour), repo: "acme/api"},
		{name: "one event", url: "/stats/capacity?range=30d&event=push", now: now, since: now.Add(-30 * 24 * time.Hour), event: "push"},
		{name: "capacity fixtures", url: "/stats/capacity?range=30d&repo=" + url.QueryEscape(storetest.CapacityRepo), now: now, since: now.Add(-30 * 24 * time.Hour), repo: storetest.CapacityRepo, wantLabels: []string{"ubuntu-latest", "macos-latest"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			got := capacityTable(page)
			want := expectedCapacityTable(t, st, []any{tc.since, tc.now, tc.repo, tc.event})
			if len(want) == 0 {
				t.Fatalf("fixtures have no jobs for %s; the case checks nothing", tc.url)
			}
			if len(got) != len(want) {
				t.Errorf("page lists %d labels, SQL finds %d", len(got), len(want))
			}
			for name, w := range want {
				for col, v := range w {
					if got[name][col] != v {
						t.Errorf("%s %s = %q, want %q", name, col, got[name][col], v)
					}
				}
			}
			for _, name := range tc.wantLabels {
				if _, ok := got[name]; !ok {
					t.Errorf("page lacks label %q", name)
				}
			}
			if rows := strings.Count(page, "data-label="); rows != len(want) {
				t.Errorf("%d table rows, want %d", rows, len(want))
			}
		})
	}
}

// capacityPeak sweeps the intervals of the jobs in the window. A job that
// completes the instant another starts does not overlap it.
func capacityPeak(t *testing.T, st *store.Store, args []any) int {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `
		SELECT j.started_at, j.completed_at FROM jobs j
		WHERE j.completed_at > j.started_at AND `+capacityJobWindow, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type event struct {
		at    time.Time
		delta int
	}
	var events []event
	for rows.Next() {
		var start, end time.Time
		if err := rows.Scan(&start, &end); err != nil {
			t.Fatal(err)
		}
		events = append(events, event{start, 1}, event{end, -1})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(events, func(a, b event) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return a.delta - b.delta
	})
	peak, running := 0, 0
	for _, e := range events {
		running += e.delta
		peak = max(peak, running)
	}
	return peak
}

func TestCapacityPeakConcurrencyMatchesSweep(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range []struct {
		name, url   string
		since       time.Time
		repo, event string
		want        string
	}{
		{name: "full range", url: "/stats/capacity?range=all"},
		{name: "ninety days", url: "/stats/capacity?range=90d", since: now.Add(-90 * 24 * time.Hour)},
		{name: "capacity fixtures", url: "/stats/capacity?range=30d&repo=" + url.QueryEscape(storetest.CapacityRepo), since: now.Add(-30 * 24 * time.Hour), repo: storetest.CapacityRepo, want: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, now), tc.url).Body.String()
			want := strconv.Itoa(capacityPeak(t, st, []any{tc.since, now, tc.repo, tc.event}))
			if tc.want != "" && want != tc.want {
				t.Fatalf("sweep finds a peak of %s, fixtures promise %s", want, tc.want)
			}
			if got := cards(page)["peak"][0]; got != want {
				t.Errorf("peak concurrent jobs = %q, want %q", got, want)
			}
		})
	}
}

func TestCapacityHeatmapCountsAddUpToRuns(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range []struct {
		name, url   string
		since       time.Time
		repo, event string
	}{
		{name: "full range", url: "/stats/capacity?range=all"},
		{name: "one repository", url: "/stats/capacity?range=30d&repo=acme%2Fweb", since: now.Add(-30 * 24 * time.Hour), repo: "acme/web"},
		{name: "one event", url: "/stats/capacity?range=90d&event=pull_request", since: now.Add(-90 * 24 * time.Hour), event: "pull_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, now), tc.url).Body.String()
			var cells []int
			for _, m := range regexp.MustCompile(`class="heatmap-cell"[^>]*data-value="(\d+)"`).FindAllStringSubmatch(page, -1) {
				n, _ := strconv.Atoi(m[1])
				cells = append(cells, n)
			}
			if len(cells) != 7*24 {
				t.Fatalf("heatmap has %d cells, want %d", len(cells), 7*24)
			}

			want := make([]int, 7*24)
			rows, err := st.Pool.Query(context.Background(), `
				SELECT extract(dow FROM t)::int, extract(hour FROM t)::int, count(*)
				FROM (SELECT coalesce(r.run_started_at, r.created_at) AT TIME ZONE 'UTC' AS t FROM runs r WHERE `+capacityRunWindow+`) x
				GROUP BY 1, 2`, tc.since, now, tc.repo, tc.event)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			runs := 0
			for rows.Next() {
				var dow, hour, n int
				if err := rows.Scan(&dow, &hour, &n); err != nil {
					t.Fatal(err)
				}
				want[(dow+6)%7*24+hour] = n
				runs += n
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if runs == 0 {
				t.Fatalf("fixtures have no runs for %s; the case checks nothing", tc.url)
			}

			total := 0
			for _, n := range cells {
				total += n
			}
			if total != runs {
				t.Errorf("heatmap cells add up to %d, want the %d runs in the window", total, runs)
			}
			if !slices.Equal(cells, want) {
				t.Errorf("heatmap cells differ from SQL by weekday and hour:\n got %v\nwant %v", cells, want)
			}
		})
	}
}

func TestCapacityEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.AddDate(0, 0, 30)), "/stats/capacity?range=7d").Body.String()

	for _, id := range []string{"start-p50", "start-p95"} {
		if got := cards(page)[id][0]; got != "–" {
			t.Errorf("card %s = %q, want –", id, got)
		}
	}
	if got := cards(page)["peak"][0]; got != "0" {
		t.Errorf("peak = %q, want 0", got)
	}
	if rows := capacityTable(page); len(rows) != 0 {
		t.Errorf("empty window lists labels: %v", rows)
	}
	for _, want := range []string{
		"No jobs in this range", "No job minutes in this range", "No jobs started in this range",
		"No runs in this range", "No jobs ran in this range",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("empty page lacks %q", want)
		}
	}
}

func TestCapacityLegendHidesALabel(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	page := get(t, h, "/stats/capacity?range=30d").Body.String()
	if !strings.Contains(page, `href="/stats/capacity?hide=minutes%3Aubuntu-latest&amp;range=30d"`) {
		t.Errorf("legend lacks a toggle for ubuntu-latest in the minutes chart")
	}
	hidden := get(t, h, "/stats/capacity?range=30d&hide=minutes%3Aubuntu-latest").Body.String()
	if !strings.Contains(hidden, `data-off`) {
		t.Error("hidden series is not marked off in the legend")
	}
}
