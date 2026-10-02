package ui

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
)

type filterForm struct {
	Repo  string
	Days  int
	Repos []string
}

// workflowURL builds the GitHub URL for a workflow's runs from the path
// stored on its runs, stripping the ".github/workflows/" or "dynamic/"
// prefix GitHub puts on it. Empty path means no link.
func workflowURL(ghURL, repo, path string) string {
	if path == "" {
		return ""
	}
	file := strings.TrimPrefix(path, ".github/workflows/")
	file = strings.TrimPrefix(file, "dynamic/")
	return ghURL + "/" + repo + "/actions/workflows/" + file
}

func runURL(ghURL, repo string, runID int64, attempt int) string {
	return fmt.Sprintf("%s/%s/actions/runs/%d/attempts/%d", ghURL, repo, runID, attempt)
}

func commitURL(ghURL, repo, sha string) string {
	return fmt.Sprintf("%s/%s/commit/%s", ghURL, repo, sha)
}

func branchURL(ghURL, repo, branch string) string {
	return fmt.Sprintf("%s/%s/tree/%s", ghURL, repo, branch)
}

// stepURL is the step's log, the job's html_url plus its step anchor.
// Empty html_url means no link.
func stepURL(htmlURL string, number int) string {
	if htmlURL == "" {
		return ""
	}
	return fmt.Sprintf("%s#step:%d:1", htmlURL, number)
}

func (u *UI) filter(r *http.Request, defaultDays int) (store.Filter, filterForm, error) {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days <= 0 || days > 3650 {
		days = defaultDays
	}
	f := filterForm{Repo: r.URL.Query().Get("repo"), Days: days}
	f.Repos, err = u.Store.Repositories(r.Context())
	return store.Filter{Repository: f.Repo, Since: time.Now().AddDate(0, 0, -days)}, f, err
}

func floatParam(r *http.Request, key string, def float64) float64 {
	v, err := strconv.ParseFloat(r.URL.Query().Get(key), 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func (u *UI) jobs(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 7)
	if err != nil {
		u.fail(w, err)
		return
	}
	jobs, err := u.Store.RecentJobs(r.Context(), f, 100)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "jobs", map[string]any{"Filter": form, "Rows": jobs, "GitHubURL": u.GitHubURL})
}

func (u *UI) job(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := u.Store.Job(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	if d == nil {
		http.NotFound(w, r)
		return
	}
	series, err := u.Store.JobSeries(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "job", map[string]any{"Job": d, "Chart": chart(d, series), "GitHubURL": u.GitHubURL})
}

func (u *UI) steps(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 30)
	if err != nil {
		u.fail(w, err)
		return
	}
	rows, err := u.Store.SlowSteps(r.Context(), f)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "steps", map[string]any{"Filter": form, "Rows": rows})
}

func (u *UI) regressions(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 14)
	if err != nil {
		u.fail(w, err)
		return
	}
	ratio := floatParam(r, "ratio", 1.25)
	minSecs := floatParam(r, "min", 5)
	rows, err := u.Store.Regressions(r.Context(), f, ratio, minSecs)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "regressions", map[string]any{
		"Filter": form, "Rows": rows, "Ratio": ratio, "Min": minSecs, "BaselineDays": store.BaselineDays, "GitHubURL": u.GitHubURL,
	})
}

func (u *UI) sizing(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 30)
	if err != nil {
		u.fail(w, err)
		return
	}
	rows, err := u.Store.Sizing(r.Context(), f)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "sizing", map[string]any{"Filter": form, "Rows": rows, "GitHubURL": u.GitHubURL})
}

const maxDailyDays = 60

type dailyCell struct {
	Median float64
	Runs   int64
}

type dailyRow struct {
	Repository string
	Workflow   string
	Job        string
	Path       string
	JobID      int64
	Cells      []dailyCell
}

func (u *UI) daily(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 14)
	if err != nil {
		u.fail(w, err)
		return
	}
	form.Days = min(form.Days, maxDailyDays)
	first := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1-form.Days)
	f.Since = first
	durations, err := u.Store.DailyDurations(r.Context(), f)
	if err != nil {
		u.fail(w, err)
		return
	}
	days := make([]time.Time, form.Days)
	for i := range days {
		days[i] = first.AddDate(0, 0, i)
	}
	var workflows, jobs []dailyRow
	for _, d := range durations {
		rows := &jobs
		if d.Job == "" {
			rows = &workflows
		}
		n := len(*rows)
		if n == 0 || (*rows)[n-1].Repository != d.Repository || (*rows)[n-1].Workflow != d.Workflow || (*rows)[n-1].Job != d.Job {
			*rows = append(*rows, dailyRow{Repository: d.Repository, Workflow: d.Workflow, Job: d.Job, Path: d.Path, JobID: d.JobID, Cells: make([]dailyCell, len(days))})
			n++
		}
		if i := int(d.Day.Sub(first) / (24 * time.Hour)); i >= 0 && i < len(days) {
			(*rows)[n-1].Cells[i] = dailyCell{Median: d.Median, Runs: d.Runs}
		}
	}
	u.render(w, "daily", map[string]any{"Filter": form, "Days": days, "Workflows": workflows, "Jobs": jobs, "GitHubURL": u.GitHubURL})
}

type spendRow struct {
	store.SpendGroup
	spend.Price
}

func (u *UI) spend(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 90)
	if err != nil {
		u.fail(w, err)
		return
	}
	groups, err := u.Store.SpendGroups(r.Context(), f)
	if err != nil {
		u.fail(w, err)
		return
	}
	var (
		rows    []spendRow
		total   float64
		unknown int64
	)
	for _, g := range groups {
		p := u.Rates.Price(g.Labels, g.Private, g.Minutes)
		rows = append(rows, spendRow{g, p})
		total += p.Cost
		if !p.Known {
			unknown += g.Minutes
		}
	}
	u.render(w, "spend", map[string]any{"Filter": form, "Rows": rows, "Total": total, "UnknownMinutes": unknown})
}

const (
	chartW, chartH = 960.0, 220.0
	padL, padR     = 40.0, 10.0
	padT, padB     = 10.0, 20.0
	maxPoints      = 1500
)

// chart draws the runner's CPU utilization and memory used as a share of
// MemTotal across the job, with a line at the start of each step.
func chart(d *store.JobDetail, samples []store.Sample) template.HTML {
	if len(samples) == 0 {
		return ""
	}
	start, end := samples[0].Time, samples[len(samples)-1].Time
	if d.StartedAt != nil && d.StartedAt.Before(start) {
		start = *d.StartedAt
	}
	if d.CompletedAt != nil && d.CompletedAt.After(end) {
		end = *d.CompletedAt
	}
	span := end.Sub(start).Seconds()
	if span <= 0 {
		return ""
	}
	x := func(t time.Time) float64 { return padL + t.Sub(start).Seconds()/span*(chartW-padL-padR) }
	y := func(frac float64) float64 {
		frac = max(0, min(1, frac))
		return padT + (1-frac)*(chartH-padT-padB)
	}

	var cpu, mem []store.Sample
	for _, s := range samples {
		switch s.Metric {
		case store.MetricCPUUtilization:
			cpu = append(cpu, s)
		case store.MetricMemoryUsage:
			if d.MemTotal != nil && *d.MemTotal > 0 {
				mem = append(mem, store.Sample{Time: s.Time, Value: s.Value / *d.MemTotal})
			}
		}
	}
	line := func(pts []store.Sample, color string) string {
		stride := max(1, len(pts)/maxPoints)
		var b strings.Builder
		for i := 0; i < len(pts); i += stride {
			fmt.Fprintf(&b, "%.1f,%.1f ", x(pts[i].Time), y(pts[i].Value))
		}
		return fmt.Sprintf(`<polyline fill="none" stroke="%s" stroke-width="1.5" points="%s"/>`, color, b.String())
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" role="img">`, chartW, chartH)
	for _, frac := range []float64{0, 0.5, 1} {
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" stroke="#d0d7de"/><text x="2" y="%.1f" font-size="10" fill="#656d76">%.0f%%</text>`,
			padL, chartW-padR, y(frac), y(frac), y(frac)+3, frac*100)
	}
	for _, st := range d.Steps {
		if st.StartedAt == nil {
			continue
		}
		sx := x(*st.StartedAt)
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.0f" y2="%.0f" stroke="#afb8c1" stroke-dasharray="2,2"><title>%s</title></line>`,
			sx, sx, padT, chartH-padB, template.HTMLEscapeString(st.Name))
	}
	b.WriteString(line(cpu, "#0969da"))
	if len(mem) > 0 {
		b.WriteString(line(mem, "#8250df"))
	}
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" font-size="10" fill="#0969da">CPU utilization</text>`, padL, chartH-4)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" font-size="10" fill="#8250df">memory used / MemTotal</text>`, padL+100, chartH-4)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
