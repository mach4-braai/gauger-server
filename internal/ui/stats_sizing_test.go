package ui_test

import (
	"context"
	"fmt"
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

// sizingRows reads each row of the table with the given id as its
// attributes and its cells by data-col.
func sizingRows(page, table string) []map[string]string {
	t := regexp.MustCompile(`(?s)<table[^>]*id="` + table + `".*?</table>`).FindString(page)
	var out []map[string]string
	for _, m := range regexp.MustCompile(`(?s)<tr ([^>]*)>(.*?)</tr>`).FindAllStringSubmatch(t, -1) {
		row := map[string]string{}
		for _, a := range regexp.MustCompile(`([a-z-]+)="([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
			row[a[1]] = a[2]
		}
		for _, c := range regexp.MustCompile(`(?s)data-col="([a-z0-9-]+)"[^>]*>(.*?)</td>`).FindAllStringSubmatch(m[2], -1) {
			row["col:"+c[1]] = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(c[2], "")
		}
		if v := regexp.MustCompile(`title="([^"]*)"`).FindStringSubmatch(m[2]); v != nil {
			row["title"] = v[1]
		}
		out = append(out, row)
	}
	return out
}

func sizingJobRow(rows []map[string]string, id int64) map[string]string {
	for _, r := range rows {
		if r["id"] == "sizing-job-"+strconv.FormatInt(id, 10) {
			return r
		}
	}
	return nil
}

// sqlSizing is the sampled jobs in the window with their peaks, read with
// SQL that shares nothing with the store query.
type sqlSizing struct {
	id                 int64
	labels             []string
	private            *bool
	minutes            int64
	cpu, mem, memTotal *float64
}

func sampledJobs(t *testing.T, st *store.Store, since, until time.Time, repo string) (jobs []sqlSizing, total int64) {
	t.Helper()
	ctx := context.Background()
	window := `coalesce(j.started_at, j.created_at, j.runner_seen_at) >= $1 AND coalesce(j.started_at, j.created_at, j.runner_seen_at) < $2
		AND ($3 = '' OR j.repository = $3)`
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM jobs j WHERE `+window, since, until, repo).Scan(&total); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Pool.Query(ctx, `
		SELECT j.id, j.labels, p.private,
			CASE WHEN j.status = 'completed' AND j.completed_at > j.started_at THEN ceil(extract(epoch FROM j.completed_at - j.started_at) / 60)::bigint ELSE 0 END,
			(SELECT max(value) FROM samples WHERE job_id = j.id AND metric = 'system.cpu.utilization' AND series = ''),
			(SELECT max(value) FROM samples WHERE job_id = j.id AND metric = 'system.memory.usage' AND series = 'system.memory.state=used'),
			(SELECT max(value) FROM samples WHERE job_id = j.id AND metric = 'system.memory.limit')
		FROM jobs j LEFT JOIN repositories p ON p.full_name = j.repository
		WHERE EXISTS (SELECT 1 FROM samples m WHERE m.job_id = j.id) AND `+window, since, until, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var j sqlSizing
		if err := rows.Scan(&j.id, &j.labels, &j.private, &j.minutes, &j.cpu, &j.mem, &j.memTotal); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return jobs, total
}

func (j sqlSizing) memShare() *float64 {
	if j.mem == nil || j.memTotal == nil || *j.memTotal == 0 {
		return nil
	}
	return new(*j.mem / *j.memTotal)
}

func (j sqlSizing) candidate() bool {
	m := j.memShare()
	return m != nil && j.cpu != nil && *m <= 0.5 && *j.cpu <= 0.5
}

func TestSizingPageMatchesSQL(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	rates := spend.Rates(spend.DefaultRates)

	for _, tc := range []struct {
		name, url string
		now       time.Time
		days      int
		repo      string
	}{
		{name: "full range", url: "/stats/sizing?range=90d", now: now, days: 90},
		{name: "one repository", url: "/stats/sizing?range=30d&repo=acme%2Fapi", now: now, days: 30, repo: "acme/api"},
		{name: "one week", url: "/stats/sizing?range=7d", now: now, days: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := get(t, dashboard(st, tc.now), tc.url).Body.String()
			jobs, total := sampledJobs(t, st, tc.now.AddDate(0, 0, -tc.days), tc.now, tc.repo)
			if len(jobs) == 0 {
				t.Fatalf("fixtures have no sampled jobs for %s; the case checks nothing", tc.url)
			}

			var candidates int64
			var saving float64
			perLabel := map[string][]int64{}
			for _, j := range jobs {
				label := rates.Label(j.labels)
				switch {
				case slices.Contains(j.labels, "self-hosted"):
					label = "self-hosted"
				case label == "" && len(j.labels) > 0:
					label = j.labels[0]
				}
				c := perLabel[label]
				if c == nil {
					c = make([]int64, 4)
				}
				c[0]++
				if j.cpu != nil && *j.cpu > 0.5 {
					c[1]++
				}
				if m := j.memShare(); m != nil && *m > 0.5 {
					c[2]++
				}
				if j.cpu != nil && *j.cpu > 0.5 || j.memShare() != nil && *j.memShare() > 0.5 {
					c[3]++
				}
				perLabel[label] = c
				if j.candidate() {
					candidates++
					saving += rates.Downsize(j.labels, j.private, j.minutes).Saving
				}
			}

			got := cards(page)
			if want := chart.Percent(float64(len(jobs)) / float64(total)); got["coverage"][0] != want {
				t.Errorf("coverage = %q, want %s", got["coverage"][0], want)
			}
			if want := fmt.Sprintf("%s of %s jobs", chart.Integer(float64(len(jobs))), chart.Integer(float64(total))); got["coverage"][1] != want {
				t.Errorf("coverage hint = %q, want %q", got["coverage"][1], want)
			}
			if got["candidates"][0] != strconv.FormatInt(candidates, 10) {
				t.Errorf("candidates = %q, want %d", got["candidates"][0], candidates)
			}
			if got["saving"][0] != chart.USD(saving) {
				t.Errorf("saving = %q, want %s", got["saving"][0], chart.USD(saving))
			}

			labelRows := sizingRows(page, "sizing-labels")
			if len(labelRows) != len(perLabel) {
				t.Errorf("label rows = %d, want %d", len(labelRows), len(perLabel))
			}
			for _, r := range labelRows {
				want := perLabel[r["data-label"]]
				if want == nil {
					t.Errorf("unexpected label row %q", r["data-label"])
					continue
				}
				for i, col := range []string{"jobs", "cpu", "memory", "either"} {
					if r["col:"+col] != strconv.FormatInt(want[i], 10) {
						t.Errorf("%s %s = %s, want %d", r["data-label"], col, r["col:"+col], want[i])
					}
				}
			}

			jobRows := sizingRows(page, "sizing-jobs")
			shown := 0
			for _, j := range jobs {
				row := sizingJobRow(jobRows, j.id)
				if row == nil {
					continue
				}
				shown++
				if row["data-candidate"] != strconv.FormatBool(j.candidate()) {
					t.Errorf("job %d candidate = %s, want %v", j.id, row["data-candidate"], j.candidate())
				}
				if want := fmt.Sprintf("%.0f%%", *j.cpu*100); row["col:cpu-peak"] != want {
					t.Errorf("job %d peak CPU = %s, want %s", j.id, row["col:cpu-peak"], want)
				}
			}
			if shown != min(len(jobs), 200) {
				t.Errorf("job rows = %d, want %d", shown, min(len(jobs), 200))
			}
			if first := jobRows[0]["data-candidate"]; candidates > 0 && first != "true" {
				t.Errorf("first job row is not a candidate, candidates sort first")
			}
		})
	}
}

func TestSizingCandidatesAreOnLightJobs(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))
	page := get(t, h, "/stats/sizing?range=7d").Body.String()
	rows := sizingRows(page, "sizing-jobs")

	minutes := func(id int64) int64 {
		var m int64
		err := st.Pool.QueryRow(context.Background(),
			`SELECT ceil(extract(epoch FROM completed_at - started_at) / 60)::bigint FROM jobs WHERE id = $1`, id).Scan(&m)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	priv := sizingJobRow(rows, fx.LightPrivateJob)
	if priv == nil || priv["data-candidate"] != "true" {
		t.Fatalf("private ubuntu-latest job %d should be a candidate: %v", fx.LightPrivateJob, priv)
	}
	if want := chart.USD(float64(minutes(fx.LightPrivateJob)) * (0.006 - 0.002)); priv["col:smaller"] != "ubuntu-slim" || priv["col:saving"] != want {
		t.Errorf("private job: smaller %q saving %q, want ubuntu-slim saving %s", priv["col:smaller"], priv["col:saving"], want)
	}

	pub := sizingJobRow(rows, fx.LightPublicJob)
	if pub == nil || pub["data-candidate"] != "true" {
		t.Fatalf("public ubuntu-latest job %d should be a candidate: %v", fx.LightPublicJob, pub)
	}
	if pub["col:smaller"] != "ubuntu-slim" || pub["col:saving"] != "–" || pub["title"] != "These minutes are free." {
		t.Errorf("public job: smaller %q saving %q title %q, want ubuntu-slim, no price, free minutes", pub["col:smaller"], pub["col:saving"], pub["title"])
	}

	mac := sizingJobRow(rows, fx.LightMacJob)
	if mac == nil || mac["data-candidate"] != "true" {
		t.Fatalf("macOS job %d should be a candidate: %v", fx.LightMacJob, mac)
	}
	if mac["col:smaller"] != "–" || mac["col:saving"] != "–" || mac["title"] != "No smaller runner for this label." {
		t.Errorf("macOS job: smaller %q saving %q title %q, want no smaller label and no price", mac["col:smaller"], mac["col:saving"], mac["title"])
	}

	for _, id := range []int64{fx.SampledJob, fx.ArtifactJob} {
		if r := sizingJobRow(rows, id); r == nil || r["data-candidate"] != "false" || r["col:saving"] != "–" {
			t.Errorf("busy job %d should be listed as not a candidate with no saving: %v", id, r)
		}
	}

	only := get(t, h, "/stats/sizing?range=7d&repo=oss%2Fgauger").Body.String()
	if sizingJobRow(sizingRows(only, "sizing-jobs"), fx.LightPrivateJob) != nil {
		t.Error("a job of acme/api shows under repo=oss/gauger")
	}
	if sizingJobRow(sizingRows(only, "sizing-jobs"), fx.LightPublicJob) == nil {
		t.Error("the oss/gauger job is missing under repo=oss/gauger")
	}
}

func TestSizingStepsEqualStoreSizing(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	now := fx.Now.Add(time.Minute)
	page := get(t, dashboard(st, now), "/stats/sizing?range=30d").Body.String()

	want, err := st.Sizing(context.Background(), store.Filter{Since: now.AddDate(0, 0, -30), Until: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("fixtures have no sized steps; the test checks nothing")
	}
	rows := sizingRows(page, "sizing-steps")
	if len(rows) != len(want) {
		t.Fatalf("step rows = %d, want %d", len(rows), len(want))
	}
	opt := func(v *float64, f func(float64) string) string {
		if v == nil {
			return "–"
		}
		return f(*v)
	}
	for i, s := range want {
		r := rows[i]
		if r["data-job"] != strconv.FormatInt(s.JobID, 10) || r["data-step"] != strconv.Itoa(s.StepNumber) {
			t.Errorf("row %d is job %s step %s, want job %d step %d", i, r["data-job"], r["data-step"], s.JobID, s.StepNumber)
		}
		checks := map[string]string{
			"runs":      chart.Integer(float64(s.Runs)),
			"memory":    opt(s.PeakMemory, func(v float64) string { return fmt.Sprintf("%.2f GiB", v/(1<<30)) }),
			"memory-of": fmt.Sprintf("%.0f%%", *s.PeakMemory / *s.MemTotal * 100),
			"cpu":       fmt.Sprintf("%.1f of %.0f", *s.PeakCPU**s.CPUCount, *s.CPUCount),
			"saturated": opt(s.Saturated, func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }),
		}
		for col, w := range checks {
			if r["col:"+col] != w {
				t.Errorf("step %q %s = %q, want %q", s.Step, col, r["col:"+col], w)
			}
		}
	}
	if !strings.Contains(page, "Runner-level values during each step") {
		t.Error("the step table does not say its values are runner-level")
	}
}

func TestSizingEmptyWindow(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.AddDate(0, 0, 30))
	page := get(t, h, "/stats/sizing?range=7d").Body.String()

	got := cards(page)
	for id, want := range map[string]string{"coverage": "–", "candidates": "0", "saving": "$0"} {
		if got[id][0] != want {
			t.Errorf("card %s = %q, want %q", id, got[id][0], want)
		}
	}
	if got["coverage"][1] != "0 of 0 jobs" {
		t.Errorf("coverage hint = %q, want 0 of 0 jobs", got["coverage"][1])
	}
	if n := strings.Count(page, "No sampled jobs in this range"); n != 2 {
		t.Errorf("empty states for labels and jobs = %d, want 2", n)
	}
	if !strings.Contains(page, "No steps with gauger samples in this range") {
		t.Error("the step table has no empty state")
	}
}

func TestSizingDatastarRequestPatchesPage(t *testing.T) {
	st := storetest.Open(t, 90*24*time.Hour)
	fx := storetest.Seed(t, st)
	h := dashboard(st, fx.Now.Add(time.Minute))

	rec := get(t, h, "/stats/sizing?range=7d&repo=oss%2Fgauger&datastar=%7B%7D", "Datastar-Request", "true")
	body := rec.Body.String()
	for _, want := range []string{
		`data: elements <div id="page"`,
		`id="sizing-jobs"`,
		`href="/stats/sizing?repo=oss%2Fgauger"`,
		`history.replaceState(null, '', "/stats/sizing?range=7d\u0026repo=oss%2Fgauger")`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("patch is missing %q:\n%s", want, body)
		}
	}
}
