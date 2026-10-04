package stats

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

const (
	failuresRowLimit = 15
	failuresBadHue   = "var(--bad)"
	failuresCancHue  = "var(--ink-3)"
	failuresRateHue  = "var(--warn)"
)

type failuresJobRow struct {
	Name       string
	Repository string
	Workflow   string
	Failures   string
	Runs       string
	Rate       string
	// Href is the dashboard page of the job's latest failure.
	Href string
}

type failuresStepRow struct {
	Name     string
	Failures string
	Runs     string
	Rate     string
	// JobHref is the dashboard page of the job of the step's latest
	// failure; GitHubHref is that step's log, empty if unknown.
	JobHref    string
	GitHubHref string
	StepNumber string
}

type failuresRecentRow struct {
	JobID      string
	Name       string
	Repository string
	Workflow   string
	Branch     string
	BranchHref string
	When       string
	WhenFull   string
	Href       string
	// Cause is the job's first failed step; empty if no step failed.
	Cause      string
	StepNumber string
	LogHref    string
}

type failuresView struct {
	Description string
	Tiles       []tile
	Step        chart.Step
	Chart       chart.TimeProps
	Legend      []chart.LegendItem
	Jobs        []failuresJobRow
	JobsNote    string
	Steps       []failuresStepRow
	StepsNote   string
	Recent      []failuresRecentRow
}

func (s *Server) failures(r *http.Request, f store.Filter) (templ.Component, error) {
	st := step(f)
	fs, err := s.Store.Failures(r.Context(), f, string(st))
	if err != nil {
		return nil, err
	}

	var t struct{ runsFailed, runsCancelled, runsDecided, jobsFailed, jobsCancelled, jobsDecided int64 }
	for _, b := range fs.Buckets {
		t.runsFailed += b.RunsFailed
		t.runsCancelled += b.RunsCancelled
		t.runsDecided += b.RunsDecided
		t.jobsFailed += b.JobsFailed
		t.jobsCancelled += b.JobsCancelled
		t.jobsDecided += b.JobsDecided
	}

	var first time.Time
	if len(fs.Buckets) > 0 {
		first = fs.Buckets[0].Bucket
	}
	buckets := axis(f, st, first)
	at := func(b store.FailureBucket) time.Time { return b.Bucket }
	failed := chart.Densify(buckets, fs.Buckets, at, func(b store.FailureBucket) float64 { return float64(b.RunsFailed) })
	cancelled := chart.Densify(buckets, fs.Buckets, at, func(b store.FailureBucket) float64 { return float64(b.RunsCancelled) })
	decided := chart.Densify(buckets, fs.Buckets, at, func(b store.FailureBucket) float64 { return float64(b.RunsDecided) })
	rates := make([]float64, len(buckets))
	for i := range rates {
		rates[i] = math.NaN()
		if decided[i] > 0 {
			rates[i] = failed[i] / decided[i]
		}
	}

	runs := chart.TimeProps{
		Props: chart.Props{
			ID:            "failures",
			Hidden:        chart.Hidden(r.URL.Query(), "failures"),
			Format:        chart.Compact,
			FormatTooltip: chart.Integer,
			FormatRight:   failuresPercent,
			Empty:         "No failed or cancelled runs in this range",
		},
		Buckets: buckets,
		Step:    st,
	}
	var values []string
	for _, c := range []struct {
		key, label, color string
		values            []float64
		right             bool
		total             string
	}{
		{"failed", "Failed", failuresBadHue, failed, false, chart.Integer(float64(t.runsFailed))},
		{"cancelled", "Cancelled", failuresCancHue, cancelled, false, chart.Integer(float64(t.runsCancelled))},
		{"rate", "Failure rate", failuresRateHue, rates, true, ratio(t.runsFailed, t.runsDecided)},
	} {
		sum := 0.0
		for _, v := range c.values {
			if !math.IsNaN(v) {
				sum += v
			}
		}
		if sum == 0 {
			continue
		}
		sr := chart.Series{Key: c.key, Label: c.label, Color: c.color, Values: c.values, Right: c.right}
		if c.right {
			sr.Kind = chart.Line
		}
		runs.Series = append(runs.Series, sr)
		values = append(values, c.total)
	}
	legend := chart.Toggles(runs.Props, r.URL)
	for i, v := range values {
		legend[i].Value = v
	}

	v := failuresView{
		Description: "What fails in " + describe(f) + ", where, and which step is the first to fail.",
		Step:        st,
		Chart:       runs,
		Legend:      legend,
		Tiles: []tile{
			{ID: "failed-runs", Label: "Failed runs", Value: chart.Integer(float64(t.runsFailed)), Spark: failed, SparkColor: failuresBadHue},
			{ID: "cancelled-runs", Label: "Cancelled runs", Value: chart.Integer(float64(t.runsCancelled)),
				Title: "Cancelled runs are not failures and are left out of the failure rate."},
			failuresRate("run-failure-rate", "Run failure rate", t.runsFailed, t.runsDecided),
			{ID: "failed-jobs", Label: "Failed jobs", Value: chart.Integer(float64(t.jobsFailed))},
			{ID: "cancelled-jobs", Label: "Cancelled jobs", Value: chart.Integer(float64(t.jobsCancelled)),
				Title: "Cancelled jobs are not failures and are left out of the failure rate."},
			failuresRate("job-failure-rate", "Job failure rate", t.jobsFailed, t.jobsDecided),
		},
	}

	for _, j := range fs.Jobs[:min(failuresRowLimit, len(fs.Jobs))] {
		v.Jobs = append(v.Jobs, failuresJobRow{
			Name: j.Name, Repository: j.Repository, Workflow: j.Workflow,
			Failures: chart.Integer(float64(j.Failures)),
			Runs:     chart.Integer(float64(j.Runs)),
			Rate:     ratio(j.Failures, j.Runs),
			Href:     "/stats/jobs/" + strconv.FormatInt(j.LatestJobID, 10),
		})
	}
	v.JobsNote = failuresNote(len(v.Jobs), len(fs.Jobs), "jobs")

	for _, p := range fs.Steps[:min(failuresRowLimit, len(fs.Steps))] {
		v.Steps = append(v.Steps, failuresStepRow{
			Name:       p.Name,
			Failures:   chart.Integer(float64(p.Failures)),
			Runs:       chart.Integer(float64(p.Runs)),
			Rate:       ratio(p.Failures, p.Runs),
			JobHref:    "/stats/jobs/" + strconv.FormatInt(p.LatestJobID, 10),
			GitHubHref: github.StepURL(p.LatestJobHTMLURL, p.LatestStep),
			StepNumber: strconv.Itoa(p.LatestStep),
		})
	}
	v.StepsNote = failuresNote(len(v.Steps), len(fs.Steps), "steps")

	for _, j := range fs.Recent {
		row := failuresRecentRow{
			JobID: strconv.FormatInt(j.JobID, 10), Name: j.Name, Repository: j.Repository,
			Workflow: j.Workflow, Branch: j.Branch,
			When:     j.At.UTC().Format("Jan 2 15:04"),
			WhenFull: j.At.UTC().Format("Mon Jan 2 15:04:05 UTC"),
			Href:     "/stats/jobs/" + strconv.FormatInt(j.JobID, 10),
		}
		if j.Branch != "" {
			row.BranchHref = github.BranchURL(s.GitHubURL, j.Repository, j.Branch)
		}
		if j.StepNumber > 0 {
			row.Cause = j.StepName
			row.StepNumber = strconv.Itoa(j.StepNumber)
			row.LogHref = github.StepURL(j.HTMLURL, j.StepNumber)
		}
		v.Recent = append(v.Recent, row)
	}
	return failuresPage(v), nil
}

func failuresRate(id, label string, failed, decided int64) tile {
	return tile{
		ID: id, Label: label, Small: true, Value: ratio(failed, decided),
		Hint:  chart.Integer(float64(failed)) + " of " + chart.Integer(float64(decided)),
		Title: "Failed, out of those that completed other than cancelled or skipped.",
	}
}

// failuresNote says how many of the names a table lists, empty if all.
func failuresNote(shown, all int, noun string) string {
	if shown == all {
		return ""
	}
	return "The " + strconv.Itoa(shown) + " " + noun + " with the most failures, of " + chart.Integer(float64(all)) + "."
}

// failuresPercent formats a fraction as a percentage without a trailing
// ".0": 25%, 12.5%.
func failuresPercent(v float64) string {
	p := chart.Percent(v)
	if whole, ok := strings.CutSuffix(p, ".0%"); ok {
		return whole + "%"
	}
	return p
}

func (v failuresView) bucketLabel() string {
	if v.Step == chart.Hour {
		return "Per hour, UTC"
	}
	return "Per day, UTC"
}
