package ui_test

import (
	"context"
	"html"
	"math"
	"net/url"
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

// breakdownRow is one rendered table row: the group's name, each cell's
// text by column and each cell's raw value.
type breakdownRow struct {
	name   string
	text   map[string]string
	values map[string]string
	hrefs  []string
}

var (
	rowPattern  = regexp.MustCompile(`(?s)<tr data-row>(.*?)</tr>`)
	cellPattern = regexp.MustCompile(`<td[^>]*?data-col="([a-z0-9]+)"(?: data-value="([^"]*)")?[^>]*>(.*?)</td>`)
	tagPattern  = regexp.MustCompile(`<[^>]*>`)
	namePattern = regexp.MustCompile(`class="cell-primary"[^>]*>([^<]*)<`)
	hrefPattern = regexp.MustCompile(`<a href="([^"]*)" data-nav>(?:Runs|Trends)</a>`)
	totalCell   = regexp.MustCompile(`data-total="([a-z]+)" data-value="([^"]*)"`)
)

func breakdownRows(page string) []breakdownRow {
	var rows []breakdownRow
	for _, m := range rowPattern.FindAllStringSubmatch(page, -1) {
		row := breakdownRow{text: map[string]string{}, values: map[string]string{}}
		if n := namePattern.FindStringSubmatch(m[1]); n != nil {
			row.name = html.UnescapeString(n[1])
		}
		for _, c := range cellPattern.FindAllStringSubmatch(m[1], -1) {
			row.text[c[1]] = html.UnescapeString(strings.TrimSpace(tagPattern.ReplaceAllString(c[3], "")))
			row.values[c[1]] = c[2]
		}
		for _, h := range hrefPattern.FindAllStringSubmatch(m[1], -1) {
			row.hrefs = append(row.hrefs, html.UnescapeString(h[1]))
		}
		rows = append(rows, row)
	}
	return rows
}

func sumValues(t *testing.T, rows []breakdownRow, col string) float64 {
	t.Helper()
	sum := 0.0
	for _, r := range rows {
		v, err := strconv.ParseFloat(r.values[col], 64)
		if err != nil {
			t.Fatalf("row %q has no numeric %s: %q", r.name, col, r.values[col])
		}
		sum += v
	}
	return sum
}

func dollars(t *testing.T, s string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimPrefix(s, "$"), ",", ""), 64)
	if err != nil {
		t.Fatalf("not a dollar amount: %q", s)
	}
	return v
}

func TestBreakdownRowsMatchOverviewCards(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)

	for _, tc := range []struct {
		name, query string
		since       time.Time
		repo, event string
	}{
		{name: "full range", query: "range=all"},
		{name: "30 days", query: "range=30d", since: now.Add(-30 * 24 * time.Hour)},
		{name: "one repository", query: "range=90d&repo=acme%2Fapi", since: now.Add(-90 * 24 * time.Hour), repo: "acme/api"},
		{name: "one event", query: "range=90d&event=pull_request", since: now.Add(-90 * 24 * time.Hour), event: "pull_request"},
	} {
		want := expectedCards(t, st, tc.since, now, tc.repo, tc.event)
		overview := cards(get(t, h, "/stats/?"+tc.query).Body.String())
		if overview["runs"][0] != want["runs"] || overview["jobs"][0] != want["jobs"] {
			t.Fatalf("%s: the Overview shows %v, SQL says %v", tc.name, overview, want)
		}
		for _, d := range []string{"repo", "workflow", "job", "event", "branch"} {
			t.Run(tc.name+" by "+d, func(t *testing.T) {
				page := get(t, h, "/stats/breakdown?by="+d+"&"+tc.query).Body.String()
				rows := breakdownRows(page)
				if len(rows) == 0 {
					t.Fatal("no rows")
				}
				if got := chart.Integer(sumValues(t, rows, "jobs")); got != want["jobs"] {
					t.Errorf("jobs add up to %s, want %s", got, want["jobs"])
				}
				if got := chart.Integer(sumValues(t, rows, "minutes")); got != want["minutes"] {
					t.Errorf("minutes add up to %s, want %s", got, want["minutes"])
				}
				if got, wantSpend := sumValues(t, rows, "spend"), dollars(t, want["spend"]); math.Abs(got-wantSpend) > 0.005 {
					t.Errorf("spend adds up to %.4f, want %s", got, want["spend"])
				}
				if d != "job" {
					if got := chart.Integer(sumValues(t, rows, "runs")); got != want["runs"] {
						t.Errorf("runs add up to %s, want %s", got, want["runs"])
					}
				}
				totals := map[string]string{}
				for _, m := range totalCell.FindAllStringSubmatch(page, -1) {
					totals[m[1]] = m[2]
				}
				for col, wantTotal := range map[string]string{"jobs": want["jobs"], "minutes": want["minutes"]} {
					v, _ := strconv.ParseFloat(totals[col], 64)
					if got := chart.Integer(v); got != wantTotal {
						t.Errorf("total %s = %s, want %s", col, got, wantTotal)
					}
				}
			})
		}
	}
}

func TestBreakdownGroupsADynamicWorkflowIntoOneRow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	page := get(t, h, "/stats/breakdown?by=workflow&range=all&repo="+url.QueryEscape(storetest.DynamicRepo)).Body.String()
	var dynamic []breakdownRow
	for _, r := range breakdownRows(page) {
		if r.name == "github-code-scanning/codeql" {
			dynamic = append(dynamic, r)
		}
	}
	if len(dynamic) != 1 {
		t.Fatalf("%d rows for the dynamic workflow, want 1:\n%v", len(dynamic), breakdownRows(page))
	}
	if got, want := dynamic[0].values["runs"], strconv.Itoa(len(storetest.DynamicPullRequests)); got != want {
		t.Errorf("runs = %s, want %s, one per pull request", got, want)
	}
	if !strings.Contains(page, `href="https://github.com/`+storetest.DynamicRepo+`/actions/workflows/github-code-scanning/codeql"`) {
		t.Error("the workflow row does not link to its workflow on GitHub")
	}
	if len(dynamic[0].hrefs) != 1 || !strings.HasPrefix(dynamic[0].hrefs[0], "/stats/trends?") {
		t.Fatalf("dynamic workflow links = %v, want only Trends, as Runs matches a workflow by run name", dynamic[0].hrefs)
	}
	if got := trendsSelected(get(t, h, dynamic[0].hrefs[0]).Body.String(), "workflow"); !slices.Equal(got, []string{dynamic[0].name}) {
		t.Errorf("Trends selects workflow %v, want %s", got, dynamic[0].name)
	}
}

func TestBreakdownBranchViewShowsFailureRate(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)

	page := get(t, h, "/stats/breakdown?by=branch&range=all").Body.String()
	if !strings.Contains(page, "Failure rate") || strings.Contains(page, ">Success") {
		t.Error("the branch view should show failure rate in place of success")
	}
	var failed, decided int64
	err := st.Pool.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE conclusion <> 'success'), count(*) FROM runs
		WHERE status = 'completed' AND conclusion NOT IN ('cancelled', 'skipped') AND head_branch = 'main'
		  AND coalesce(run_started_at, created_at) < $1`, now).Scan(&failed, &decided)
	if err != nil || decided == 0 {
		t.Fatalf("fixtures have no decided runs on main: %v", err)
	}
	var main *breakdownRow
	rows := breakdownRows(page)
	for i := range rows {
		if rows[i].name == "main" {
			main = &rows[i]
		}
	}
	if main == nil {
		t.Fatalf("no row for main in %v", rows)
	}
	if got, want := main.text["failure"], chart.Percent(float64(failed)/float64(decided)); got != want {
		t.Errorf("failure rate on main = %s, want %s", got, want)
	}
	if len(main.hrefs) != 1 || !strings.HasPrefix(main.hrefs[0], "/stats/runs?") || !strings.Contains(main.hrefs[0], "branch=main") {
		t.Errorf("branch row links = %v, want only a Runs link filtered to the branch", main.hrefs)
	}
}

func TestBreakdownRowLinksNarrowTheTarget(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	for _, tc := range []struct {
		by, filter   string
		runs, trends bool
	}{
		{"repo", "range=30d&event=push", true, true},
		{"workflow", "range=30d&event=push", true, true},
		{"job", "range=30d&event=push", false, true},
		{"event", "range=30d", true, false},
		{"branch", "range=30d&event=push", true, false},
	} {
		t.Run(tc.by, func(t *testing.T) {
			filter, err := url.ParseQuery(tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			rows := breakdownRows(get(t, h, "/stats/breakdown?by="+tc.by+"&"+tc.filter).Body.String())
			if len(rows) == 0 {
				t.Fatal("no rows")
			}
			for _, r := range rows {
				links := map[string]url.Values{}
				for _, href := range r.hrefs {
					u, err := url.Parse(href)
					if err != nil {
						t.Fatal(err)
					}
					links[u.Path] = u.Query()
				}
				runs, hasRuns := links["/stats/runs"]
				trends, hasTrends := links["/stats/trends"]
				if hasRuns != tc.runs || hasTrends != tc.trends || len(links) != len(r.hrefs) {
					t.Fatalf("row %q links = %v, want Runs %v and Trends %v", r.name, r.hrefs, tc.runs, tc.trends)
				}
				for _, q := range links {
					for k := range filter {
						if q.Get(k) != filter.Get(k) {
							t.Errorf("row %q link %v drops the shell's %s", r.name, q, k)
						}
					}
				}
				if hasRuns {
					page := get(t, h, "/stats/runs?"+runs.Encode()).Body.String()
					if got := runsShown(t, page); strconv.Itoa(got) != r.values["jobs"] {
						t.Errorf("row %q: Runs at %v lists %d jobs, the row has %s", r.name, runs, got, r.values["jobs"])
					}
				}
				if !hasTrends {
					continue
				}
				page := get(t, h, "/stats/trends?"+trends.Encode()).Body.String()
				switch tc.by {
				case "repo":
					if trends.Get("repo") != r.name {
						t.Errorf("row %q: Trends link %v is not narrowed to the repository", r.name, trends)
					}
				case "workflow":
					if got := trendsSelected(page, "workflow"); !slices.Equal(got, []string{r.name}) {
						t.Errorf("row %q: Trends selects workflow %v", r.name, got)
					}
				case "job":
					if got := trendsSelected(page, "workflow"); len(got) != 1 || got[0] != trends.Get("workflow") {
						t.Errorf("row %q: Trends selects workflow %v, want %s", r.name, got, trends.Get("workflow"))
					}
					if got := trendsSelected(page, "job"); !slices.Equal(got, []string{r.name}) {
						t.Errorf("row %q: Trends selects job %v", r.name, got)
					}
				}
			}
		})
	}
}

func TestBreakdownSortsByTheQuery(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	column := func(query, col string) []float64 {
		t.Helper()
		var out []float64
		for _, r := range breakdownRows(get(t, h, "/stats/breakdown?by=repo&range=all&"+query).Body.String()) {
			v, err := strconv.ParseFloat(r.values[col], 64)
			if err != nil {
				t.Fatalf("%s of %q: %q", col, r.name, r.values[col])
			}
			out = append(out, v)
		}
		return out
	}
	for _, tc := range []struct{ query, col string }{
		{"", "minutes"},
		{"sort=runs&dir=desc", "runs"},
		{"sort=jobs&dir=asc", "jobs"},
	} {
		got := column(tc.query, tc.col)
		want := slices.Clone(got)
		slices.Sort(want)
		if !strings.Contains(tc.query, "asc") {
			slices.Reverse(want)
		}
		if len(got) < 2 || !slices.Equal(got, want) {
			t.Errorf("%q: %s column = %v, want it sorted", tc.query, tc.col, got)
		}
	}

	page := get(t, h, "/stats/breakdown?by=repo&range=all&sort=runs&dir=desc").Body.String()
	for _, want := range []string{
		`data-sorted="desc"`,
		`href="/stats/breakdown?by=repo&amp;dir=asc&amp;range=all&amp;sort=runs"`,
		`<input type="hidden" form="filters" name="sort" value="runs">`,
		`<input type="radio" form="filters" name="by" value="repo" checked>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %s", want)
		}
	}
}

func TestBreakdownUnknownDimensionShowsRepositories(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	rows := breakdownRows(get(t, h, "/stats/breakdown?by=nonsense&range=all").Body.String())
	var names []string
	for _, r := range rows {
		names = append(names, r.name)
	}
	slices.Sort(names)
	want := slices.Clone(fx.Repositories)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("rows = %v, want the repositories %v", names, want)
	}
}

func TestBreakdownDatastarRequestPatchesPage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	rec := get(t, h, "/stats/breakdown?by=event&range=30d&datastar=%7B%7D", "Datastar-Request", "true")
	body := rec.Body.String()
	for _, want := range []string{
		`data: elements <div id="page"`,
		`data: elements <form id="filters" class="filter-bar" method="get" action="/stats/breakdown"`,
		`value="30d" checked>`,
		`<input type="radio" form="filters" name="by" value="event" checked>`,
		`history.replaceState(null, '', "/stats/breakdown?by=event\u0026range=30d")`,
		`href="/stats/breakdown?by=event&amp;dir=asc&amp;range=30d&amp;sort=minutes"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("patch is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<html") {
		t.Error("a patch should carry the page, nav and filters, not the shell")
	}
}

func TestBreakdownEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.AddDate(0, 0, 30))

	for _, by := range []store.BreakdownDimension{store.BreakdownByRepository, store.BreakdownByBranch} {
		page := get(t, h, "/stats/breakdown?range=7d&by="+string(by)).Body.String()
		if rows := breakdownRows(page); len(rows) != 0 {
			t.Errorf("by %s: %d rows in an empty window", by, len(rows))
		}
		if !strings.Contains(page, "Nothing to break down in this range") {
			t.Errorf("by %s: the empty state is missing", by)
		}
	}
}
