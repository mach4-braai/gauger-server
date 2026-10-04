package stats

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// wasteRows caps each table; the card header says how many there are.
const wasteRows = 50

// wasteSeries are the wasted minutes chart's series, bottom of the stack
// first.
var wasteSeries = []struct{ key, label, color string }{
	{"failure", "Failed", "var(--bad)"},
	{"cancelled", "Cancelled", "var(--ink-3)"},
}

type wasteView struct {
	Description string
	Tiles       []tile
	Step        chart.Step
	Minutes     chart.TimeProps
	Legend      []chart.LegendItem
	ReRuns      []wasteReRun
	ReRunTotal  int
	Flaky       []wasteFlaky
	FlakyTotal  int
}

// wasteJob is a link to one job's page.
type wasteJob struct {
	Href  string
	Label string
	Title string
	Tone  string
}

type wasteReRun struct {
	Repository string
	Workflow   string
	Job        string
	Attempts   string
	Runs       string
	Earlier    *wasteJob
	Latest     wasteJob
}

type wasteFlaky struct {
	Repository string
	Workflow   string
	Job        string
	Commit     string
	CommitURL  string
	Branch     string
	Failures   string
	Failed     wasteJob
	Passed     wasteJob
}

func (s *Server) waste(r *http.Request, f store.Filter) (templ.Component, error) {
	ctx := r.Context()
	minutes, err := s.Store.WastedMinutes(ctx, f)
	if err != nil {
		return nil, err
	}
	st := step(f)
	byBucket, err := s.Store.WastedByBucket(ctx, f, string(st))
	if err != nil {
		return nil, err
	}
	reRunCount, err := s.Store.ReRunCount(ctx, f)
	if err != nil {
		return nil, err
	}
	reRuns, err := s.Store.ReRuns(ctx, f)
	if err != nil {
		return nil, err
	}
	flaky, err := s.Store.FlakyCandidates(ctx, f)
	if err != nil {
		return nil, err
	}

	var total, unpriced int64
	var cost float64
	byConclusion := map[string]int64{}
	for _, m := range minutes {
		total += m.Minutes
		byConclusion[m.Conclusion] += m.Minutes
		p := s.Rates.Price(m.Labels, m.Private, m.Minutes)
		cost += p.Cost
		if !p.Known {
			unpriced += m.Minutes
		}
	}
	spendHint := "at GitHub's list prices"
	if unpriced > 0 {
		spendHint = chart.Integer(float64(unpriced)) + " min on unpriced labels"
	}
	var rerunJobs int64
	for _, rr := range reRuns {
		rerunJobs += rr.Attempts
	}

	var first time.Time
	if len(byBucket) > 0 {
		first = byBucket[0].Bucket
	}
	buckets := axis(f, st, first)
	chartProps := chart.TimeProps{
		Props: chart.Props{
			ID:            "waste",
			Hidden:        chart.Hidden(r.URL.Query(), "waste"),
			Empty:         "No wasted minutes in this range",
			FormatTooltip: func(v float64) string { return chart.Integer(v) + " min" },
		},
		Buckets: buckets,
		Step:    st,
	}
	for _, c := range wasteSeries {
		values := chart.Densify(buckets, byBucket,
			func(b store.WastedBucket) time.Time { return b.Bucket },
			func(b store.WastedBucket) float64 {
				if b.Conclusion != c.key {
					return 0
				}
				return float64(b.Minutes)
			})
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		if sum > 0 {
			chartProps.Series = append(chartProps.Series, chart.Series{Key: c.key, Label: c.label, Color: c.color, Values: values})
		}
	}
	legend := chart.Toggles(chartProps.Props, r.URL)
	for i, sr := range chartProps.Series {
		legend[i].Value = chart.Integer(float64(byConclusion[sr.Key])) + " min"
	}

	v := wasteView{
		Description: "Minutes that produced no result in " + describe(f) + ".",
		Tiles: []tile{
			{ID: "wasted-minutes", Label: "Wasted minutes", Value: chart.Integer(float64(total)),
				Hint:  fmt.Sprintf("%s failed · %s cancelled", chart.Integer(float64(byConclusion["failure"])), chart.Integer(float64(byConclusion["cancelled"]))),
				Title: "Minutes of jobs that were cancelled or failed after they started, each job rounded up to a minute."},
			{ID: "wasted-spend", Label: "Wasted spend", Value: chart.USD(cost), Hint: spendHint,
				Title: "Wasted minutes times the runner label's rate. Standard runners in public repositories and self-hosted runners are free."},
			{ID: "reruns", Label: "Re-runs", Value: chart.Integer(float64(reRunCount)), Hint: plural(rerunJobs, "job re-run"),
				Title: "Run attempts after the first that started in this range."},
			{ID: "flaky", Label: "Flaky candidates", Value: chart.Integer(float64(len(flaky))), Hint: "failed, then passed",
				Title: "Jobs that failed and then passed on the same commit, in a later attempt or a later run."},
		},
		Step:       st,
		Minutes:    chartProps,
		Legend:     legend,
		ReRunTotal: len(reRuns),
		FlakyTotal: len(flaky),
	}
	for _, rr := range reRuns[:min(len(reRuns), wasteRows)] {
		row := wasteReRun{
			Repository: rr.Repository, Workflow: rr.Workflow, Job: rr.Job,
			Attempts: chart.Integer(float64(rr.Attempts)), Runs: chart.Integer(float64(rr.Runs)),
			Latest: wasteJobLink(rr.Latest.ID, rr.Latest.Conclusion, 0, 0),
		}
		if rr.Earlier.ID != 0 {
			e := wasteJobLink(rr.Earlier.ID, rr.Earlier.Conclusion, 0, 0)
			row.Earlier = &e
		}
		v.ReRuns = append(v.ReRuns, row)
	}
	for _, c := range flaky[:min(len(flaky), wasteRows)] {
		v.Flaky = append(v.Flaky, wasteFlaky{
			Repository: c.Repository, Workflow: c.Workflow, Job: c.Job,
			Commit: c.SHA[:min(len(c.SHA), 7)], CommitURL: github.CommitURL(s.GitHubURL, c.Repository, c.SHA), Branch: c.Branch,
			Failures: chart.Integer(float64(c.Failures)),
			Failed:   wasteJobLink(c.Failed.ID, "failure", c.Failed.RunID, c.Failed.Attempt),
			Passed:   wasteJobLink(c.Passed.ID, "success", c.Passed.RunID, c.Passed.Attempt),
		})
	}
	return wastePage(v), nil
}

// wasteJobLink links a job's page. With a run, the label names the run
// and attempt; without one it shows the job's conclusion.
func wasteJobLink(id int64, conclusion string, run int64, attempt int) wasteJob {
	l := wasteJob{Href: "/stats/jobs/" + strconv.FormatInt(id, 10), Label: conclusion, Title: "Job " + strconv.FormatInt(id, 10) + ": " + conclusion}
	if run != 0 {
		l.Label = fmt.Sprintf("%d #%d", run, attempt)
		l.Title = fmt.Sprintf("%s, attempt %d of run %d", conclusion, attempt, run)
	}
	switch conclusion {
	case "success":
		l.Tone = "ok"
	case "failure", "timed_out":
		l.Tone = "bad"
	case "cancelled":
		l.Tone = "warn"
	}
	return l
}

func (v wasteView) bucketLabel() string {
	if v.Step == chart.Hour {
		return "Per hour, UTC"
	}
	return "Per day, UTC"
}

func wasteShown(shown, total int) string {
	if total > shown {
		return fmt.Sprintf("Showing %d of %s.", shown, chart.Integer(float64(total)))
	}
	return ""
}
