package stats

import (
	"fmt"
	"net/http"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// conclusions are the series of the runs chart, bottom of the stack first.
var conclusions = []struct{ key, label, color string }{
	{"success", "Succeeded", "var(--ok)"},
	{"failure", "Failed", "var(--bad)"},
	{"cancelled", "Cancelled", "var(--ink-3)"},
	{"other", "Other", "var(--warn)"},
	{"running", "In progress", "var(--chart-primary)"},
}

// conclusionKey files a run's conclusion under a series of the runs chart.
// An empty conclusion is a run that has not completed.
func conclusionKey(c string) string {
	switch c {
	case "success", "failure", "cancelled":
		return c
	case "":
		return "running"
	}
	return "other"
}

type overviewView struct {
	Description string
	Main        []tile
	Detail      []tile
	Step        chart.Step
	Runs        chart.TimeProps
	Legend      []chart.LegendItem
}

func (s *Server) overview(r *http.Request, f store.Filter) (templ.Component, error) {
	ctx := r.Context()
	o, err := s.Store.Overview(ctx, f)
	if err != nil {
		return nil, err
	}
	st := step(f)
	counts, err := s.Store.RunsByConclusion(ctx, f, string(st))
	if err != nil {
		return nil, err
	}
	var first time.Time
	if len(counts) > 0 {
		first = counts[0].Bucket
	}
	buckets := axis(f, st, first)
	runs := chart.TimeProps{
		Props: chart.Props{
			ID:     "runs",
			Hidden: chart.Hidden(r.URL.Query(), "runs"),
			Empty:  "No runs in this range",
		},
		Buckets: buckets,
		Step:    st,
	}
	total := make([]float64, len(buckets))
	var sums []float64
	for _, c := range conclusions {
		values := chart.Densify(buckets, counts,
			func(n store.ConclusionCount) time.Time { return n.Bucket },
			func(n store.ConclusionCount) float64 {
				if conclusionKey(n.Conclusion) != c.key {
					return 0
				}
				return float64(n.Runs)
			})
		sum := 0.0
		for i, v := range values {
			total[i] += v
			sum += v
		}
		if sum > 0 {
			runs.Series = append(runs.Series, chart.Series{Key: c.key, Label: c.label, Color: c.color, Values: values})
			sums = append(sums, sum)
		}
	}
	legend := chart.Toggles(runs.Props, r.URL)
	for i, sum := range sums {
		legend[i].Value = chart.Integer(sum)
	}

	var minutes, unpriced int64
	var cost float64
	for _, m := range o.Minutes {
		minutes += m.Minutes
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

	v := overviewView{
		Description: "Runs, jobs and steps in " + describe(f) + ".",
		Main: []tile{
			{ID: "runs", Label: "Runs", Value: chart.Integer(float64(o.Runs)), Hint: plural(o.ReRuns, "re-run"), Spark: total},
			{ID: "jobs", Label: "Jobs", Value: chart.Integer(float64(o.Jobs))},
			{ID: "steps", Label: "Steps", Value: chart.Integer(float64(o.Steps))},
			{ID: "minutes", Label: "Job minutes", Value: chart.Integer(float64(minutes)), Hint: "each job rounded up to a minute"},
			{ID: "spend", Label: "Estimated spend", Value: chart.USD(cost), Hint: spendHint,
				Title: "Job minutes times the runner label's rate. Standard runners in public repositories and self-hosted runners are free."},
		},
		Detail: []tile{
			rate("run-success", "Run success", o.RunsSucceeded, o.RunsDecided),
			rate("job-success", "Job success", o.JobsSucceeded, o.JobsDecided),
			seconds("run-p50", "Run duration p50", o.RunP50, "From a run's first job start to its last job end."),
			seconds("run-p95", "Run duration p95", o.RunP95, "From a run's first job start to its last job end."),
			seconds("queue-p50", "Queue p50", o.QueueP50, "A job's start minus its creation."),
			seconds("queue-p95", "Queue p95", o.QueueP95, "A job's start minus its creation."),
			{
				ID: "coverage", Label: "gauger coverage", Small: true,
				Value: ratio(o.JobsWithSamples, o.Jobs),
				Hint: fmt.Sprintf("%s of %s jobs · %s from artifact",
					chart.Integer(float64(o.JobsWithSamples)), chart.Integer(float64(o.Jobs)), chart.Integer(float64(o.JobsFromArtifact))),
				Title: "Jobs with runner samples, and how many of those came from the fallback artifact.",
			},
		},
		Step:   st,
		Runs:   runs,
		Legend: legend,
	}
	return overviewPage(v), nil
}

func rate(id, label string, n, of int64) tile {
	return tile{
		ID: id, Label: label, Small: true, Value: ratio(n, of),
		Hint:  chart.Integer(float64(n)) + " of " + chart.Integer(float64(of)),
		Title: "Completed with success, out of those that completed other than cancelled or skipped.",
	}
}

func ratio(n, of int64) string {
	if of == 0 {
		return "–"
	}
	return chart.Percent(float64(n) / float64(of))
}

func seconds(id, label string, v *float64, title string) tile {
	t := tile{ID: id, Label: label, Small: true, Value: "–", Title: title}
	if v != nil {
		t.Value = chart.Seconds(*v)
	}
	return t
}

func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return chart.Integer(float64(n)) + " " + noun + "s"
}

func (v overviewView) bucketLabel() string {
	if v.Step == chart.Hour {
		return "Per hour, UTC"
	}
	return "Per day, UTC"
}
