package ui_test

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

const runsJobBegan = `coalesce(j.started_at, j.created_at, j.runner_seen_at)`

var (
	runsJobLink = regexp.MustCompile(`href="/stats/jobs/(\d+)"`)
	runsTotal   = regexp.MustCompile(`of ([\d,]+) jobs`)
)

// runsJobIDs lists the jobs a Runs page links to, in table order.
func runsJobIDs(page string) []int64 {
	var ids []int64
	for _, m := range runsJobLink.FindAllStringSubmatch(page, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		ids = append(ids, id)
	}
	return ids
}

// runsShown reads "of N jobs" from a page, or 0 when it has none.
func runsShown(t *testing.T, page string) int {
	t.Helper()
	m := runsTotal.FindStringSubmatch(page)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// runsQueryIDs runs sql, which selects job ids, over the jobs of the
// window [since, until) in repo (empty for all) and returns them.
// Conditions go in where; its parameters start at $4.
func runsQueryIDs(t *testing.T, st *store.Store, since, until time.Time, repo, where, order string, args ...any) []int64 {
	t.Helper()
	if where == "" {
		where = "true"
	}
	rows, err := st.Pool.Query(context.Background(), `
		SELECT j.id FROM jobs j LEFT JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
		WHERE `+runsJobBegan+` >= $1 AND `+runsJobBegan+` < $2 AND ($3 = '' OR j.repository = $3) AND (`+where+`)
		ORDER BY `+order, append([]any{since, until, repo}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
	return ids
}

const runsNewestFirst = runsJobBegan + ` DESC, j.id DESC`

func TestRunsTableListsRecentJobs(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)

	for _, tc := range []struct {
		name, url string
		since     time.Time
		repo      string
	}{
		{"one repository over 7 days", "/stats/runs?range=7d&repo=oss%2Fgauger", now.AddDate(0, 0, -7), "oss/gauger"},
		{"one repository over 90 days", "/stats/runs?range=90d&repo=acme%2Fapi", now.AddDate(0, 0, -90), "acme/api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, h, tc.url).Body.String()
			recent, err := st.RecentJobs(context.Background(), store.Filter{Repository: tc.repo, Since: tc.since}, store.RunsPageSize)
			if err != nil {
				t.Fatal(err)
			}
			var want []int64
			for _, j := range recent {
				want = append(want, j.ID)
			}
			if len(want) == 0 {
				t.Fatalf("RecentJobs returned nothing for %s; the case checks nothing", tc.url)
			}
			if got := runsJobIDs(page); !reflect.DeepEqual(got, want) {
				t.Errorf("table lists jobs %v, RecentJobs returns %v", got, want)
			}
		})
	}
}

func TestRunsPagesOfOneHundred(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)

	all := runsQueryIDs(t, st, time.Time{}, now, "", "", runsNewestFirst)
	if len(all) < 2*store.RunsPageSize+1 {
		t.Fatalf("fixtures have %d jobs; the case needs three pages", len(all))
	}
	for _, tc := range []struct {
		url  string
		want []int64
		from string
	}{
		{"/stats/runs?range=all", all[:100], "1–100"},
		{"/stats/runs?range=all&page=2", all[100:200], "101–200"},
		{"/stats/runs?range=all&page=3", all[200:min(300, len(all))], ""},
		{"/stats/runs?range=all&page=9999", all[(len(all)-1)/100*100:], ""},
	} {
		page := get(t, h, tc.url).Body.String()
		if got := runsJobIDs(page); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s lists jobs %v, want %v", tc.url, got, tc.want)
		}
		if n := runsShown(t, page); n != len(all) {
			t.Errorf("%s says %d jobs, want %d", tc.url, n, len(all))
		}
		if tc.from != "" && !strings.Contains(page, tc.from+" of ") {
			t.Errorf("%s does not say it shows %s", tc.url, tc.from)
		}
	}

	first := get(t, h, "/stats/runs?range=all").Body.String()
	if strings.Contains(first, ">Previous<") || !strings.Contains(first, `href="/stats/runs?page=2&amp;range=all" data-nav>Next<`) {
		t.Errorf("first page links wrong:\n%s", first)
	}
	last := get(t, h, "/stats/runs?range=all&page=9999").Body.String()
	if strings.Contains(last, ">Next<") || !strings.Contains(last, ">Previous<") {
		t.Error("last page links wrong")
	}
}

func TestRunsFiltersNarrowTheTable(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)
	since := now.AddDate(0, 0, -90)
	base := "/stats/runs?range=90d&repo=acme%2Fapi"

	day := fx.Now.AddDate(0, 0, -3).UTC().Truncate(24 * time.Hour)
	bucket := day.Format(time.RFC3339)

	all := runsQueryIDs(t, st, since, now, "acme/api", "", runsNewestFirst)
	basePage := get(t, h, base).Body.String()
	if n := runsShown(t, basePage); n != len(all) {
		t.Fatalf("unfiltered page says %d jobs, SQL has %d", n, len(all))
	}
	link := "/stats/runs?bucket=" + url.QueryEscape(bucket) + "&range=90d&repo=acme%2Fapi"
	if !strings.Contains(basePage, `<a href="`+html.EscapeString(link)+`" data-nav>`) {
		t.Fatalf("the chart has no link for the bucket %s:\n%s", bucket, basePage)
	}

	for _, tc := range []struct {
		name  string
		query string
		param string
		where string
		args  []any
	}{
		{"search a job name", "q=lint", "q=lint",
			`j.name ILIKE '%lint%' OR coalesce(j.workflow_name, r.workflow_name, '') ILIKE '%lint%' OR coalesce(j.head_branch, r.head_branch, '') ILIKE '%lint%'`, nil},
		{"search a branch", "q=main", "q=main",
			`j.name ILIKE '%main%' OR coalesce(j.workflow_name, r.workflow_name, '') ILIKE '%main%' OR coalesce(j.head_branch, r.head_branch, '') ILIKE '%main%'`, nil},
		{"search ignores case", "q=DEPLOY", "q=DEPLOY",
			`j.name ILIKE '%deploy%' OR coalesce(j.workflow_name, r.workflow_name, '') ILIKE '%deploy%' OR coalesce(j.head_branch, r.head_branch, '') ILIKE '%deploy%'`, nil},
		{"conclusion", "outcome=failure", "outcome=failure", `j.status = 'completed' AND j.conclusion = 'failure'`, nil},
		{"other conclusions", "outcome=other", "outcome=other",
			`j.status = 'completed' AND j.conclusion NOT IN ('success', 'failure', 'cancelled')`, nil},
		{"workflow", "workflow=Deploy", "workflow=Deploy", `coalesce(j.workflow_name, r.workflow_name, '') = 'Deploy'`, nil},
		{"branch", "branch=main", "branch=main", `coalesce(j.head_branch, r.head_branch, '') = 'main'`, nil},
		{"bucket", "bucket=" + url.QueryEscape(bucket), "bucket=" + url.QueryEscape(bucket),
			runsJobBegan + ` >= $4 AND ` + runsJobBegan + ` < $5`, []any{day, day.AddDate(0, 0, 1)}},
		{"search and conclusion", "q=lint&outcome=success", "outcome=success",
			`j.status = 'completed' AND j.conclusion = 'success' AND j.name ILIKE '%lint%'`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := runsQueryIDs(t, st, since, now, "acme/api", tc.where, runsNewestFirst, tc.args...)
			if len(want) == 0 || len(want) >= len(all) {
				t.Fatalf("SQL selects %d of %d jobs; the case does not narrow", len(want), len(all))
			}
			target := base + "&" + tc.query
			page := get(t, h, target).Body.String()
			if got := runsJobIDs(page); !reflect.DeepEqual(got, want[:min(len(want), store.RunsPageSize)]) {
				t.Errorf("table lists %v, SQL selects %v", got, want)
			}
			if n := runsShown(t, page); n != len(want) {
				t.Errorf("page says %d jobs, SQL selects %d", n, len(want))
			}

			body := get(t, h, target+"&datastar=%7B%7D", "Datastar-Request", "true").Body.String()
			script := body[strings.Index(body, "history.replaceState"):]
			if !strings.Contains(script, tc.param) {
				t.Errorf("the address bar script does not carry %s:\n%s", tc.param, script)
			}
		})
	}

	t.Run("form shows the filters", func(t *testing.T) {
		page := get(t, h, base+"&q=lint&outcome=failure&workflow=CI&sort=duration&dir=asc").Body.String()
		for _, want := range []string{
			`name="q" form="filters" value="lint"`,
			`<input type="radio" form="filters" name="outcome" value="failure" checked>`,
			`<input type="hidden" form="filters" name="workflow" value="CI">`,
			`<input type="hidden" form="filters" name="sort" value="duration">`,
			`<input type="hidden" form="filters" name="dir" value="asc">`,
			"Workflow: CI",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("page is missing %q", want)
			}
		}
	})
}

func TestRunsSortOrders(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)
	since := now.AddDate(0, 0, -30)

	for _, tc := range []struct {
		query, order string
	}{
		{"", runsNewestFirst},
		{"sort=started&dir=asc", runsJobBegan + ` ASC, j.id DESC`},
		{"sort=repo&dir=asc", `j.repository ASC, ` + runsNewestFirst},
		{"sort=job&dir=desc", `coalesce(j.name, '') DESC, ` + runsNewestFirst},
		{"sort=duration", `extract(epoch FROM j.completed_at - j.started_at) DESC NULLS LAST, ` + runsNewestFirst},
		{"sort=queue&dir=asc", `CASE WHEN j.started_at >= j.created_at THEN extract(epoch FROM j.started_at - j.created_at) END ASC NULLS LAST, ` + runsNewestFirst},
		{"sort=samples", `(SELECT count(DISTINCT m.ts) FROM samples m WHERE m.job_id = j.id) DESC, ` + runsNewestFirst},
		{"sort=nonsense", runsNewestFirst},
	} {
		t.Run(tc.query, func(t *testing.T) {
			want := runsQueryIDs(t, st, since, now, "", "", tc.order)
			page := get(t, h, "/stats/runs?range=30d&"+tc.query).Body.String()
			if got := runsJobIDs(page); !reflect.DeepEqual(got, want[:min(len(want), store.RunsPageSize)]) {
				t.Errorf("table order %v, SQL order %v", got, want[:min(len(want), store.RunsPageSize)])
			}
		})
	}

	page := get(t, h, "/stats/runs?range=30d&sort=repo&dir=asc").Body.String()
	head := page[strings.Index(page, "<thead>"):strings.Index(page, "</thead>")]
	var keys []string
	for _, m := range regexp.MustCompile(`sort=(\w+)(?:&amp;[^"]*)?" data-nav`).FindAllStringSubmatch(head, -1) {
		keys = append(keys, m[1])
		if !store.IsRunsSort(m[1]) {
			t.Errorf("header sorts by %q, which the store does not know", m[1])
		}
	}
	if len(keys) != 10 {
		t.Errorf("header has %d sortable columns, want 10: %v", len(keys), keys)
	}
	if !strings.Contains(head, `data-sorted`) || !strings.Contains(head, "↑") {
		t.Errorf("header does not mark the ascending repository column:\n%s", head)
	}
	if !strings.Contains(head, `href="/stats/runs?dir=desc&amp;range=30d&amp;sort=repo"`) {
		t.Errorf("the sorted column does not flip direction:\n%s", head)
	}
}

func TestRunsLinks(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)

	checked := 0
	for _, repo := range fx.Repositories {
		page := get(t, h, "/stats/runs?range=90d&repo="+url.QueryEscape(repo)).Body.String()
		rows := strings.Split(page, "<tr>")[2:]
		if len(rows) == 0 {
			t.Fatalf("no rows for %s", repo)
		}
		for _, row := range rows {
			m := runsJobLink.FindStringSubmatch(row)
			if m == nil {
				t.Fatalf("row has no job link:\n%s", row)
			}
			var (
				path, sha, branch string
				runID             int64
				attempt           int
			)
			err := st.Pool.QueryRow(context.Background(), `
				SELECT coalesce(r.path, ''), j.run_id, j.run_attempt, coalesce(r.head_sha, ''), coalesce(j.head_branch, r.head_branch, '')
				FROM jobs j LEFT JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
				WHERE j.id = $1`, m[1]).Scan(&path, &runID, &attempt, &sha, &branch)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				github.RunURL("https://github.com", repo, runID, attempt),
				github.CommitURL("https://github.com", repo, sha),
				github.BranchURL("https://github.com", repo, branch),
			}
			if path != "" {
				want = append(want, github.WorkflowURL("https://github.com", repo, path))
			}
			for _, w := range want {
				if !strings.Contains(row, `<a href="`+html.EscapeString(w)+`">`) {
					t.Errorf("job %s of %s has no link to %s:\n%s", m[1], repo, w, row)
				}
				checked++
			}
			if path == "" && strings.Contains(row, "/blob/") {
				t.Errorf("job %s links to a workflow it has no path for", m[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no links were checked")
	}
}

func TestRunsSamplesMarkArtifactJobs(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	page := get(t, h, "/stats/runs?range=7d&repo=oss%2Fgauger").Body.String()
	rowOf := func(id int64) string {
		for _, row := range strings.Split(page, "<tr>") {
			if strings.Contains(row, fmt.Sprintf(`href="/stats/jobs/%d"`, id)) {
				return row
			}
		}
		t.Fatalf("job %d is not in the table", id)
		return ""
	}
	samples := func(id int64) string {
		var n int
		err := st.Pool.QueryRow(context.Background(), `SELECT count(DISTINCT ts) FROM samples WHERE job_id = $1`, id).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return chart.Integer(float64(n))
	}
	art, live, none := rowOf(fx.ArtifactJob), rowOf(fx.SampledJob), rowOf(fx.UnsampledJob)
	if n := samples(fx.ArtifactJob); n == "0" || !strings.Contains(art, `>`+n+` <span class="muted">(artifact)</span></td>`) {
		t.Errorf("artifact job should show %s samples and the marker:\n%s", n, art)
	}
	if n := samples(fx.SampledJob); n == "0" || !regexp.MustCompile(`>`+n+`\s*</td></tr>`).MatchString(live) || strings.Contains(live, "(artifact)") {
		t.Errorf("live job should show %s samples and no marker:\n%s", n, live)
	}
	if strings.Contains(none, "(artifact)") || !regexp.MustCompile(`>0\s*</td></tr>`).MatchString(none) {
		t.Errorf("unsampled job should show 0 samples and no marker:\n%s", none)
	}
}

func TestRunsRowsShowTimesAndResult(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	var (
		started, completed time.Time
		created            *time.Time
		conclusion         string
	)
	err := st.Pool.QueryRow(context.Background(),
		`SELECT started_at, completed_at, created_at, conclusion FROM jobs WHERE id = $1`, fx.SampledJob).
		Scan(&started, &completed, &created, &conclusion)
	if err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/stats/runs?range=7d&repo=oss%2Fgauger").Body.String()
	var row string
	for _, r := range strings.Split(page, "<tr>") {
		if strings.Contains(r, fmt.Sprintf(`href="/stats/jobs/%d"`, fx.SampledJob)) {
			row = r
		}
	}
	for _, want := range []string{
		started.UTC().Format("2006-01-02 15:04"),
		chart.Seconds(completed.Sub(started).Seconds()),
		chart.Seconds(started.Sub(*created).Seconds()),
		`data-tone="ok">` + conclusion + `</span>`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("row is missing %q:\n%s", want, row)
		}
	}

	running := get(t, h, "/stats/runs?range=24h&repo=acme%2Fweb&outcome=running").Body.String()
	if !strings.Contains(running, `data-tone="accent">in_progress</span>`) && !strings.Contains(running, `data-tone="accent">queued</span>`) {
		t.Errorf("no running job in the table:\n%s", running)
	}
}

func TestRunsChartStacksJobsByConclusion(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	h := dashboard(st, now)

	page := get(t, h, "/stats/runs?range=30d&repo=acme%2Fapi").Body.String()
	total := 0
	for _, m := range regexp.MustCompile(`<span class="legend-value">([\d,]+)</span>`).FindAllStringSubmatch(page, -1) {
		n, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
		total += n
	}
	want := len(runsQueryIDs(t, st, now.AddDate(0, 0, -30), now, "acme/api", "", runsNewestFirst))
	if total != want {
		t.Errorf("chart series add up to %d jobs, SQL has %d", total, want)
	}

	hidden := get(t, h, "/stats/runs?range=30d&repo=acme%2Fapi&hide=jobs%3Asuccess").Body.String()
	if !strings.Contains(hidden, `<input type="hidden" form="filters" name="hide" value="jobs:success">`) {
		t.Error("a hidden series is not carried by the filter form")
	}
	if got := runsShown(t, hidden); got != want {
		t.Errorf("hiding a series changed the table to %d jobs, want %d", got, want)
	}
}

func TestRunsEmptyStates(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)

	page := get(t, dashboard(st, fx.Now.AddDate(0, 0, 30)), "/stats/runs?range=7d").Body.String()
	for _, want := range []string{"No jobs match", "No jobs in this range"} {
		if !strings.Contains(page, want) {
			t.Errorf("empty window is missing %q", want)
		}
	}
	if ids := runsJobIDs(page); len(ids) != 0 {
		t.Errorf("empty window lists jobs %v", ids)
	}

	bare := storetest.Open(t, 90*24*time.Hour)
	page = get(t, dashboard(bare, time.Now()), "/stats/runs").Body.String()
	if !strings.Contains(page, `<a href="/setup">Setup</a>`) || !strings.Contains(page, "No jobs yet") {
		t.Errorf("a store with no repositories should point at /setup:\n%s", page)
	}
}

func TestHomeRedirectsToRuns(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	h := dashboard(st, time.Now())

	for url, want := range map[string]string{
		"/":                         "/stats/runs",
		"/?repo=acme%2Fapi&days=30": "/stats/runs?repo=acme%2Fapi&days=30",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
			t.Errorf("GET %s = %d to %q, want 302 to %q", url, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}

func TestRunsNavLinkIsActive(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	h := dashboard(st, time.Now())
	page := get(t, h, "/stats/runs?repo=acme%2Fapi").Body.String()
	if !strings.Contains(page, `class="nav-row" href="/stats/runs?repo=acme%2Fapi" data-active="true"`) {
		t.Errorf("the sidebar has no Runs link carrying the repository:\n%s", page)
	}
}
