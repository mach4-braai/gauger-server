package ui_test

import (
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

const (
	failuresRunWindow = `coalesce(r.run_started_at, r.created_at) >= $1 AND coalesce(r.run_started_at, r.created_at) < $2
		AND ($3 = '' OR r.repository = $3) AND ($4 = '' OR r.event = $4)`
	failuresJobWindow = `coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1 AND coalesce(j.started_at, j.created_at, j.runner_seen_at) < $2
		AND ($3 = '' OR j.repository = $3)
		AND ($4 = '' OR EXISTS (SELECT 1 FROM runs r WHERE r.id = j.run_id AND r.attempt = j.run_attempt AND r.event = $4))`
	failuresRowLimit = 15
)

var (
	failuresRowRE  = regexp.MustCompile(`(?s)<tr data-([a-z-]+)="([^"]*)"(?: data-workflow="([^"]*)" data-job="([^"]*)")?>(.*?)</tr>`)
	failuresCellRE = regexp.MustCompile(`(?s)data-col="(\w+)">(.*?)</td>`)
	failuresTagRE  = regexp.MustCompile(`<[^>]*>`)
)

// failuresPageRow is one table row of the failures page.
type failuresPageRow struct {
	key   string
	cells map[string]string
	html  string
}

// failuresRows reads the rows whose first attribute is data-<attr>. Job
// rows key on repository, workflow and job name.
func failuresRows(page, attr string) []failuresPageRow {
	var rows []failuresPageRow
	for _, m := range failuresRowRE.FindAllStringSubmatch(page, -1) {
		if m[1] != attr {
			continue
		}
		key := html.UnescapeString(m[2])
		if attr == "repo" {
			key += "|" + html.UnescapeString(m[3]) + "|" + html.UnescapeString(m[4])
		}
		row := failuresPageRow{key: key, cells: map[string]string{}, html: m[5]}
		for _, c := range failuresCellRE.FindAllStringSubmatch(m[5], -1) {
			row.cells[c[1]] = strings.Join(strings.Fields(html.UnescapeString(failuresTagRE.ReplaceAllString(c[2], " "))), " ")
		}
		rows = append(rows, row)
	}
	return rows
}

func failuresRatio(n, of int64) string {
	if of == 0 {
		return "–"
	}
	return chart.Percent(float64(n) / float64(of))
}

// failuresCount runs a single-count query over the window arguments.
func failuresCount(t *testing.T, st *store.Store, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.Pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

type failuresCase struct {
	name, url   string
	now         time.Time
	since       time.Time
	repo, event string
}

func failuresCases(now time.Time) []failuresCase {
	return []failuresCase{
		{name: "full range", url: "/stats/failures?range=all", now: now},
		{name: "one repository", url: "/stats/failures?range=90d&repo=acme%2Fapi", now: now, since: now.Add(-90 * 24 * time.Hour), repo: "acme/api"},
		{name: "one event", url: "/stats/failures?range=30d&event=push", now: now, since: now.Add(-30 * 24 * time.Hour), event: "push"},
		{name: "one day", url: "/stats/failures?range=24h", now: now, since: now.Add(-24 * time.Hour)},
	}
}

func TestFailuresPerJobMatchSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range failuresCases(now) {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			args := []any{tc.since, tc.now, tc.repo, tc.event}

			type want struct{ failures, runs int64 }
			rows, err := st.Pool.Query(context.Background(), `
				SELECT j.repository, coalesce(j.workflow_name, ''), coalesce(j.name, ''),
					count(*) FILTER (WHERE j.conclusion = 'failure'),
					count(*) FILTER (WHERE j.status = 'completed' AND j.conclusion NOT IN ('cancelled', 'skipped'))
				FROM jobs j WHERE `+failuresJobWindow+`
				GROUP BY 1, 2, 3
				HAVING count(*) FILTER (WHERE j.conclusion = 'failure') > 0
				ORDER BY 4 DESC`, args...)
			if err != nil {
				t.Fatal(err)
			}
			wants := map[string]want{}
			var order []int64
			for rows.Next() {
				var repo, workflow, name string
				var w want
				if err := rows.Scan(&repo, &workflow, &name, &w.failures, &w.runs); err != nil {
					t.Fatal(err)
				}
				wants[repo+"|"+workflow+"|"+name] = w
				order = append(order, w.failures)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(wants) == 0 {
				t.Fatalf("fixtures have no failed jobs for %s; the case checks nothing", tc.url)
			}

			got := failuresRows(page, "repo")
			if len(got) != min(failuresRowLimit, len(wants)) {
				t.Fatalf("page has %d job rows, SQL has %d failing jobs", len(got), len(wants))
			}
			for i, g := range got {
				w, ok := wants[g.key]
				if !ok {
					t.Fatalf("row %q has no failures in SQL", g.key)
				}
				for col, wantCell := range map[string]string{
					"failures": chart.Integer(float64(w.failures)),
					"runs":     chart.Integer(float64(w.runs)),
					"rate":     failuresRatio(w.failures, w.runs),
				} {
					if g.cells[col] != wantCell {
						t.Errorf("%s %s = %q, want %q", g.key, col, g.cells[col], wantCell)
					}
				}
				if g.cells["failures"] != chart.Integer(float64(order[i])) {
					t.Errorf("row %d (%s) has %s failures, want %d: most failures first", i, g.key, g.cells["failures"], order[i])
				}
				if !strings.Contains(g.html, `href="/stats/jobs/`) {
					t.Errorf("%s has no link to its latest failed job:\n%s", g.key, g.html)
				}
			}

			c := cards(page)
			failedJobs := failuresCount(t, st, `SELECT count(*) FROM jobs j WHERE j.conclusion = 'failure' AND `+failuresJobWindow, args...)
			if c["failed-jobs"][0] != chart.Integer(float64(failedJobs)) {
				t.Errorf("failed jobs card = %q, want %d", c["failed-jobs"][0], failedJobs)
			}
			var sum int64
			for _, w := range wants {
				sum += w.failures
			}
			if sum != failedJobs {
				t.Errorf("per-job failures add up to %d, want the %d failed jobs", sum, failedJobs)
			}
			decided := failuresCount(t, st, `SELECT count(*) FROM jobs j WHERE j.status = 'completed' AND j.conclusion NOT IN ('cancelled', 'skipped') AND `+failuresJobWindow, args...)
			if c["job-failure-rate"][0] != failuresRatio(failedJobs, decided) {
				t.Errorf("job failure rate card = %q, want %s", c["job-failure-rate"][0], failuresRatio(failedJobs, decided))
			}
		})
	}
}

func TestFailuresPerStepMatchSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range failuresCases(now) {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			rows, err := st.Pool.Query(context.Background(), `
				SELECT regexp_replace(s.name, '@\S+$', ''),
					count(*) FILTER (WHERE s.conclusion = 'failure'),
					count(*) FILTER (WHERE s.conclusion IS DISTINCT FROM 'cancelled')
				FROM steps s JOIN jobs j ON j.id = s.job_id
				WHERE `+failuresJobWindow+`
				  AND s.started_at IS NOT NULL AND s.completed_at IS NOT NULL AND s.conclusion IS DISTINCT FROM 'skipped'
				GROUP BY 1
				HAVING count(*) FILTER (WHERE s.conclusion = 'failure') > 0
				ORDER BY 2 DESC`, tc.since, tc.now, tc.repo, tc.event)
			if err != nil {
				t.Fatal(err)
			}
			type want struct{ failures, runs int64 }
			wants := map[string]want{}
			var order []int64
			for rows.Next() {
				var name string
				var w want
				if err := rows.Scan(&name, &w.failures, &w.runs); err != nil {
					t.Fatal(err)
				}
				wants[name] = w
				order = append(order, w.failures)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(wants) == 0 {
				t.Fatalf("fixtures have no failed steps for %s; the case checks nothing", tc.url)
			}

			got := failuresRows(page, "failing-step")
			if len(got) != min(failuresRowLimit, len(wants)) {
				t.Fatalf("page has %d step rows, SQL has %d failing steps", len(got), len(wants))
			}
			for i, g := range got {
				w, ok := wants[g.key]
				if !ok {
					t.Fatalf("row %q has no failures in SQL", g.key)
				}
				if strings.Contains(g.key, "@") {
					t.Errorf("row %q still has a ref", g.key)
				}
				for col, wantCell := range map[string]string{
					"failures": chart.Integer(float64(w.failures)),
					"runs":     chart.Integer(float64(w.runs)),
					"rate":     failuresRatio(w.failures, w.runs),
				} {
					if g.cells[col] != wantCell {
						t.Errorf("%s %s = %q, want %q", g.key, col, g.cells[col], wantCell)
					}
				}
				if g.cells["failures"] != chart.Integer(float64(order[i])) {
					t.Errorf("row %d (%s) has %s failures, want %d: most failures first", i, g.key, g.cells["failures"], order[i])
				}
				if !strings.Contains(g.html, `href="/stats/jobs/`) || !strings.Contains(g.html, "/job/") {
					t.Errorf("%s has no links to its latest job and its step log:\n%s", g.key, g.html)
				}
			}
		})
	}
}

func TestFailedJobListsItsFirstFailedStep(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	page := get(t, dashboard(st, now), "/stats/failures?range=24h").Body.String()
	recent := failuresRows(page, "failed-job")

	want := failuresCount(t, st, `SELECT count(*) FROM jobs j WHERE j.conclusion = 'failure' AND `+failuresJobWindow, now.Add(-24*time.Hour), now, "", "")
	if int64(len(recent)) != min(want, int64(store.RecentFailureLimit)) {
		t.Fatalf("page lists %d recent failures, SQL has %d failed jobs (limit %d)", len(recent), want, store.RecentFailureLimit)
	}
	for _, r := range recent {
		id, _ := strconv.ParseInt(r.key, 10, 64)
		var number int
		var name *string
		err := st.Pool.QueryRow(context.Background(),
			`SELECT number, regexp_replace(name, '@\S+$', '') FROM steps WHERE job_id = $1 AND conclusion = 'failure' ORDER BY number LIMIT 1`, id).Scan(&number, &name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if name == nil {
			if r.cells["cause"] != "No failed step recorded" {
				t.Errorf("job %d has no failed step, but its cause reads %q", id, r.cells["cause"])
			}
			continue
		}
		if r.cells["cause"] != *name+" step "+strconv.Itoa(number) {
			t.Errorf("job %d cause = %q, want %q (step %d)", id, r.cells["cause"], *name, number)
		}
		if link := fmt.Sprintf("/job/%d#step:%d:1", id, number); !strings.Contains(r.html, link) {
			t.Errorf("job %d has no GitHub log link %s:\n%s", id, link, r.html)
		}
		if link := fmt.Sprintf(`href="/stats/jobs/%d"`, id); !strings.Contains(r.html, link) {
			t.Errorf("job %d has no %s link:\n%s", id, link, r.html)
		}
	}

	var migrate, backfill int64
	for name, id := range map[string]*int64{"migrate": &migrate, "backfill": &backfill} {
		err := st.Pool.QueryRow(context.Background(), `SELECT id FROM jobs WHERE name = $1 AND conclusion = 'failure'`, name).Scan(id)
		if err != nil {
			t.Fatalf("fixture job %s: %v", name, err)
		}
	}
	byJob := map[string]failuresPageRow{}
	for _, r := range recent {
		byJob[r.key] = r
	}
	m, ok := byJob[strconv.FormatInt(migrate, 10)]
	if !ok {
		t.Fatalf("the migrate job %d is not among the recent failures", migrate)
	}
	if !strings.HasPrefix(m.cells["cause"], "Run actions/checkout step 2") {
		t.Errorf("migrate failed in steps 2 and 4; cause = %q, want the first, Run actions/checkout step 2", m.cells["cause"])
	}
	if b := byJob[strconv.FormatInt(backfill, 10)]; b.cells["cause"] != "No failed step recorded" {
		t.Errorf("backfill has no failed step; cause = %q", b.cells["cause"])
	}
}

func TestFailuresCancelledRunsCountApart(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range failuresCases(now) {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			args := []any{tc.since, tc.now, tc.repo, tc.event}
			failed := failuresCount(t, st, `SELECT count(*) FROM runs r WHERE r.conclusion = 'failure' AND `+failuresRunWindow, args...)
			cancelled := failuresCount(t, st, `SELECT count(*) FROM runs r WHERE r.conclusion = 'cancelled' AND `+failuresRunWindow, args...)
			decided := failuresCount(t, st, `SELECT count(*) FROM runs r WHERE r.status = 'completed' AND r.conclusion NOT IN ('cancelled', 'skipped') AND `+failuresRunWindow, args...)
			failedJobs := failuresCount(t, st, `SELECT count(*) FROM jobs j WHERE j.conclusion = 'failure' AND `+failuresJobWindow, args...)
			cancelledJobs := failuresCount(t, st, `SELECT count(*) FROM jobs j WHERE j.conclusion = 'cancelled' AND `+failuresJobWindow, args...)
			if failed == 0 || cancelled == 0 {
				t.Fatalf("fixtures have %d failed and %d cancelled runs for %s; the case checks nothing", failed, cancelled, tc.url)
			}

			c := cards(page)
			for id, w := range map[string]string{
				"failed-runs":      chart.Integer(float64(failed)),
				"cancelled-runs":   chart.Integer(float64(cancelled)),
				"run-failure-rate": failuresRatio(failed, decided),
				"failed-jobs":      chart.Integer(float64(failedJobs)),
				"cancelled-jobs":   chart.Integer(float64(cancelledJobs)),
			} {
				if c[id][0] != w {
					t.Errorf("card %s = %q, want %q", id, c[id][0], w)
				}
			}

			legend := func(label string) string {
				m := regexp.MustCompile(`title="` + label + `"[^>]*>\s*<span class="swatch"[^>]*></span>\s*<span class="truncate">` + label + `</span>\s*<span class="legend-value">([^<]*)</span>`).FindStringSubmatch(page)
				if m == nil {
					t.Fatalf("legend has no %s entry", label)
				}
				return m[1]
			}
			if got := legend("Failed"); got != chart.Integer(float64(failed)) {
				t.Errorf("legend Failed = %q, want %d failed runs without the cancelled ones", got, failed)
			}
			if got := legend("Cancelled"); got != chart.Integer(float64(cancelled)) {
				t.Errorf("legend Cancelled = %q, want %d", got, cancelled)
			}
			if got := legend("Failure rate"); got != failuresRatio(failed, decided) {
				t.Errorf("legend Failure rate = %q, want %s", got, failuresRatio(failed, decided))
			}
		})
	}
}

func TestFailuresPageEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.AddDate(0, 0, 30)), "/stats/failures?range=7d").Body.String()

	c := cards(page)
	for id, want := range map[string]string{
		"failed-runs": "0", "cancelled-runs": "0", "run-failure-rate": "–",
		"failed-jobs": "0", "cancelled-jobs": "0", "job-failure-rate": "–",
	} {
		if c[id][0] != want {
			t.Errorf("card %s = %q, want %q", id, c[id][0], want)
		}
	}
	for _, attr := range []string{"repo", "failing-step", "failed-job"} {
		if n := len(failuresRows(page, attr)); n != 0 {
			t.Errorf("empty window has %d %s rows", n, attr)
		}
	}
	for _, want := range []string{
		"No failed or cancelled runs in this range",
		"No failed jobs in this range",
		"No failed steps in this range",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestFailuresPageIsInTheNavAndPatches(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)

	if page := get(t, h, "/stats/failures").Body.String(); !strings.Contains(page, `href="/stats/failures"`) {
		t.Error("the sidebar has no link to the failures page")
	}
	body := get(t, h, "/stats/failures?range=30d&datastar=%7B%7D", "Datastar-Request", "true").Body.String()
	for _, want := range []string{`data: elements <div id="page"`, `id="stat-failed-runs"`, `history.replaceState(null, '', "/stats/failures?range=30d")`} {
		if !strings.Contains(body, want) {
			t.Errorf("patch is missing %q", want)
		}
	}
}
