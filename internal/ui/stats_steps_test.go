package ui_test

import (
	"context"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// stepsPageRow is one row of the steps table as rendered.
type stepsPageRow struct {
	name  string
	cells map[string]string
	html  string
}

var (
	stepsRowRE  = regexp.MustCompile(`(?s)<tr data-step="([^"]*)">(.*?)</tr>`)
	stepsCellRE = regexp.MustCompile(`(?s)data-col="(\w+)">(.*?)</td>`)
	stepsTagRE  = regexp.MustCompile(`<[^>]*>`)
)

func stepsPageRows(page string) []stepsPageRow {
	var rows []stepsPageRow
	for _, m := range stepsRowRE.FindAllStringSubmatch(page, -1) {
		row := stepsPageRow{name: html.UnescapeString(m[1]), cells: map[string]string{}, html: m[2]}
		for _, c := range stepsCellRE.FindAllStringSubmatch(m[2], -1) {
			row.cells[c[1]] = strings.Join(strings.Fields(html.UnescapeString(stepsTagRE.ReplaceAllString(c[2], " "))), " ")
		}
		rows = append(rows, row)
	}
	return rows
}

// stepsMinutes formats seconds as the page does: minutes, with a decimal
// below ten.
func stepsMinutes(secs float64) string {
	if m := secs / 60; m < 9.95 {
		return strconv.FormatFloat(m, 'f', 1, 64)
	}
	return chart.Integer(secs / 60)
}

// expectedStep is one step name's numbers from SQL over the fixtures.
type expectedStep struct {
	name                 string
	runs, failures       int64
	secs, p50, p95, slow float64
	slowJob              int64
	slowNumber           int
}

// expectedSteps groups the steps that ran in the window by name with the
// ref after "@" removed, most seconds first.
func expectedSteps(t *testing.T, st *store.Store, since, until time.Time, repo, event string) []expectedStep {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `
		WITH ran AS (
			SELECT regexp_replace(s.name, '@\S+$', '') AS name, s.job_id, s.number, s.conclusion,
				extract(epoch FROM s.completed_at - s.started_at)::float8 AS secs
			FROM steps s JOIN jobs j ON j.id = s.job_id
			WHERE coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1
			  AND coalesce(j.started_at, j.created_at, j.runner_seen_at) < $2
			  AND ($3 = '' OR j.repository = $3)
			  AND ($4 = '' OR EXISTS (SELECT 1 FROM runs r WHERE r.id = j.run_id AND r.attempt = j.run_attempt AND r.event = $4))
			  AND s.started_at IS NOT NULL AND s.completed_at IS NOT NULL AND s.conclusion IS DISTINCT FROM 'skipped'
		)
		SELECT name, count(*), count(*) FILTER (WHERE conclusion = 'failure'), sum(secs),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY secs), percentile_cont(0.95) WITHIN GROUP (ORDER BY secs),
			max(secs),
			(SELECT o.job_id FROM ran o WHERE o.name = ran.name ORDER BY o.secs DESC, o.job_id DESC, o.number DESC LIMIT 1),
			(SELECT o.number FROM ran o WHERE o.name = ran.name ORDER BY o.secs DESC, o.job_id DESC, o.number DESC LIMIT 1)
		FROM ran GROUP BY name ORDER BY sum(secs) DESC, name COLLATE "C"`, since, until, repo, event)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []expectedStep
	for rows.Next() {
		var e expectedStep
		if err := rows.Scan(&e.name, &e.runs, &e.failures, &e.secs, &e.p50, &e.p95, &e.slow, &e.slowJob, &e.slowNumber); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStepsPageMatchesSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range []struct {
		name, url   string
		now         time.Time
		since       time.Time
		repo, event string
	}{
		{name: "full range", url: "/stats/steps?range=all", now: now},
		{name: "one repository", url: "/stats/steps?range=90d&repo=acme%2Fapi", now: now, since: now.Add(-90 * 24 * time.Hour), repo: "acme/api"},
		{name: "one event", url: "/stats/steps?range=30d&event=push", now: now, since: now.Add(-30 * 24 * time.Hour), event: "push"},
		{name: "one day", url: "/stats/steps?range=24h", now: now, since: now.Add(-24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			want := expectedSteps(t, st, tc.since, tc.now, tc.repo, tc.event)
			if len(want) == 0 {
				t.Fatalf("fixtures have no steps for %s; the case checks nothing", tc.url)
			}
			got := stepsPageRows(page)
			if len(got) != len(want) {
				t.Fatalf("page has %d rows, SQL has %d", len(got), len(want))
			}
			var total, setup float64
			for i, w := range want {
				g := got[i]
				if g.name != w.name {
					t.Fatalf("row %d is %q, want %q (most minutes first)", i, g.name, w.name)
				}
				if strings.Contains(g.name, "@") {
					t.Errorf("row %q still has a ref", g.name)
				}
				for col, wantCell := range map[string]string{
					"runs":     chart.Integer(float64(w.runs)),
					"total":    stepsMinutes(w.secs),
					"p50":      chart.Seconds(w.p50),
					"p95":      chart.Seconds(w.p95),
					"failures": chart.Integer(float64(w.failures)),
				} {
					if g.cells[col] != wantCell {
						t.Errorf("%s %s = %q, want %q", w.name, col, g.cells[col], wantCell)
					}
				}
				if !strings.HasPrefix(g.cells["slowest"], chart.Seconds(w.slow)+" step "+strconv.Itoa(w.slowNumber)) {
					t.Errorf("%s slowest = %q, want %s in step %d", w.name, g.cells["slowest"], chart.Seconds(w.slow), w.slowNumber)
				}
				if link := fmt.Sprintf(`href="/stats/jobs/%d"`, w.slowJob); !strings.Contains(g.html, link) {
					t.Errorf("%s has no %s link to its slowest occurrence:\n%s", w.name, link, g.html)
				}
				if !strings.Contains(g.html, fmt.Sprintf("/job/%d#step:%d:1", w.slowJob, w.slowNumber)) {
					t.Errorf("%s has no GitHub link to job %d step %d:\n%s", w.name, w.slowJob, w.slowNumber, g.html)
				}
				total += w.secs
				if store.IsSetupStep(w.name) {
					setup += w.secs
					if g.cells["kind"] != "Setup" {
						t.Errorf("%s kind = %q, want Setup", w.name, g.cells["kind"])
					}
				} else if g.cells["kind"] != "Work" {
					t.Errorf("%s kind = %q, want Work", w.name, g.cells["kind"])
				}
			}

			c := cards(page)
			if c["names"][0] != chart.Integer(float64(len(want))) {
				t.Errorf("steps card = %q, want %d", c["names"][0], len(want))
			}
			for id, secs := range map[string]float64{"minutes": total, "setup": setup, "work": total - setup} {
				if c[id][0] != stepsMinutes(secs) {
					t.Errorf("%s card = %q, want %s", id, c[id][0], stepsMinutes(secs))
				}
			}
			if !strings.Contains(page, `class="share-bar"`) || !strings.Contains(page, `id="chart-setupshare"`) {
				t.Error("page has no setup share bar or setup share chart")
			}
			if !strings.Contains(page, `class="bar-list"`) {
				t.Error("page has no bar list of the top steps")
			}
		})
	}
}

func TestStepsPageMergesRefsOfOneAction(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.Add(time.Minute)), "/stats/steps?range=all").Body.String()

	var want struct {
		runs int64
		secs float64
	}
	err := st.Pool.QueryRow(context.Background(), `
		SELECT count(*), sum(extract(epoch FROM completed_at - started_at))::float8 FROM steps
		WHERE name LIKE 'Run jdx/mise-action@%' AND started_at IS NOT NULL AND completed_at IS NOT NULL`).Scan(&want.runs, &want.secs)
	if err != nil {
		t.Fatal(err)
	}
	var distinct int
	err = st.Pool.QueryRow(context.Background(), `SELECT count(DISTINCT name) FROM steps WHERE name LIKE 'Run jdx/mise-action@%'`).Scan(&distinct)
	if err != nil || distinct < 3 {
		t.Fatalf("fixtures have %d refs of jdx/mise-action (%v), want at least 3 to merge", distinct, err)
	}

	var found []stepsPageRow
	for _, r := range stepsPageRows(page) {
		if strings.Contains(r.name, "jdx/mise-action") {
			found = append(found, r)
		}
	}
	if len(found) != 1 || found[0].name != "Run jdx/mise-action" {
		t.Fatalf("mise-action rows = %+v, want one row named Run jdx/mise-action", found)
	}
	if found[0].cells["runs"] != chart.Integer(float64(want.runs)) || found[0].cells["total"] != stepsMinutes(want.secs) {
		t.Errorf("mise-action row = %v, want %d runs and %s minutes summed over every ref", found[0].cells, want.runs, stepsMinutes(want.secs))
	}
	if found[0].cells["kind"] != "Setup" {
		t.Errorf("mise-action kind = %q, want Setup", found[0].cells["kind"])
	}
}

func TestStepsPageSetupAndWorkAddUp(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	c := cards(get(t, dashboard(st, fx.Now.Add(time.Minute)), "/stats/steps?range=90d").Body.String())
	num := func(id string) float64 {
		v, err := strconv.ParseFloat(strings.ReplaceAll(c[id][0], ",", ""), 64)
		if err != nil {
			t.Fatalf("card %s = %q: %v", id, c[id][0], err)
		}
		return v
	}
	step := func(v float64) float64 {
		if v < 9.95 {
			return 0.1
		}
		return 1
	}
	setup, work, total := num("setup"), num("work"), num("minutes")
	if tol := (step(setup) + step(work) + step(total)) / 2; math.Abs(setup+work-total) > tol {
		t.Errorf("setup %v + work %v = %v, want the total %v", c["setup"][0], c["work"][0], setup+work, total)
	}
	if share := c["setup"][1]; !strings.HasSuffix(share, "% of the total") {
		t.Errorf("setup hint = %q, want a share of the total", share)
	}
}

func TestStepsPageSorts(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	names := func(url string) []string {
		var out []string
		for _, r := range stepsPageRows(get(t, h, url).Body.String()) {
			out = append(out, r.name)
		}
		return out
	}
	byTotal := names("/stats/steps?range=all")
	asc := names("/stats/steps?range=all&sort=name&dir=asc")
	desc := names("/stats/steps?range=all&sort=name&dir=desc")
	if len(byTotal) < 5 || len(asc) != len(byTotal) || len(desc) != len(byTotal) {
		t.Fatalf("row counts %d, %d, %d", len(byTotal), len(asc), len(desc))
	}
	for i := 1; i < len(asc); i++ {
		if asc[i-1] > asc[i] {
			t.Errorf("sort=name&dir=asc has %q before %q", asc[i-1], asc[i])
		}
		if desc[i-1] < desc[i] {
			t.Errorf("sort=name&dir=desc has %q before %q", desc[i-1], desc[i])
		}
	}
	if byTotal[0] == asc[0] {
		t.Errorf("name order starts like the default order with %q", byTotal[0])
	}
	if bad := names("/stats/steps?range=all&sort=bogus&dir=sideways"); strings.Join(bad, "|") != strings.Join(byTotal, "|") {
		t.Errorf("unknown sort gives %v, want the default order %v", bad, byTotal)
	}

	page := get(t, h, "/stats/steps?range=all&sort=p95&dir=asc").Body.String()
	for _, want := range []string{
		`data-sorted="asc"`,
		`href="/stats/steps?dir=desc&amp;range=all&amp;sort=p95"`,
		`href="/stats/steps?dir=asc&amp;range=all&amp;sort=name"`,
		`form="filters" name="sort" value="p95"`,
		`form="filters" name="dir" value="asc"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if def := get(t, h, "/stats/steps?range=all").Body.String(); strings.Contains(def, `name="sort"`) {
		t.Error("default order should leave the sort out of the filter form")
	}
}

func TestStepsPageEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.AddDate(0, 0, 30)), "/stats/steps?range=7d").Body.String()
	if n := len(stepsPageRows(page)); n != 0 {
		t.Errorf("empty window has %d rows", n)
	}
	if !strings.Contains(page, "No steps in this range") {
		t.Error("empty window does not say so")
	}
	c := cards(page)
	for _, id := range []string{"names", "setup", "work"} {
		if c[id][0] != "0" && c[id][0] != "0.0" {
			t.Errorf("card %s = %q, want zero", id, c[id][0])
		}
	}
	if !strings.Contains(page, `href="/stats/steps"`) {
		t.Error("the sidebar has no Steps link")
	}
}

func TestStepsDatastarRequestPatchesPage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	rec := get(t, dashboard(st, fx.Now), "/stats/steps?range=30d&sort=runs&datastar=%7B%7D", "Datastar-Request", "true")
	body := rec.Body.String()
	for _, want := range []string{
		`data: elements <div id="page"`,
		`<tr data-step=`,
		`history.replaceState(null, '', "/stats/steps?range=30d\u0026sort=runs")`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("patch is missing %q", want)
		}
	}
}

func TestOldStepsPageRedirects(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	h := dashboard(st, time.Now())
	for url, want := range map[string]string{
		"/steps":                        "/stats/steps",
		"/steps?repo=acme%2Fapi&days=7": "/stats/steps?repo=acme%2Fapi&days=7",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != want {
			t.Errorf("GET %s = %d to %q, want 302 to %q", url, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}
