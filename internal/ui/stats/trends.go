package stats

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// maxDailyBuckets caps the trend chart's axis; the most recent buckets stay.
const maxDailyBuckets = 60

// trendBucket is one choice of the bucket picker. Its name is the unit
// DailyDurations takes.
type trendBucket struct {
	name, label string
	trunc       func(time.Time) time.Time
	next        func(time.Time) time.Time
	back        func(time.Time, int) time.Time
	tick        func(time.Time) string
	title       func(time.Time) string
}

func trendWeekStart(t time.Time) time.Time {
	t = t.Truncate(24 * time.Hour)
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t.AddDate(0, 0, 1-wd)
}

var trendBuckets = []trendBucket{
	{
		name: "day", label: "Day",
		trunc: func(t time.Time) time.Time { return t.Truncate(24 * time.Hour) },
		next:  func(t time.Time) time.Time { return t.AddDate(0, 0, 1) },
		back:  func(t time.Time, n int) time.Time { return t.AddDate(0, 0, -n) },
		tick:  func(t time.Time) string { return t.Format("01-02") },
		title: func(t time.Time) string { return t.Format("2006-01-02") },
	},
	{
		name: "week", label: "Week",
		trunc: trendWeekStart,
		next:  func(t time.Time) time.Time { return t.AddDate(0, 0, 7) },
		back:  func(t time.Time, n int) time.Time { return t.AddDate(0, 0, -7*n) },
		tick:  func(t time.Time) string { _, w := t.ISOWeek(); return fmt.Sprintf("W%02d", w) },
		title: func(t time.Time) string { return "Week of " + t.Format("2006-01-02") },
	},
	{
		name: "month", label: "Month",
		trunc: func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC) },
		next:  func(t time.Time) time.Time { return t.AddDate(0, 1, 0) },
		back:  func(t time.Time, n int) time.Time { return t.AddDate(0, -n, 0) },
		tick:  func(t time.Time) string { return t.Format("2006-01") },
		title: func(t time.Time) string { return t.Format("2006-01") },
	},
}

// trendBucketNamed finds a bucket by name, falling back to day when the
// name is empty or unknown.
func trendBucketNamed(name string) trendBucket {
	for _, b := range trendBuckets {
		if b.name == name {
			return b
		}
	}
	return trendBuckets[0]
}

// trendStarts lists each bucket's start from since's bucket through until's,
// trimmed to the most recent maxDailyBuckets. A zero since reaches back
// the full cap.
func trendStarts(b trendBucket, since, until time.Time) []time.Time {
	last := b.trunc(until)
	if since.IsZero() {
		since = b.back(last, maxDailyBuckets-1)
	}
	var starts []time.Time
	for t := b.trunc(since); !t.After(last); t = b.next(t) {
		starts = append(starts, t)
	}
	if len(starts) > maxDailyBuckets {
		starts = starts[len(starts)-maxDailyBuckets:]
	}
	return starts
}

func trendIndex(starts []time.Time) map[int64]int {
	index := make(map[int64]int, len(starts))
	for i, t := range starts {
		index[t.Unix()] = i
	}
	return index
}

// RangeForDays is the shell range that covers days, for redirects from the
// old pages that took a number of days.
func RangeForDays(days int) string {
	for _, w := range windows {
		if w.span > 0 && time.Duration(days)*24*time.Hour <= w.span {
			return w.key
		}
	}
	return "all"
}

// trendCell is one bucket's median duration and the runs behind it.
type trendCell struct {
	Median float64
	Runs   int64
}

// trendRow is one workflow, or one job of a workflow, across the buckets.
type trendRow struct {
	Repository string
	Workflow   string
	Job        string
	Path       string
	JobID      int64
	Cells      []trendCell
}

func (r trendRow) key() string {
	return r.Repository + "|" + r.Path + "|" + r.Workflow + "|" + r.Job
}

// trendRegression is a row of the regressions table.
type trendRegression struct {
	store.Regression
	Change       string
	WorkflowHref string
	StepHref     string
	BranchHref   string
	SelectHref   string
	Selected     bool
}

// trendStep is the chart of the regression the query selects.
type trendStep struct {
	Title  string
	Chart  chart.Props
	Legend []chart.LegendItem
}

type trendsView struct {
	Description string
	Bucket      string
	Workflow    string
	Job         string
	Workflows   []string
	Jobs        []string
	Ticks       []string
	Per         string
	Chart       chart.Props
	Legend      []chart.LegendItem
	WorkflowRow []trendRow
	JobRow      []trendRow
	GitHubURL   string
	Ratio       float64
	Min         float64
	Baseline    int
	Regressions []trendRegression
	Step        *trendStep
	StepKey     string
}

func trendFloat(q url.Values, key string, def float64) float64 {
	v, err := strconv.ParseFloat(q.Get(key), 64)
	if err != nil || v <= 0 || math.IsInf(v, 0) {
		return def
	}
	return v
}

// trendSeconds labels an axis in seconds, keeping fractions below a minute.
func trendSeconds(v float64) string {
	if v < 60 {
		return strconv.FormatFloat(v, 'f', -1, 64) + "s"
	}
	return chart.Seconds(v)
}

func (s *Server) trends(r *http.Request, f store.Filter) (templ.Component, error) {
	v, err := s.trendsView(r, f)
	if err != nil {
		return nil, err
	}
	return trendsPage(v), nil
}

func (s *Server) trendsView(r *http.Request, f store.Filter) (trendsView, error) {
	ctx, q := r.Context(), r.URL.Query()
	b := trendBucketNamed(q.Get("bucket"))
	starts := trendStarts(b, f.Since, f.Until)
	v := trendsView{Bucket: b.name, GitHubURL: s.GitHubURL, Baseline: store.BaselineDays}

	scope := describe(store.Filter{Repository: f.Repository, Since: f.Since, Until: f.Until})
	v.Description = "Median durations of successful runs and jobs, and the steps that got slower, in " + scope + "."
	if f.Event != "" {
		v.Description += " The event filter does not apply to this page."
	}

	var err error
	if v.Workflows, err = s.Store.Workflows(ctx, f.Repository); err != nil {
		return v, err
	}
	if w := q.Get("workflow"); slices.Contains(v.Workflows, w) {
		v.Workflow = w
	}
	if v.Workflow != "" {
		if v.Jobs, err = s.Store.Jobs(ctx, f.Repository, v.Workflow); err != nil {
			return v, err
		}
	}
	if j := q.Get("job"); slices.Contains(v.Jobs, j) {
		v.Job = j
	}

	durations, err := s.Store.DailyDurations(ctx, store.Filter{Repository: f.Repository, Since: starts[0], Until: f.Until}, b.name, v.Workflow, v.Job)
	if err != nil {
		return v, err
	}
	index := trendIndex(starts)
	for _, d := range durations {
		rows := &v.JobRow
		if d.Job == "" {
			rows = &v.WorkflowRow
		}
		n := len(*rows)
		if n == 0 || (*rows)[n-1].Repository != d.Repository || (*rows)[n-1].Path != d.Path || (*rows)[n-1].Workflow != d.Workflow || (*rows)[n-1].Job != d.Job {
			*rows = append(*rows, trendRow{Repository: d.Repository, Workflow: d.Workflow, Job: d.Job, Path: d.Path, JobID: d.JobID, Cells: make([]trendCell, len(starts))})
			n++
		}
		if i, ok := index[d.Bucket.UTC().Unix()]; ok {
			(*rows)[n-1].Cells[i] = trendCell{Median: d.Median, Runs: d.Runs}
		}
	}

	v.Ticks = make([]string, len(starts))
	titles := make([]string, len(starts))
	for i, t := range starts {
		v.Ticks[i], titles[i] = b.tick(t), b.title(t)
	}
	rows, label, runs := v.WorkflowRow, func(r trendRow) string { return r.Workflow }, "Runs"
	v.Per = "workflow"
	if v.Workflow != "" {
		rows, label, runs = v.JobRow, func(r trendRow) string { return r.Job }, "Job runs"
		v.Per = "job"
	}
	allRepos := f.Repository == ""
	v.Chart = chart.Props{
		ID: "trend", Ticks: v.Ticks, Titles: titles, Unstacked: true, Height: 280,
		Format: trendSeconds, FormatRight: chart.Compact, FormatTooltip: chart.Seconds,
		Hidden: chart.Hidden(q, "trend"),
		Empty:  "No successful runs in this window",
		Series: trendSeries(rows, len(starts), runs, func(r trendRow) string {
			if allRepos {
				return r.Repository + " · " + label(r)
			}
			return label(r)
		}),
	}
	v.Legend = chart.Toggles(v.Chart, r.URL)

	ratio, minSecs := trendFloat(q, "ratio", 1.25), trendFloat(q, "min", 5)
	v.Ratio, v.Min = ratio, minSecs
	found, err := s.Store.Regressions(ctx, f, ratio, minSecs)
	if err != nil {
		return v, err
	}
	var selected *store.Regression
	for i, g := range found {
		key := trendStepKey(g)
		row := trendRegression{
			Regression:   g,
			Change:       fmt.Sprintf("%.2f×", g.Median/g.Baseline),
			WorkflowHref: github.WorkflowURL(s.GitHubURL, g.Repository, g.Path),
			StepHref:     github.StepURL(g.JobHTMLURL, g.StepNumber),
			SelectHref:   trendLink(r.URL, "step", key),
			Selected:     key == q.Get("step"),
		}
		if g.Branch != "" {
			row.BranchHref = github.BranchURL(s.GitHubURL, g.Repository, g.Branch)
		}
		if row.Selected {
			selected = &found[i]
			v.StepKey = key
			row.SelectHref = trendLink(r.URL, "step", "")
		}
		v.Regressions = append(v.Regressions, row)
	}
	if selected != nil {
		if v.Step, err = s.trendStep(r, f, *selected); err != nil {
			return v, err
		}
	}
	return v, nil
}

// trendSeries makes one grouped-bar series per row, with a dashed line of
// the run count on the right axis. Buckets without runs are gaps.
func trendSeries(rows []trendRow, slots int, runsLabel string, label func(trendRow) string) []chart.Series {
	var series []chart.Series
	totals := make([]float64, slots)
	for j, row := range rows {
		values := make([]float64, slots)
		for i, c := range row.Cells {
			values[i] = math.NaN()
			if c.Runs > 0 {
				values[i] = c.Median
				totals[i] += float64(c.Runs)
			}
		}
		series = append(series, chart.Series{Key: row.key(), Label: label(row), Color: chart.SeriesColors[j%len(chart.SeriesColors)], Values: values})
	}
	if len(series) == 0 {
		return nil
	}
	for i, n := range totals {
		if n == 0 {
			totals[i] = math.NaN()
		}
	}
	return append(series, chart.Series{Key: "runs", Label: runsLabel, Color: "var(--ink-3)", Values: totals, Kind: chart.Line, Right: true, Dashed: true})
}

// trendStepKey names a regression row in the query, so a link can select it.
func trendStepKey(g store.Regression) string {
	key, _ := json.Marshal([]string{g.Repository, g.Workflow, g.Job, g.Step, g.Branch, g.Day.UTC().Format("2006-01-02")})
	return string(key)
}

// trendLink is u with key set to value, or removed when value is empty.
func trendLink(u *url.URL, key, value string) string {
	q := u.Query()
	if value == "" {
		q.Del(key)
	} else {
		q.Set(key, value)
	}
	return u.Path + "?" + q.Encode()
}

// trendStep charts the daily median of the selected regression's step with
// its baseline as a reference line.
func (s *Server) trendStep(r *http.Request, f store.Filter, g store.Regression) (*trendStep, error) {
	day := trendBuckets[0]
	starts := trendStarts(day, f.Since, f.Until)
	days, err := s.Store.StepDaily(r.Context(), starts[0], g.Repository, g.Workflow, g.Job, g.Step, g.Branch)
	if err != nil {
		return nil, err
	}
	index := trendIndex(starts)
	median, runs := make([]float64, len(starts)), make([]float64, len(starts))
	for i := range starts {
		median[i], runs[i] = math.NaN(), math.NaN()
	}
	ticks, titles := make([]string, len(starts)), make([]string, len(starts))
	for i, t := range starts {
		ticks[i], titles[i] = day.tick(t), day.title(t)
	}
	for _, d := range days {
		if i, ok := index[d.Day.UTC().Unix()]; ok {
			median[i], runs[i] = d.Median, float64(d.Runs)
		}
	}
	p := chart.Props{
		ID: "step", Ticks: ticks, Titles: titles, Unstacked: true, Height: 240,
		Format: trendSeconds, FormatRight: chart.Compact, FormatTooltip: chart.Seconds,
		Hidden: chart.Hidden(r.URL.Query(), "step"),
		Empty:  "No runs of this step in this window",
		Series: []chart.Series{
			{Key: "median", Label: "Median", Color: chart.SeriesColors[0], Values: median},
			{Key: "runs", Label: "Runs", Color: "var(--ink-3)", Values: runs, Kind: chart.Line, Right: true, Dashed: true},
		},
		References: []chart.Reference{{Value: g.Baseline, Label: fmt.Sprintf("%d-day baseline %s", store.BaselineDays, chart.Seconds(g.Baseline))}},
	}
	title := g.Step + " in " + g.Job
	if g.Branch != "" {
		title += " on " + g.Branch
	}
	return &trendStep{Title: title, Chart: p, Legend: chart.Toggles(p, r.URL)}, nil
}
