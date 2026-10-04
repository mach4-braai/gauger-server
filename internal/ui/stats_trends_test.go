package ui_test

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// trendsTips reads the trend chart's tooltips: bucket title, then series
// label, to the value shown.
func trendsTips(page string) map[string]map[string]string {
	out := map[string]map[string]string{}
	tip := regexp.MustCompile(`data-show="\$_chart\.trend === \d+"><div class="chart-tooltip-title">([^<]*)</div>((?:<div class="chart-tooltip-row">.*?</div>)*)`)
	row := regexp.MustCompile(`<span class="chart-tooltip-label">([^<]*)</span>\s*<span class="chart-tooltip-value">([^<]*)</span>`)
	for _, m := range tip.FindAllStringSubmatch(page, -1) {
		rows := map[string]string{}
		for _, r := range row.FindAllStringSubmatch(m[2], -1) {
			rows[html.UnescapeString(r[1])] = html.UnescapeString(r[2])
		}
		out[html.UnescapeString(m[1])] = rows
	}
	return out
}

// trendsLegend lists the labels of the trend chart's legend, runs line
// included. The step chart's legend is not read.
func trendsLegend(page string) []string {
	if i := strings.Index(page, `id="chart-step"`); i >= 0 {
		page = page[:i]
	}
	var out []string
	for _, m := range regexp.MustCompile(`<a class="legend-item" href="[^"]*" title="([^"]*)"`).FindAllStringSubmatch(page, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

// trendsSelected lists the non-empty options the named select marks selected.
func trendsSelected(page, name string) []string {
	block := regexp.MustCompile(`name="` + name + `"[^>]*>([\s\S]*?)</select>`).FindStringSubmatch(page)[1]
	var out []string
	for _, m := range regexp.MustCompile(`<option value="([^"]+)" selected>`).FindAllStringSubmatch(block, -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

// trendsTable reads the text of each body row of the table with id.
func trendsTable(t *testing.T, page, id string) [][]string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<table[^>]* id="` + id + `">.*?<tbody>(.*?)</tbody>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no table %q in the page", id)
	}
	tag := regexp.MustCompile(`<[^>]*>`)
	var rows [][]string
	for _, tr := range regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`).FindAllStringSubmatch(m[1], -1) {
		var cells []string
		for _, td := range regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`).FindAllStringSubmatch(tr[1], -1) {
			cells = append(cells, strings.TrimSpace(html.UnescapeString(tag.ReplaceAllString(td[1], ""))))
		}
		rows = append(rows, cells)
	}
	return rows
}

// trendsAxis lists the buckets the page should chart: from the bucket
// holding now-span (60 back for span 0) through the one holding now.
func trendsAxis(bucket string, now time.Time, span time.Duration) []time.Time {
	trunc := func(t time.Time) time.Time {
		switch d := t.Truncate(24 * time.Hour); bucket {
		case "week":
			return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
		case "month":
			return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		default:
			return d
		}
	}
	step := func(t time.Time, n int) time.Time {
		switch bucket {
		case "week":
			return t.AddDate(0, 0, 7*n)
		case "month":
			return t.AddDate(0, n, 0)
		}
		return t.AddDate(0, 0, n)
	}
	last := trunc(now)
	first := step(last, -59)
	if span > 0 {
		first = trunc(now.Add(-span))
	}
	var out []time.Time
	for t := first; !t.After(last); t = step(t, 1) {
		out = append(out, t)
	}
	return out[max(0, len(out)-60):]
}

func trendsTitle(bucket string, t time.Time) string {
	switch bucket {
	case "week":
		return "Week of " + t.Format("2006-01-02")
	case "month":
		return t.Format("2006-01")
	}
	return t.Format("2006-01-02")
}

func TestTrendsChartMediansMatchDailyDurations(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)
	ctx := context.Background()

	for _, tc := range []struct {
		name, query   string
		repo, bucket  string
		workflow, job string
		span          time.Duration
	}{
		{name: "all repositories by day", query: "range=30d", bucket: "day", span: 30 * 24 * time.Hour},
		{name: "one repository by week", query: "range=90d&bucket=week&repo=acme%2Fapi", repo: "acme/api", bucket: "week", span: 90 * 24 * time.Hour},
		{name: "all time by month", query: "range=all&bucket=month", bucket: "month"},
		{name: "all time by day is capped", query: "range=all", bucket: "day"},
		{name: "jobs of a workflow", query: "range=30d&repo=acme%2Fapi&workflow=CI", repo: "acme/api", bucket: "day", workflow: "CI", span: 30 * 24 * time.Hour},
		{name: "one job", query: "range=30d&repo=oss%2Fgauger&workflow=CI&job=test", repo: "oss/gauger", bucket: "day", workflow: "CI", job: "test", span: 30 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, h, "/stats/trends?"+tc.query).Body.String()
			tips := trendsTips(page)
			axis := trendsAxis(tc.bucket, now, tc.span)
			if len(tips) != len(axis) {
				t.Fatalf("chart has %d buckets, want %d", len(tips), len(axis))
			}

			rows, err := st.DailyDurations(ctx, store.Filter{Repository: tc.repo, Since: axis[0]}, tc.bucket, tc.workflow, tc.job)
			if err != nil {
				t.Fatal(err)
			}
			wantJobs := tc.workflow != ""
			runs := map[string]int64{}
			seen := 0
			for _, d := range rows {
				if (d.Job != "") != wantJobs || d.Bucket.Before(axis[0]) {
					continue
				}
				label := d.Workflow
				if wantJobs {
					label = d.Job
				}
				if tc.repo == "" {
					label = d.Repository + " · " + label
				}
				title := trendsTitle(tc.bucket, d.Bucket)
				if got, want := tips[title][label], chart.Seconds(d.Median); got != want {
					t.Errorf("%s, %s = %q, want %q", title, label, got, want)
				}
				runs[title] += d.Runs
				seen++
			}
			if seen == 0 {
				t.Fatalf("DailyDurations returned nothing for %s; the case checks nothing", tc.query)
			}
			runsLabel := map[bool]string{false: "Runs", true: "Job runs"}[wantJobs]
			for title, n := range runs {
				if got, want := tips[title][runsLabel], chart.Integer(float64(n)); got != want {
					t.Errorf("%s run count = %q, want %q", title, got, want)
				}
			}
			for title, rows := range tips {
				if _, ok := runs[title]; !ok && len(rows) > 0 {
					t.Errorf("%s has tooltip rows %v, want none for a bucket without runs", title, rows)
				}
			}
		})
	}
}

func TestTrendsRegressionsTableMatchesStore(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)

	for _, tc := range []struct {
		name, query, repo string
		ratio, min        float64
		wantBench         bool
	}{
		{name: "defaults", query: "range=30d", ratio: 1.25, min: 5, wantBench: true},
		{name: "one repository", query: "range=30d&repo=acme%2Fapi", repo: "acme/api", ratio: 1.25, min: 5, wantBench: true},
		{name: "stricter", query: "range=30d&ratio=2&min=100", ratio: 2, min: 100, wantBench: true},
		{name: "past the bench step", query: "range=30d&ratio=3&min=5", ratio: 3, min: 5},
		{name: "invalid inputs fall back to the defaults", query: "range=30d&ratio=x&min=-4", ratio: 1.25, min: 5, wantBench: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, h, "/stats/trends?"+tc.query).Body.String()
			want, err := st.Regressions(context.Background(), store.Filter{Repository: tc.repo, Since: now.Add(-30 * 24 * time.Hour)}, tc.ratio, tc.min)
			if err != nil {
				t.Fatal(err)
			}
			var wantRows [][]string
			bench := false
			for _, g := range want {
				wantRows = append(wantRows, []string{
					g.Day.UTC().Format("2006-01-02"), g.Repository, g.Workflow, g.Job, g.Step, g.Branch,
					chart.Seconds(g.Median), chart.Integer(float64(g.Runs)),
					chart.Seconds(g.Baseline) + " (" + chart.Integer(float64(g.BaseRuns)) + ")",
					strconv.FormatFloat(g.Median/g.Baseline, 'f', 2, 64) + "×",
				})
				bench = bench || g.Workflow == "Bench"
			}
			if bench != tc.wantBench {
				t.Fatalf("Regressions has a Bench row = %v, want %v", bench, tc.wantBench)
			}
			var got [][]string
			if len(want) > 0 {
				got = trendsTable(t, page, "regressions")
			} else if !strings.Contains(page, "No step is above its baseline by that much.") {
				t.Error("empty regressions table should say so")
			}
			if !slices.EqualFunc(got, wantRows, slices.Equal[[]string]) {
				t.Errorf("regressions table:\n got %q\nwant %q", got, wantRows)
			}
			for _, name := range []string{"ratio", "min"} {
				want := map[string]float64{"ratio": tc.ratio, "min": tc.min}[name]
				if !strings.Contains(page, `name="`+name+`"`) || !strings.Contains(page, `value="`+strconv.FormatFloat(want, 'f', -1, 64)+`"`) {
					t.Errorf("input %s should show %v", name, want)
				}
			}
		})
	}
}

func TestTrendsSelectedRegressionChartsItsStepWithBaseline(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)

	rows, err := st.Regressions(context.Background(), store.Filter{Repository: "acme/api", Since: now.Add(-30 * 24 * time.Hour)}, 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	var g store.Regression
	for _, r := range rows {
		if r.Workflow == "Bench" {
			g = r
		}
	}
	if g.Workflow == "" {
		t.Fatalf("Regressions = %+v, want the Bench step", rows)
	}

	page := get(t, h, "/stats/trends?range=30d&repo=acme%2Fapi&ratio=2&min=100").Body.String()
	var href string
	for _, tr := range regexp.MustCompile(`(?s)<tr[^>]*>.*?</tr>`).FindAllString(page, -1) {
		if m := regexp.MustCompile(`<a href="([^"]*)" data-nav>`).FindStringSubmatch(tr); m != nil && strings.Contains(tr, ">Bench</a>") {
			href = m[1]
		}
	}
	if href == "" {
		t.Fatalf("regressions table has no selectable Bench row:\n%s", page)
	}
	link, err := url.Parse(html.UnescapeString(href))
	if err != nil || link.Path != "/stats/trends" || link.Query().Get("step") == "" || link.Query().Get("ratio") != "2" {
		t.Fatalf("select link = %q, want the page with step set and the other parameters kept", href)
	}
	if strings.Contains(page, `id="chart-step"`) {
		t.Error("the step chart should wait for a selection")
	}

	selected := get(t, h, link.String()).Body.String()
	i := strings.Index(selected, `id="chart-step"`)
	if i < 0 {
		t.Fatalf("selecting the row did not chart its step:\n%s", selected)
	}
	chartHTML := selected[i:]
	if !strings.Contains(chartHTML, chart.Seconds(g.Baseline)) || !strings.Contains(chartHTML, "14-day baseline") {
		t.Errorf("step chart should draw the %s baseline as a reference line", chart.Seconds(g.Baseline))
	}
	day := regexp.MustCompile(`data-show="\$_chart\.step === \d+"><div class="chart-tooltip-title">` + g.Day.UTC().Format("2006-01-02") + `</div>(.*?)</div></div>`).FindStringSubmatch(chartHTML)
	if day == nil || !strings.Contains(day[1], chart.Seconds(g.Median)) {
		t.Errorf("step chart tooltip for %s should show the median %s, got %v", g.Day, chart.Seconds(g.Median), day)
	}
	if !strings.Contains(selected, `data-selected="true"`) || !strings.Contains(selected, `type="hidden" form="filters" name="step"`) {
		t.Error("the selected row should be marked and kept in the form")
	}

	other := get(t, h, "/stats/trends?range=30d&repo=acme%2Fapi&ratio=2&min=100&step=%5B%22nope%22%5D").Body.String()
	if strings.Contains(other, `id="chart-step"`) {
		t.Error("a step that is not in the table should chart nothing")
	}
}

func TestTrendsGroupsDynamicWorkflowByFile(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	page := get(t, dashboard(st, now), "/stats/trends?range=90d&repo=acme%2Fapi&bucket=week").Body.String()

	const label = "github-code-scanning/codeql"
	if n := strings.Count(strings.Join(trendsLegend(page), "\n"), label); n != 1 {
		t.Errorf("legend has %d entries for %s, want one series", n, label)
	}
	if len(fx.PRWorkflows) != 2 {
		t.Fatalf("fixtures name %d pull request runs, want 2", len(fx.PRWorkflows))
	}
	for _, name := range fx.PRWorkflows {
		if strings.Contains(page, name) {
			t.Errorf("page names the per-pull-request workflow %q, want the file's label only", name)
		}
	}

	var want int64
	err := st.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM runs r
		WHERE r.repository = 'acme/api' AND r.path = 'dynamic/github-code-scanning/codeql'
		  AND r.status = 'completed' AND r.conclusion = 'success'
		  AND EXISTS (SELECT 1 FROM jobs j WHERE j.run_id = r.id AND j.run_attempt = r.attempt
		              AND j.conclusion = 'success' AND j.started_at >= $1)`, trendsAxis("week", now, 90*24*time.Hour)[0]).Scan(&want)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range trendsTable(t, page, "trend-Workflows") {
		if row[1] == label {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("workflows table has %d rows for %s, want 1", count, label)
	}
	row := regexp.MustCompile(`(?s)<tr>\s*<td>acme/api</td>\s*<td>\s*<a href="[^"]*">` + label + `</a>\s*</td>(.*?)</tr>`).FindStringSubmatch(page)
	if row == nil {
		t.Fatalf("workflows table has no linked row for %s", label)
	}
	var got int64
	for _, n := range regexp.MustCompile(`title="(\d+) runs?"`).FindAllStringSubmatch(row[1], -1) {
		c, _ := strconv.Atoi(n[1])
		got += int64(c)
	}
	if want < int64(len(fx.PRWorkflows)) || got != want {
		t.Errorf("%s ran %d times in the table, want %d successful runs, at least the %d pull request runs", label, got, want, len(fx.PRWorkflows))
	}
}

func TestTrendsEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.AddDate(0, 0, 30)), "/stats/trends?range=7d").Body.String()

	for _, want := range []string{
		`<div class="chart-empty">No successful runs in this window</div>`,
		"No successful jobs in this window.",
		"No step is above its baseline by that much.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("empty window should say %q", want)
		}
	}
}

func TestTrendsFiltersByWorkflowAndJob(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	ctx := context.Background()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(10 * time.Hour)
	var id int64
	run := func(repo, workflow string, jobs map[string]int) {
		id++
		err := st.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := store.UpsertRun(ctx, tx, repo, &github.Run{ID: id, RunAttempt: 1, Name: workflow, Status: "completed", Conclusion: "success"}); err != nil {
				return err
			}
			i := int64(0)
			for name, secs := range jobs {
				i++
				end := day.Add(time.Duration(secs) * time.Second)
				if err := store.UpsertJob(ctx, tx, repo, &github.Job{
					ID: id*100 + i, RunID: id, RunAttempt: 1, WorkflowName: workflow, Name: name,
					Status: "completed", Conclusion: "success", StartedAt: &day, CompletedAt: &end,
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run("acme/app", "CI", map[string]int{"a": 100, "b": 300})
	run("acme/app", "Deploy", map[string]int{"release": 50})
	run("other/repo", "Lint", map[string]int{"vet": 20})

	h := dashboard(st, day.Add(time.Hour))
	today := day.Format("2006-01-02")

	page := get(t, h, "/stats/trends?repo=acme/app&range=24h").Body.String()
	if got, want := trendsLegend(page), []string{"CI", "Deploy", "Runs"}; !slices.Equal(got, want) {
		t.Errorf("legend = %v, want one series per workflow and the run count", got)
	}
	tips := trendsTips(page)[today]
	if tips["CI"] != "5m 00s" || tips["Deploy"] != "50s" || tips["Runs"] != "2" {
		t.Errorf("tooltip for %s = %v, want CI 5m 00s (first job start to last job end), Deploy 50s, 2 runs", today, tips)
	}

	page = get(t, h, "/stats/trends?repo=acme/app&range=24h&workflow=CI").Body.String()
	if got := trendsSelected(page, "workflow"); !slices.Equal(got, []string{"CI"}) {
		t.Fatalf("workflow select = %v, want [CI] selected", got)
	}
	if got, want := trendsLegend(page), []string{"a", "b", "Job runs"}; !slices.Equal(got, want) {
		t.Fatalf("legend = %v, want bars for jobs a and b, not workflows", got)
	}
	jobs := trendsTable(t, page, "trend-Jobs")
	if len(jobs) != 2 || jobs[0][1] != "CI" || jobs[0][2] != "a" || jobs[1][2] != "b" {
		t.Errorf("Jobs table = %q, want the narrowed a and b rows of CI", jobs)
	}
	if !regexp.MustCompile(`(?s)id="trend-Jobs".*?<td>\s*<a href="/stats/jobs/\d+">a</a>\s*</td>`).MatchString(page) {
		t.Errorf("job a should link to its job page:\n%s", page)
	}
	for _, row := range trendsTable(t, page, "trend-Jobs") {
		if slices.Contains(row, "release") {
			t.Errorf("Jobs table still lists Deploy's job after filtering to CI: %q", row)
		}
	}

	page = get(t, h, "/stats/trends?repo=acme/app&range=24h&workflow=CI&job=a").Body.String()
	if got := trendsSelected(page, "job"); !slices.Equal(got, []string{"a"}) {
		t.Errorf("job select = %v, want [a] selected", got)
	}
	if got, want := trendsLegend(page), []string{"a", "Job runs"}; !slices.Equal(got, want) {
		t.Fatalf("legend = %v, want exactly one bar for job a", got)
	}
	if tips := trendsTips(page)[today]; tips["a"] != "1m 40s" || tips["Job runs"] != "1" {
		t.Errorf("chart bar for job a = %v, want 1m 40s from 1 run", tips)
	}

	page = get(t, h, "/stats/trends?repo=other/repo&range=24h&workflow=CI&job=a").Body.String()
	if got := trendsSelected(page, "workflow"); len(got) != 0 {
		t.Fatalf("workflow select = %v, want none selected (CI does not exist on other/repo)", got)
	}
	if got := trendsSelected(page, "job"); len(got) != 0 {
		t.Fatalf("job select = %v, want none selected", got)
	}
	if got, want := trendsLegend(page), []string{"Lint", "Runs"}; !slices.Equal(got, want) {
		t.Fatalf("legend = %v, want the unfiltered Lint workflow bar", got)
	}

	page = get(t, h, "/stats/trends?range=24h").Body.String()
	if got, want := trendsLegend(page), []string{"acme/app · CI", "acme/app · Deploy", "other/repo · Lint", "Runs"}; !slices.Equal(got, want) {
		t.Errorf("legend = %v, want repository-prefixed labels with every repository", got)
	}
}

func TestTrendsBucketPickerFallsBackToDay(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)
	for _, v := range []string{"", "fortnight", "DAY", "Week"} {
		page := get(t, h, "/stats/trends?bucket="+v).Body.String()
		if !regexp.MustCompile(`name="bucket" form="filters" value="day" checked`).MatchString(page) {
			t.Errorf("bucket=%q should select day", v)
		}
	}
	for _, v := range []string{"day", "week", "month"} {
		page := get(t, h, "/stats/trends?bucket="+v).Body.String()
		if !regexp.MustCompile(`name="bucket" form="filters" value="` + v + `" checked`).MatchString(page) {
			t.Errorf("bucket=%q should be selected", v)
		}
	}
}

func TestOldDailyAndRegressionsRedirectToTrends(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)

	for _, tc := range []struct{ from, to string }{
		{"/daily", "/stats/trends?range=30d"},
		{"/daily?repo=acme/app&days=1&bucket=week&workflow=CI&job=a", "/stats/trends?bucket=week&job=a&range=24h&repo=acme%2Fapp&workflow=CI"},
		{"/regressions?days=45&ratio=2&min=10&repo=acme/api", "/stats/trends?min=10&range=90d&ratio=2&repo=acme%2Fapi"},
		{"/regressions?days=7", "/stats/trends?range=7d"},
		{"/regressions?days=3650", "/stats/trends?range=all"},
		{"/regressions?days=zero&ratio=", "/stats/trends?range=30d"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.from, nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != tc.to {
			t.Errorf("GET %s = %d to %q, want 302 to %q", tc.from, rec.Code, rec.Header().Get("Location"), tc.to)
		}
	}
}
