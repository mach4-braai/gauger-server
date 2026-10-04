package ui_test

import (
	"context"
	"html"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// resourcesTable returns the cells of the row for group in the table with
// the given id, as column to mean and peak.
func resourcesTable(t *testing.T, page, id, group string) map[string][2]string {
	t.Helper()
	table := regexp.MustCompile(`(?s)<table[^>]*id="` + id + `".*?</table>`).FindString(page)
	if table == "" {
		t.Fatalf("no %s table", id)
	}
	row := regexp.MustCompile(`(?s)<tr data-group="` + regexp.QuoteMeta(html.EscapeString(group)) + `"[^>]*>(.*?)</tr>`).FindStringSubmatch(table)
	if row == nil {
		t.Fatalf("no row for %q in %s", group, id)
	}
	cells := map[string][2]string{}
	re := regexp.MustCompile(`data-col="([a-z-]+)">\s*(?:<span class="mean">([^<]*)</span>(?:\s*<span class="micro peak">peak ([^<]*)</span>)?|([^<]*))\s*<`)
	for _, m := range re.FindAllStringSubmatch(row[1], -1) {
		if m[4] != "" {
			cells[m[1]] = [2]string{html.UnescapeString(m[4]), ""}
		} else {
			cells[m[1]] = [2]string{html.UnescapeString(m[2]), html.UnescapeString(m[3])}
		}
	}
	return cells
}

func resourcesPercentOf(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil {
		t.Fatalf("%q is not a percentage", s)
	}
	return v
}

func TestResourcesCPUModeSharesMatchTheJobPage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	page := get(t, h, "/stats/resources?range=30d&repo=oss%2Fgauger&event=workflow_dispatch").Body.String()
	got := resourcesTable(t, page, "resources-cpu", "self-hosted, gpu")
	job := get(t, h, jobPageURL(fx.ResourcesJob)).Body.String()

	cpu := job[strings.Index(job, `id="chart-cpu"`):strings.Index(job, `id="chart-memory"`)]
	sums, slots := map[string]float64{}, 0
	for _, tip := range strings.Split(cpu, `class="chart-tooltip"`)[1:] {
		rows := regexp.MustCompile(`chart-tooltip-label">([a-z]+)</span>\s*<span class="chart-tooltip-value">([\d.]+)%`).FindAllStringSubmatch(tip, -1)
		if len(rows) == 0 {
			continue
		}
		slots++
		for _, r := range rows {
			v, _ := strconv.ParseFloat(r[2], 64)
			sums[r[1]] += v
		}
	}
	if slots < 10 {
		t.Fatalf("job page has %d slices with CPU values, too few to compare", slots)
	}

	modes := []string{"user", "system", "iowait", "steal", "interrupt"}
	for _, mode := range modes {
		peak := regexp.MustCompile(`(?s)title="` + mode + `"[^>]*>.*?legend-value">peak ([^<]*)<`).FindStringSubmatch(job)
		if peak == nil {
			t.Fatalf("the job page has no %s peak", mode)
		}
		if got[mode][1] != peak[1] {
			t.Errorf("%s peak = %s, the job page shows %s", mode, got[mode][1], peak[1])
		}
		mean := resourcesPercentOf(t, got[mode][0])
		if want := sums[mode] / float64(slots); math.Abs(mean-want) > 0.1 {
			t.Errorf("%s mean = %.1f%%, the job page's slices average %.2f%%", mode, mean, want)
		}
	}
	if _, ok := got["nice"]; ok {
		t.Error("nice is zero for every sample and should have no column")
	}
	if _, ok := got["idle"]; ok {
		t.Error("idle should have no column")
	}
}

func TestResourcesTilesMatchSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	var oldest time.Time
	if err := st.Pool.QueryRow(context.Background(), `SELECT min(ts) FROM samples`).Scan(&oldest); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, url, repo string
		since           time.Time
	}{
		{name: "full range", url: "/stats/resources?range=all"},
		{name: "ninety days", url: "/stats/resources?range=90d", since: now.Add(-90 * 24 * time.Hour)},
		{name: "one repository", url: "/stats/resources?range=90d&repo=oss%2Fdocs", since: now.Add(-90 * 24 * time.Hour), repo: "oss/docs"},
		{name: "last week", url: "/stats/resources?range=7d", since: now.Add(-7 * 24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var jobs, sampled, artifact int64
			err := st.Pool.QueryRow(context.Background(), `
				SELECT count(*),
					count(*) FILTER (WHERE EXISTS (SELECT 1 FROM samples s WHERE s.job_id = j.id)),
					count(*) FILTER (WHERE j.artifact_ingested_at IS NOT NULL AND EXISTS (SELECT 1 FROM samples s WHERE s.job_id = j.id))
				FROM jobs j
				WHERE coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1
				  AND coalesce(j.started_at, j.created_at, j.runner_seen_at) < $2
				  AND ($3 = '' OR j.repository = $3)`, tc.since, now, tc.repo).Scan(&jobs, &sampled, &artifact)
			if err != nil {
				t.Fatal(err)
			}
			if sampled == 0 {
				t.Fatalf("no sampled jobs in %s; the case checks nothing", tc.url)
			}
			page := get(t, dashboard(st, now), tc.url).Body.String()
			got := cards(page)
			want := map[string][2]string{
				"resources-sampled": {chart.Integer(float64(sampled)), chart.Integer(float64(artifact)) + " from the fallback artifact"},
				"resources-share":   {chart.Percent(float64(sampled) / float64(jobs)), chart.Integer(float64(sampled)) + " of " + chart.Integer(float64(jobs)) + " jobs"},
				"resources-oldest":  {oldest.UTC().Format("2006-01-02"), "samples are kept for 90 days"},
			}
			for id, w := range want {
				if got[id] != w {
					t.Errorf("tile %s = %q, want %q", id, got[id], w)
				}
			}
		})
	}
}

// resourcesTopFromSeries ranks the sampled jobs by the bytes their
// series moved, from GaugerSeries and Increase.
func resourcesTopFromSeries(t *testing.T, st *store.Store, counts func(store.GaugerSeries) bool) []int64 {
	t.Helper()
	ctx := context.Background()
	rows, err := st.Pool.Query(ctx, `SELECT DISTINCT job_id FROM samples`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	all, err := st.GaugerSeries(ctx, ids...)
	if err != nil {
		t.Fatal(err)
	}
	moved := map[int64]float64{}
	for id, series := range all {
		for _, g := range series {
			if counts(g) {
				moved[id] += store.Increase(g.Points)
			}
		}
	}
	slices.SortFunc(ids, func(a, b int64) int {
		switch {
		case moved[a] > moved[b]:
			return -1
		case moved[a] < moved[b]:
			return 1
		}
		return int(a - b)
	})
	return ids[:10]
}

func TestResourcesTopJobsLinkToTheJobPage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.Add(time.Minute)), "/stats/resources?range=90d").Body.String()

	for _, tc := range []struct {
		id     string
		counts func(store.GaugerSeries) bool
	}{
		{"resources-top-disk", func(g store.GaugerSeries) bool { return g.Metric == store.MetricDiskIO }},
		{"resources-top-network", func(g store.GaugerSeries) bool {
			return g.Metric == store.MetricNetworkIO && store.CountedInterface(g.Attr(store.AttrInterface))
		}},
	} {
		section := regexp.MustCompile(`(?s)id="` + tc.id + `".*?</div></div>`).FindString(page)
		var got []string
		for _, m := range regexp.MustCompile(`class="bar-list-row" href="/stats/jobs/(\d+)" data-nav`).FindAllStringSubmatch(section, -1) {
			got = append(got, m[1])
		}
		var want []string
		for _, id := range resourcesTopFromSeries(t, st, tc.counts) {
			want = append(want, strconv.FormatInt(id, 10))
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s links jobs %v, want %v", tc.id, got, want)
		}
	}
}

func TestResourcesGroupByWorkflowAndLegendToggles(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	byLabel := get(t, h, "/stats/resources?range=30d").Body.String()
	if !strings.Contains(byLabel, `data-group="self-hosted, gpu"`) || strings.Contains(byLabel, `data-group="oss/gauger / Train"`) {
		t.Error("grouping by label should list runner labels, not workflows")
	}
	byWorkflow := get(t, h, "/stats/resources?range=30d&by=workflow").Body.String()
	if !strings.Contains(byWorkflow, `data-group="oss/gauger / Train"`) || strings.Contains(byWorkflow, `data-group="self-hosted, gpu"`) {
		t.Error("grouping by workflow should list repository and workflow, not labels")
	}
	if !strings.Contains(byWorkflow, `name="by" value="workflow" checked`) {
		t.Error("the group-by control does not show workflow as selected")
	}

	hidden := get(t, h, "/stats/resources?range=30d&hide=resourcescpu%3Aiowait").Body.String()
	if !strings.Contains(hidden, `name="hide" value="resourcescpu:iowait"`) {
		t.Error("a hidden series is not carried by the filter form")
	}
	if strings.Count(hidden, "<rect") >= strings.Count(byLabel, "<rect") {
		t.Error("hiding the iowait series left the CPU chart's bars unchanged")
	}

	for _, want := range []string{`id="chart-resourcescpu"`, `id="chart-resourcesmemory"`, `id="chart-resourcestrend"`, `href="/stats/resources?range=30d"`, "Infrastructure"} {
		if !strings.Contains(byLabel, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	patch := get(t, h, "/stats/resources?range=30d&by=workflow&datastar=%7B%7D", "Datastar-Request", "true").Body.String()
	if !strings.Contains(patch, `data-group="oss/gauger / Train"`) || strings.Contains(patch, "<html") {
		t.Error("a Datastar request should patch the page grouped by workflow")
	}
}

func TestResourcesEmptyStates(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)

	for _, tc := range []struct {
		name, url string
		now       time.Time
		title     string
		hint      string
	}{
		{
			name: "range before retention", url: "/stats/resources?range=all", now: fx.Now.AddDate(0, 0, 100),
			title: "No samples in this range", hint: "gauger keeps samples for 90 days",
		},
		{
			name: "empty window", url: "/stats/resources?range=7d", now: fx.Now.AddDate(0, 0, 30),
			title: "No jobs in this range", hint: "No job started in the last 7 days",
		},
		{
			name: "jobs without samples", url: "/stats/resources?range=90d&repo=acme%2Fweb", now: fx.Now.Add(time.Minute),
			title: "No jobs with samples in this range", hint: "none reported runner samples",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			if !strings.Contains(page, `id="resources-empty"`) {
				t.Fatal("no empty state")
			}
			for _, want := range []string{tc.title, tc.hint} {
				if !strings.Contains(page, want) {
					t.Errorf("empty state is missing %q", want)
				}
			}
			for _, absent := range []string{`id="chart-`, `<table`, `id="resources-top-`} {
				if strings.Contains(page, absent) {
					t.Errorf("an empty range still draws %q", absent)
				}
			}
			if got := cards(page)["resources-sampled"][0]; got != "0" {
				t.Errorf("jobs with samples = %q, want 0", got)
			}
		})
	}

	page := get(t, dashboard(st, fx.Now.AddDate(0, 0, 100)), "/stats/resources?range=all").Body.String()
	if got := cards(page)["resources-oldest"][0]; got != "–" {
		t.Errorf("oldest sample kept = %q past retention, want –", got)
	}
}
