package ui_test

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// healthSection is the part of page from the heading titled from through the
// heading titled to.
func healthSection(t *testing.T, page, from, to string) string {
	t.Helper()
	start := strings.Index(page, from+"</h2>")
	end := strings.Index(page, to+"</h2>")
	if start < 0 || end < start {
		t.Fatalf("page has no %q section before %q", from, to)
	}
	return page[start:end]
}

func healthCount(t *testing.T, st *store.Store, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestHealthDeliveriesMatchSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now
	keep := now.Add(-store.DeliveryRetention)

	if all := healthCount(t, st, `SELECT count(*) FROM webhook_deliveries`); all <= healthCount(t, st, `SELECT count(*) FROM webhook_deliveries WHERE received_at >= $1`, keep) {
		t.Fatal("fixtures have no deliveries older than the retention; the cap checks nothing")
	}

	legend := regexp.MustCompile(`<span class="truncate">([a-z_]+)</span>\s*<span class="legend-value">([\d,]+)</span>`)
	for _, tc := range []struct {
		name, url string
		since     time.Time
		capped    bool
	}{
		{"7d", "/stats/health?range=7d", now.Add(-7 * 24 * time.Hour), false},
		{"30d", "/stats/health?range=30d", keep, false},
		{"90d is cut to 30 days", "/stats/health?range=90d", keep, true},
		{"all is cut to 30 days", "/stats/health?range=all", keep, true},
		{"one repository does not filter deliveries", "/stats/health?range=7d&repo=acme%2Fapi", now.Add(-7 * 24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, now), tc.url).Body.String()

			rows, err := st.Pool.Query(context.Background(),
				`SELECT event, count(*) FROM webhook_deliveries WHERE received_at >= $1 AND received_at < $2 GROUP BY 1`, tc.since, now)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{}
			var total int64
			for rows.Next() {
				var event string
				var n int64
				if err := rows.Scan(&event, &n); err != nil {
					t.Fatal(err)
				}
				want[event] = chart.Integer(float64(n))
				total += n
			}
			rows.Close()
			if len(want) < 4 || total == 0 {
				t.Fatalf("window has %d event types and %d deliveries; the case checks nothing", len(want), total)
			}

			got := map[string]string{}
			for _, m := range legend.FindAllStringSubmatch(healthSection(t, page, "Webhook deliveries", "Task queue"), -1) {
				got[m[1]] = m[2]
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("deliveries per event type = %v, want %v", got, want)
			}
			if tile := cards(page)["deliveries"][0]; tile != chart.Integer(float64(total)) {
				t.Errorf("deliveries card = %q, want %d", tile, total)
			}
			if capped := strings.Contains(page, `id="deliveries-cap"`); capped != tc.capped {
				t.Errorf("cap note shown = %v, want %v", capped, tc.capped)
			}
		})
	}
}

func TestHealthTaskQueueMatchesSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now), "/stats/health").Body.String()

	row := regexp.MustCompile(`(?s)<tr id="task-([a-z]+)">.*?class="task-queued">([\d,]+)</td>.*?class="task-due">([\d,]+)</td>.*?class="task-attempts">(\d+)</td>.*?class="task-error">([^<]*)</td>`)
	got := map[string][4]string{}
	for _, m := range row.FindAllStringSubmatch(page, -1) {
		got[m[1]] = [4]string{m[2], m[3], m[4], m[5]}
	}

	rows, err := st.Pool.Query(context.Background(), `
		SELECT kind, count(*), count(*) FILTER (WHERE next_at <= $1), max(attempts) FROM tasks GROUP BY kind`, fx.Now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][3]int64{}
	for rows.Next() {
		var kind string
		var w [3]int64
		if err := rows.Scan(&kind, &w[0], &w[1], &w[2]); err != nil {
			t.Fatal(err)
		}
		want[kind] = w
	}
	rows.Close()
	if len(want) < 3 {
		t.Fatalf("fixtures have %d task kinds", len(want))
	}
	if len(got) != len(want) {
		t.Errorf("page lists %d task kinds, want %d: %v", len(got), len(want), got)
	}
	for kind, w := range want {
		g := got[kind]
		for i, v := range [3]string{g[0], g[1], g[2]} {
			if v != strconv.FormatInt(w[i], 10) {
				t.Errorf("task %s column %d = %q, want %d", kind, i, v, w[i])
			}
		}
	}

	if got["run"][3] != storetest.TaskLastError {
		t.Errorf("run's last error = %q, want %q", got["run"][3], storetest.TaskLastError)
	}
	if got["backfill"][3] != "" || got["backfill"][0] != "6" {
		t.Errorf("backfill row = %v, want 6 queued and no error", got["backfill"])
	}

	var queued, due int64
	for _, w := range want {
		queued += w[0]
		due += w[1]
	}
	tiles := cards(page)
	if tiles["queued"][0] != chart.Integer(float64(queued)) || tiles["due"][0] != chart.Integer(float64(due)) {
		t.Errorf("queued and due cards = %q and %q, want %d and %d", tiles["queued"][0], tiles["due"][0], queued, due)
	}
}

func TestHealthCoverageMatchesSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)

	for _, tc := range []struct {
		name, url, repo string
		since           time.Time
	}{
		{"full range", "/stats/health?range=all", "", time.Time{}},
		{"seven days", "/stats/health?range=7d", "", now.Add(-7 * 24 * time.Hour)},
		{"one repository", "/stats/health?range=30d&repo=oss%2Fgauger", "oss/gauger", now.Add(-30 * 24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, now), tc.url).Body.String()

			const window = `FROM jobs j
				WHERE j.status = 'completed'
				  AND coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1
				  AND coalesce(j.started_at, j.created_at, j.runner_seen_at) < $2
				  AND ($3 = '' OR j.repository = $3)`
			args := []any{tc.since, now, tc.repo}
			jobs := healthCount(t, st, `SELECT count(*) `+window, args...)
			sampled := healthCount(t, st, `SELECT count(*) `+window+` AND EXISTS (SELECT 1 FROM samples m WHERE m.job_id = j.id)`, args...)
			artifact := healthCount(t, st, `SELECT count(*) `+window+` AND j.artifact_ingested_at IS NOT NULL AND EXISTS (SELECT 1 FROM samples m WHERE m.job_id = j.id)`, args...)
			if jobs == 0 || sampled == 0 {
				t.Fatalf("window has %d jobs and %d sampled; the case checks nothing", jobs, sampled)
			}

			c := cards(page)["coverage"]
			if want := chart.Percent(float64(sampled) / float64(jobs)); c[0] != want {
				t.Errorf("coverage = %q, want %q", c[0], want)
			}
			wantHint := fmt.Sprintf("%s of %s jobs · %s from artifact", chart.Integer(float64(sampled)), chart.Integer(float64(jobs)), chart.Integer(float64(artifact)))
			if c[1] != wantHint {
				t.Errorf("coverage hint = %q, want %q", c[1], wantHint)
			}

			series := map[string]string{}
			for _, m := range regexp.MustCompile(`<span class="truncate">([A-Za-z ]+)</span>\s*<span class="legend-value">([\d,]+)</span>`).FindAllStringSubmatch(page[strings.Index(page, "gauger coverage</h2>"):], -1) {
				series[m[1]] = m[2]
			}
			var sum int64
			for _, v := range series {
				n, _ := strconv.Atoi(strings.ReplaceAll(v, ",", ""))
				sum += int64(n)
			}
			if sum != jobs {
				t.Errorf("coverage series %v add up to %d, want %d jobs", series, sum, jobs)
			}
			if got, want := series["Fallback artifact"], chart.Integer(float64(artifact)); artifact > 0 && got != want {
				t.Errorf("fallback artifact jobs = %q, want %q", got, want)
			}
		})
	}
}

func TestHealthEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	page := get(t, dashboard(st, fx.Now.AddDate(0, 0, 60)), "/stats/health?range=7d").Body.String()

	for _, want := range []string{"No webhook deliveries in this range", "No completed jobs in this range"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	c := cards(page)
	if c["deliveries"][0] != "0" || c["coverage"][0] != "–" {
		t.Errorf("deliveries and coverage cards = %q and %q, want 0 and –", c["deliveries"][0], c["coverage"][0])
	}
	if !strings.Contains(page, `id="task-run"`) {
		t.Error("the task queue is a snapshot and should show whatever the window")
	}
}

func TestHealthEmptyDatabase(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	page := get(t, dashboard(st, time.Now()), "/stats/health?range=all").Body.String()
	for _, want := range []string{"The queue is empty", "No webhook deliveries in this range", "No completed jobs in this range"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestHealthDatastarRequestPatchesPage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	rec := get(t, dashboard(st, fx.Now), "/stats/health?range=30d&datastar=%7B%7D", "Datastar-Request", "true")
	body := rec.Body.String()
	for _, want := range []string{`data: elements <div id="page"`, "Task queue", `id="task-run"`, `href="/stats/health?range=30d"`} {
		if !strings.Contains(body, want) {
			t.Errorf("patch is missing %q", want)
		}
	}
}
