package ui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/store/storetest"
	"github.com/mach4-braai/gauger-server/internal/ui"
)

func TestDailyFiltersByWorkflowAndJob(t *testing.T) {
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

	h := (&ui.UI{Store: st, Creds: st, GitHubURL: "https://github.com"}).Handler()
	get := func(url string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", url, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	legendItems := func(page string) [][]string {
		return regexp.MustCompile(`class="legend-item"><span class="swatch"[^>]*></span>([^<]+)</span>`).FindAllStringSubmatch(page, -1)
	}
	selected := func(page, selectName string) [][]string {
		block := regexp.MustCompile(`name="` + selectName + `">([\s\S]*?)</select>`).FindStringSubmatch(page)[1]
		return regexp.MustCompile(`<option selected>([^<]*)</option>`).FindAllStringSubmatch(block, -1)
	}

	page := get("/daily?repo=acme/app&days=1&workflow=CI")
	if got := selected(page, "workflow"); len(got) != 1 || got[0][1] != "CI" {
		t.Fatalf("workflow select = %v, want [CI] selected", got)
	}
	items := legendItems(page)
	if len(items) != 2 || items[0][1] != "a" || items[1][1] != "b" {
		t.Fatalf("legend = %v, want bars for jobs a and b, not workflows", items)
	}
	if !regexp.MustCompile(`(?s)<h2>Jobs</h2>.*?<td>CI</td><td>a</td>.*?<td>CI</td><td>b</td>`).MatchString(page) {
		t.Errorf("Jobs table missing narrowed a/b rows:\n%s", page)
	}
	if regexp.MustCompile(`(?s)<h2>Jobs</h2>.*?release`).MatchString(page) {
		t.Errorf("Jobs table still lists Deploy's job after filtering to CI:\n%s", page)
	}

	page = get("/daily?repo=acme/app&days=1&workflow=CI&job=a")
	items = legendItems(page)
	if len(items) != 1 || items[0][1] != "a" {
		t.Fatalf("legend = %v, want exactly one bar for job a", items)
	}
	if !regexp.MustCompile(`height="([\d.]+)"[^>]*><title>a · [\d-]+ · 1m40s · 1 run</title>`).MatchString(page) {
		t.Errorf("chart bar for job a missing its 1m40s/1-run title:\n%s", page)
	}

	page = get("/daily?repo=other/repo&days=1&workflow=CI&job=a")
	if got := selected(page, "workflow"); len(got) != 0 {
		t.Fatalf("workflow select = %v, want none selected (CI does not exist on other/repo)", got)
	}
	if got := selected(page, "job"); len(got) != 0 {
		t.Fatalf("job select = %v, want none selected", got)
	}
	items = legendItems(page)
	if len(items) != 1 || items[0][1] != "Lint" {
		t.Fatalf("legend = %v, want the unfiltered Lint workflow bar", items)
	}
}
