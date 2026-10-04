package ui_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// datastarSHA256 is the sha256 of the vendored datastar.js v1.0.4,
// recorded in docs/adr/0002-datastar-dashboard.md.
const datastarSHA256 = "727844adfc825ee651fb93c544a2a739986f9a21820a94524b35f0cac470cf91"

// dashboard serves ui.Handler over the fixtures with the clock at now.
func dashboard(st *store.Store, now time.Time) http.Handler {
	return (&ui.UI{Store: st, Creds: st, Rates: spend.DefaultRates, GitHubURL: "https://github.com", Now: func() time.Time { return now }}).Handler()
}

func get(t *testing.T, h http.Handler, url string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", url, rec.Code, rec.Body)
	}
	return rec
}

// cards reads each stat tile's value and hint from a page.
func cards(page string) map[string][2]string {
	out := map[string][2]string{}
	re := regexp.MustCompile(`id="stat-([a-z0-9-]+)"[^>]*><div class="stat-label">[^<]*</div><div class="stat-value">([^<]*)</div>(?:<div class="stat-foot"><span class="truncate">([^<]*)</span>)?`)
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		out[m[1]] = [2]string{m[2], m[3]}
	}
	return out
}

// expectedCards computes every Overview card with its own SQL over the
// fixtures, for the window [since, until), one repository and one event
// (empty for all).
func expectedCards(t *testing.T, st *store.Store, since, until time.Time, repo, event string) map[string]string {
	t.Helper()
	ctx := context.Background()
	runWindow := `coalesce(r.run_started_at, r.created_at) >= $1 AND coalesce(r.run_started_at, r.created_at) < $2
		AND ($3 = '' OR r.repository = $3) AND ($4 = '' OR r.event = $4)`
	jobWindow := `coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1 AND coalesce(j.started_at, j.created_at, j.runner_seen_at) < $2
		AND ($3 = '' OR j.repository = $3)
		AND ($4 = '' OR EXISTS (SELECT 1 FROM runs r WHERE r.id = j.run_id AND r.attempt = j.run_attempt AND r.event = $4))`
	args := []any{since, until, repo, event}
	count := func(query string) int64 {
		t.Helper()
		var n int64
		if err := st.Pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return n
	}
	percentile := func(query string) string {
		t.Helper()
		var v *float64
		if err := st.Pool.QueryRow(ctx, query, args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if v == nil {
			return "–"
		}
		return chart.Seconds(*v)
	}
	share := func(n, of int64) string {
		if of == 0 {
			return "–"
		}
		return chart.Percent(float64(n) / float64(of))
	}

	runs := count(`SELECT count(*) FROM runs r WHERE ` + runWindow)
	jobs := count(`SELECT count(*) FROM jobs j WHERE ` + jobWindow)
	minutes := count(`SELECT coalesce(sum(ceil(extract(epoch FROM j.completed_at - j.started_at) / 60)), 0)::bigint FROM jobs j
		WHERE j.status = 'completed' AND j.completed_at > j.started_at AND ` + jobWindow)

	rows, err := st.Pool.Query(ctx, `SELECT j.labels, p.private, sum(ceil(extract(epoch FROM j.completed_at - j.started_at) / 60))::bigint
		FROM jobs j LEFT JOIN repositories p ON p.full_name = j.repository
		WHERE j.status = 'completed' AND j.completed_at > j.started_at AND `+jobWindow+`
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

	runsDecided := count(`SELECT count(*) FROM runs r WHERE r.status = 'completed' AND r.conclusion NOT IN ('cancelled', 'skipped') AND ` + runWindow)
	runsPassed := count(`SELECT count(*) FROM runs r WHERE r.status = 'completed' AND r.conclusion = 'success' AND ` + runWindow)
	jobsDecided := count(`SELECT count(*) FROM jobs j WHERE j.status = 'completed' AND j.conclusion NOT IN ('cancelled', 'skipped') AND ` + jobWindow)
	jobsPassed := count(`SELECT count(*) FROM jobs j WHERE j.status = 'completed' AND j.conclusion = 'success' AND ` + jobWindow)
	sampled := count(`SELECT count(*) FROM jobs j WHERE EXISTS (SELECT 1 FROM samples m WHERE m.job_id = j.id) AND ` + jobWindow)
	artifact := count(`SELECT count(*) FROM jobs j WHERE j.artifact_ingested_at IS NOT NULL AND EXISTS (SELECT 1 FROM samples m WHERE m.job_id = j.id) AND ` + jobWindow)
	runDurations := `SELECT percentile_cont(%s) WITHIN GROUP (ORDER BY secs) FROM (
		SELECT extract(epoch FROM max(j.completed_at) - min(j.started_at)) AS secs
		FROM runs r JOIN jobs j ON j.run_id = r.id AND j.run_attempt = r.attempt
		WHERE r.status = 'completed' AND j.started_at IS NOT NULL AND j.completed_at IS NOT NULL AND ` + runWindow + `
		GROUP BY r.id, r.attempt) d`
	queue := `SELECT percentile_cont(%s) WITHIN GROUP (ORDER BY extract(epoch FROM j.started_at - j.created_at)) FROM jobs j
		WHERE j.started_at >= j.created_at AND ` + jobWindow

	return map[string]string{
		"runs":        chart.Integer(float64(runs)),
		"jobs":        chart.Integer(float64(jobs)),
		"steps":       chart.Integer(float64(count(`SELECT count(*) FROM steps s JOIN jobs j ON j.id = s.job_id WHERE ` + jobWindow))),
		"minutes":     chart.Integer(float64(minutes)),
		"spend":       chart.USD(cost),
		"run-success": share(runsPassed, runsDecided),
		"job-success": share(jobsPassed, jobsDecided),
		"run-p50":     percentile(strings.Replace(runDurations, "%s", "0.5", 1)),
		"run-p95":     percentile(strings.Replace(runDurations, "%s", "0.95", 1)),
		"queue-p50":   percentile(strings.Replace(queue, "%s", "0.5", 1)),
		"queue-p95":   percentile(strings.Replace(queue, "%s", "0.95", 1)),
		"coverage":    share(sampled, jobs),
		"coverage-hint": chart.Integer(float64(sampled)) + " of " + chart.Integer(float64(jobs)) + " jobs · " +
			chart.Integer(float64(artifact)) + " from artifact",
	}
}

func TestOverviewCardsMatchSQL(t *testing.T) {
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
		{name: "full range", url: "/stats/?range=all", now: now},
		{name: "one repository", url: "/stats/?range=90d&repo=acme%2Fapi", now: now, since: now.Add(-90 * 24 * time.Hour), repo: "acme/api"},
		{name: "one event", url: "/stats/?range=30d&event=push", now: now, since: now.Add(-30 * 24 * time.Hour), event: "push"},
		{name: "empty window", url: "/stats/?range=7d", now: now.AddDate(0, 0, 30), since: now.AddDate(0, 0, 23), empty: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			got := cards(page)
			want := expectedCards(t, st, tc.since, tc.now, tc.repo, tc.event)
			if !tc.empty && want["runs"] == "0" {
				t.Fatalf("fixtures have no runs for %s; the case checks nothing", tc.url)
			}
			if tc.empty && (want["runs"] != "0" || want["run-p50"] != "–") {
				t.Fatalf("window is not empty: %v", want)
			}
			for id, w := range want {
				if id == "coverage-hint" {
					if got["coverage"][1] != w {
						t.Errorf("coverage hint = %q, want %q", got["coverage"][1], w)
					}
					continue
				}
				if got[id][0] != w {
					t.Errorf("card %s = %q, want %q", id, got[id][0], w)
				}
			}

			total := 0
			for _, m := range regexp.MustCompile(`<span class="legend-value">([\d,]+)</span>`).FindAllStringSubmatch(page, -1) {
				n, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
				total += n
			}
			if strconv.Itoa(total) != strings.ReplaceAll(want["runs"], ",", "") {
				t.Errorf("runs chart series add up to %d, want the runs card's %s", total, want["runs"])
			}
			if tc.empty != strings.Contains(page, "No runs in this range") {
				t.Errorf("empty chart label shown = %v, want %v", !tc.empty, tc.empty)
			}
		})
	}
}

func TestOverviewDatastarRequestPatchesPage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)

	rec := get(t, h, "/stats/?range=30d&repo=acme%2Fapi&event=&datastar=%7B%7D", "Datastar-Request", "true")
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`event: datastar-patch-elements` + "\n" + `data: elements <div id="page"`,
		`data: elements <nav id="nav"`,
		`href="/stats/?range=30d&amp;repo=acme%2Fapi"`,
		`data: elements <form id="filters" class="filter-bar" method="get" action="/stats/"`,
		`<option value="acme/api" selected>`,
		`value="30d" checked>`,
		`history.replaceState(null, '', "/stats/?range=30d\u0026repo=acme%2Fapi")`,
		"in the last 30 days in acme/api",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("patch is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<html") {
		t.Error("a patch should carry the page, nav and filters, not the shell")
	}

	page := get(t, h, "/stats/?range=30d&repo=acme%2Fapi").Body.String()
	if got := cards(page)["runs"][0]; !strings.Contains(body, `<div class="stat-value">`+got+`</div>`) {
		t.Errorf("patch and full page disagree on runs: page has %s", got)
	}
}

func TestDatastarScriptIsTheVendoredFile(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	h := dashboard(st, time.Now())

	page := get(t, h, "/stats/").Body.String()
	m := regexp.MustCompile(`<script type="module" src="(/static/datastar\.[0-9a-f]+\.js)"></script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no datastar script in the page:\n%s", page)
	}
	rec := get(t, h, m[1])
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("Content-Type = %q, want text/javascript", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
	sum := sha256.Sum256(rec.Body.Bytes())
	if got := hex.EncodeToString(sum[:]); got != datastarSHA256 {
		t.Errorf("sha256 = %s, want the vendored v1.0.4 %s", got, datastarSHA256)
	}

	gz := get(t, h, m[1], "Accept-Encoding", "gzip")
	if gz.Header().Get("Content-Encoding") != "gzip" || gz.Body.Len() >= rec.Body.Len() {
		t.Errorf("gzip request got Content-Encoding %q and %d bytes, want gzip under %d", gz.Header().Get("Content-Encoding"), gz.Body.Len(), rec.Body.Len())
	}
	if gz := get(t, h, "/stats/", "Accept-Encoding", "gzip"); gz.Header().Get("Content-Encoding") != "gzip" {
		t.Error("the page is not gzipped")
	}
}

func TestTemplatePagesRenderOverFixtures(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now)
	for _, url := range []string{
		"/steps", "/regressions", "/daily", "/sizing", "/spend",
	} {
		get(t, h, url)
	}
}
