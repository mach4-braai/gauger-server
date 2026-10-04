package ui_test

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// wasteWindow is the SQL predicate for jobs of alias a inside the window
// that $1 to $4 name: since, until, repository and event.
func wasteWindow(a string) string {
	return fmt.Sprintf(`coalesce(%[1]s.started_at, %[1]s.created_at, %[1]s.runner_seen_at) >= $1
		AND coalesce(%[1]s.started_at, %[1]s.created_at, %[1]s.runner_seen_at) < $2
		AND ($3 = '' OR %[1]s.repository = $3)
		AND ($4 = '' OR EXISTS (SELECT 1 FROM runs r WHERE r.id = %[1]s.run_id AND r.attempt = %[1]s.run_attempt AND r.event = $4))`, a)
}

// wasteExpected computes the Waste cards with its own SQL over the
// fixtures.
func wasteExpected(t *testing.T, st *store.Store, since, until time.Time, repo, event string) map[string]string {
	t.Helper()
	ctx := context.Background()
	args := []any{since, until, repo, event}
	count := func(query string) int64 {
		t.Helper()
		var n int64
		if err := st.Pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	wastedWhere := `j.status = 'completed' AND j.conclusion = '%s' AND j.started_at IS NOT NULL AND j.completed_at > j.started_at AND ` + wasteWindow("j")
	minutes := func(conclusion string) int64 {
		return count(`SELECT coalesce(sum(ceil(extract(epoch FROM j.completed_at - j.started_at) / 60)), 0)::bigint FROM jobs j WHERE ` +
			fmt.Sprintf(wastedWhere, conclusion))
	}
	failed, cancelled := minutes("failure"), minutes("cancelled")

	rows, err := st.Pool.Query(ctx, `SELECT j.labels, p.private, sum(ceil(extract(epoch FROM j.completed_at - j.started_at) / 60))::bigint
		FROM jobs j LEFT JOIN repositories p ON p.full_name = j.repository
		WHERE j.conclusion IN ('failure', 'cancelled') AND j.status = 'completed' AND j.started_at IS NOT NULL AND j.completed_at > j.started_at AND `+wasteWindow("j")+`
		GROUP BY 1, 2`, args...)
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.0
	for rows.Next() {
		var labels []string
		var private *bool
		var m int64
		if err := rows.Scan(&labels, &private, &m); err != nil {
			t.Fatal(err)
		}
		cost += spend.Rates(spend.DefaultRates).Price(labels, private, m).Cost
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	reRuns := count(`SELECT count(*) FROM runs r WHERE r.attempt > 1
		AND coalesce(r.run_started_at, r.created_at) >= $1 AND coalesce(r.run_started_at, r.created_at) < $2
		AND ($3 = '' OR r.repository = $3) AND ($4 = '' OR r.event = $4)`)
	reRunJobs := count(`SELECT count(*) FROM jobs j WHERE j.run_attempt > 1 AND ` + wasteWindow("j"))
	flaky := count(`SELECT count(*) FROM (
		SELECT DISTINCT p.repository, coalesce(p.workflow_name, ''), p.name, pr.head_sha
		FROM jobs p JOIN runs pr ON pr.id = p.run_id AND pr.attempt = p.run_attempt
		WHERE p.status = 'completed' AND p.conclusion = 'success' AND ` + wasteWindow("p") + `
		  AND EXISTS (
			SELECT 1 FROM jobs x JOIN runs xr ON xr.id = x.run_id AND xr.attempt = x.run_attempt
			WHERE x.status = 'completed' AND x.conclusion = 'failure' AND x.completed_at < p.completed_at
			  AND x.repository = p.repository AND coalesce(x.workflow_name, '') = coalesce(p.workflow_name, '')
			  AND x.name = p.name AND xr.head_sha = pr.head_sha AND ` + wasteWindow("x") + `)
		) k`)

	return map[string]string{
		"wasted-minutes":      chart.Integer(float64(failed + cancelled)),
		"wasted-minutes-hint": chart.Integer(float64(failed)) + " failed · " + chart.Integer(float64(cancelled)) + " cancelled",
		"wasted-spend":        chart.USD(cost),
		"reruns":              chart.Integer(float64(reRuns)),
		"reruns-hint":         strconv.FormatInt(reRunJobs, 10) + " job re-runs",
		"flaky":               chart.Integer(float64(flaky)),
	}
}

func TestWasteCardsMatchSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range []struct {
		name, url   string
		now         time.Time
		since       time.Time
		repo, event string
		empty       bool
	}{
		{name: "full range", url: "/stats/waste?range=all", now: now},
		{name: "one repository", url: "/stats/waste?range=90d&repo=acme%2Fapi", now: now, since: now.Add(-90 * 24 * time.Hour), repo: "acme/api"},
		{name: "one event", url: "/stats/waste?range=30d&event=push", now: now, since: now.Add(-30 * 24 * time.Hour), event: "push"},
		{name: "empty window", url: "/stats/waste?range=7d", now: now.AddDate(0, 0, 30), since: now.AddDate(0, 0, 23), empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			got := cards(page)
			want := wasteExpected(t, st, tc.since, tc.now, tc.repo, tc.event)
			if !tc.empty {
				for _, id := range []string{"wasted-minutes", "reruns", "flaky"} {
					if want[id] == "0" {
						t.Fatalf("fixtures have no %s for %s; the case checks nothing", id, tc.url)
					}
				}
			} else if want["wasted-minutes"] != "0" || want["reruns"] != "0" || want["flaky"] != "0" {
				t.Fatalf("window is not empty: %v", want)
			}
			if tc.name == "full range" && want["wasted-spend"] == "$0" {
				t.Fatal("fixtures have no priced waste; the spend card checks nothing")
			}
			for id, w := range want {
				switch {
				case strings.HasSuffix(id, "-hint"):
					if g := got[strings.TrimSuffix(id, "-hint")][1]; g != w {
						t.Errorf("%s = %q, want %q", id, g, w)
					}
				default:
					if g := got[id][0]; g != w {
						t.Errorf("card %s = %q, want %q", id, g, w)
					}
				}
			}

			total := 0
			for _, m := range regexp.MustCompile(`<span class="legend-value">([\d,]+) min</span>`).FindAllStringSubmatch(page, -1) {
				n, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
				total += n
			}
			if strconv.Itoa(total) != strings.ReplaceAll(want["wasted-minutes"], ",", "") {
				t.Errorf("chart series add up to %d min, want the wasted minutes card's %s", total, want["wasted-minutes"])
			}
			if tc.empty != strings.Contains(page, "No wasted minutes in this range") {
				t.Errorf("empty chart label shown = %v, want %v", !tc.empty, tc.empty)
			}
			if tc.empty && (!strings.Contains(page, "No flaky candidates in this range") || !strings.Contains(page, "No re-runs in this range")) {
				t.Error("empty tables do not say so")
			}
			if !strings.Contains(page, "re-runs before 2026-09-30 are missing") {
				t.Error("page does not note the missing re-runs")
			}
		})
	}
}

// wasteTable returns the HTML of the table with the given id.
func wasteTable(t *testing.T, page, id string) string {
	t.Helper()
	start := strings.Index(page, `<table id="`+id+`"`)
	if start < 0 {
		t.Fatalf("no table %s in the page", id)
	}
	end := strings.Index(page[start:], "</table>")
	return page[start : start+end]
}

func TestWasteFlakyCandidates(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	ctx := context.Background()
	page := get(t, dashboard(st, fx.Now), "/stats/waste?range=30d").Body.String()
	flaky := wasteTable(t, page, "waste-flaky")

	jobID := func(query string, args ...any) int64 {
		t.Helper()
		var id int64
		if err := st.Pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return id
	}
	jobOf := `SELECT j.id FROM jobs j JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
		WHERE r.head_sha = $1 AND j.name = 'test' AND j.conclusion = $2`

	t.Run("failing then passing in another run", func(t *testing.T) {
		failed, passed := jobID(jobOf, fx.FlakySHA, "failure"), jobID(jobOf, fx.FlakySHA, "success")
		for _, want := range []string{
			"/commit/" + fx.FlakySHA + `"`,
			`href="/stats/jobs/` + strconv.FormatInt(failed, 10) + `"`,
			`href="/stats/jobs/` + strconv.FormatInt(passed, 10) + `"`,
		} {
			if !strings.Contains(flaky, want) {
				t.Errorf("flaky table is missing %s", want)
			}
		}
	})

	t.Run("failing then passing in the next attempt", func(t *testing.T) {
		var sha string
		if err := st.Pool.QueryRow(ctx, `SELECT head_sha FROM runs WHERE id = $1 AND attempt = 1`, fx.ReRun).Scan(&sha); err != nil {
			t.Fatal(err)
		}
		failed, passed := jobID(jobOf, sha, "failure"), jobID(jobOf, sha, "success")
		for _, want := range []string{
			"/commit/" + sha + `"`,
			`href="/stats/jobs/` + strconv.FormatInt(failed, 10) + `"`,
			`href="/stats/jobs/` + strconv.FormatInt(passed, 10) + `"`,
		} {
			if !strings.Contains(flaky, want) {
				t.Errorf("flaky table is missing %s", want)
			}
		}
	})

	t.Run("failing twice is not a candidate", func(t *testing.T) {
		if strings.Contains(flaky, fx.FailedTwiceSHA) {
			t.Errorf("flaky table lists %s, which failed on both attempts", fx.FailedTwiceSHA)
		}
		var failures int
		if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs j JOIN runs r ON r.id = j.run_id AND r.attempt = j.run_attempt
			WHERE r.head_sha = $1 AND j.name = 'test' AND j.conclusion = 'failure'`, fx.FailedTwiceSHA).Scan(&failures); err != nil || failures != 2 {
			t.Fatalf("fixture has %d failing test jobs for the SHA, want 2 (err %v)", failures, err)
		}
		reruns := wasteTable(t, page, "waste-reruns")
		var failedTwice int64
		if err := st.Pool.QueryRow(ctx, `SELECT j.id FROM jobs j WHERE j.run_id = (SELECT id FROM runs WHERE head_sha = $1 AND attempt = 1)
			AND j.run_attempt = 2 AND j.name = 'test'`, fx.FailedTwiceSHA).Scan(&failedTwice); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(reruns, `href="/stats/jobs/`+strconv.FormatInt(failedTwice, 10)+`"`) {
			t.Error("the re-run table should list the job that failed again")
		}
	})
}

func TestWasteReRunsLinkBothAttempts(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	ctx := context.Background()
	reruns := wasteTable(t, get(t, dashboard(st, fx.Now), "/stats/waste?range=7d&repo=acme%2Fapi").Body.String(), "waste-reruns")

	var first, second int64
	if err := st.Pool.QueryRow(ctx, `SELECT
		(SELECT id FROM jobs WHERE run_id = $1 AND run_attempt = 1 AND name = 'test'),
		(SELECT id FROM jobs WHERE run_id = $1 AND run_attempt = 2 AND name = 'test')`, fx.ReRun).Scan(&first, &second); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first, second} {
		if !strings.Contains(reruns, `href="/stats/jobs/`+strconv.FormatInt(id, 10)+`"`) {
			t.Errorf("re-run table is missing a link to job %d", id)
		}
	}
}

func TestWasteDatastarRequestPatchesPage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	rec := get(t, dashboard(st, fx.Now), "/stats/waste?range=30d&datastar=%7B%7D", "Datastar-Request", "true")
	body := rec.Body.String()
	for _, want := range []string{`data: elements <div id="page"`, `id="stat-wasted-minutes"`, `id="waste-flaky"`} {
		if !strings.Contains(body, want) {
			t.Errorf("patch is missing %q", want)
		}
	}
}
