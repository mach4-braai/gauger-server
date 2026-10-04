package stats

import (
	"net/http"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// coverageSources are the series of the coverage chart, bottom of the
// stack first.
var coverageSources = []struct{ key, label, color string }{
	{"live", "Reported live", "var(--ok)"},
	{"artifact", "Fallback artifact", "var(--warn)"},
	{"none", "No samples", "var(--ink-3)"},
}

type healthView struct {
	Description string
	Tiles       []tile
	// Capped is set when the range reaches past the deliveries kept.
	Capped         bool
	Retention      string
	Deliveries     chart.TimeProps
	DeliveryLegend []chart.LegendItem
	Tasks          []taskRow
	Coverage       chart.TimeProps
	CoverageLegend []chart.LegendItem
}

type taskRow struct {
	Kind     string
	Queued   string
	Due      string
	Overdue  bool
	Oldest   string
	Title    string
	Attempts string
	Error    string
}

// deliveryWindow limits f to the deliveries the server still keeps.
func deliveryWindow(f store.Filter) store.Filter {
	if floor := f.Until.Add(-store.DeliveryRetention); f.Since.Before(floor) {
		f.Since = floor
	}
	return f
}

func (s *Server) health(r *http.Request, f store.Filter) (templ.Component, error) {
	ctx := r.Context()
	now := f.Until

	df := deliveryWindow(f)
	st := step(f)
	dst := step(df)
	deliveries, err := s.Store.DeliveriesByEvent(ctx, df.Since, df.Until, string(dst))
	if err != nil {
		return nil, err
	}
	dbuckets := chart.Buckets(df.Since, df.Until, dst)
	delivered := chart.TimeProps{
		Props: chart.Props{
			ID:     "deliveries",
			Hidden: chart.Hidden(r.URL.Query(), "deliveries"),
			Empty:  "No webhook deliveries in this range",
		},
		Buckets: dbuckets,
		Step:    dst,
	}
	delivered.Series = chart.Pivot(dbuckets, deliveries,
		func(d store.DeliveryCount) time.Time { return d.Bucket },
		func(d store.DeliveryCount) string { return d.Event },
		func(d store.DeliveryCount) float64 { return float64(d.Deliveries) },
		8)
	var received float64
	deliveryLegend := chart.Toggles(delivered.Props, r.URL)
	for i, series := range delivered.Series {
		sum := 0.0
		for _, v := range series.Values {
			sum += v
		}
		received += sum
		deliveryLegend[i].Value = chart.Integer(sum)
	}

	queue, err := s.Store.TaskQueue(ctx, now)
	if err != nil {
		return nil, err
	}
	var queued, due int64
	tasks := make([]taskRow, len(queue))
	for i, k := range queue {
		queued += k.Queued
		due += k.Due
		tasks[i] = taskRowOf(k, now)
	}

	counts, err := s.Store.CoverageByBucket(ctx, f, string(st))
	if err != nil {
		return nil, err
	}
	var first time.Time
	if len(counts) > 0 {
		first = counts[0].Bucket
	}
	cbuckets := axis(f, st, first)
	covered := chart.TimeProps{
		Props: chart.Props{
			ID:     "coverage",
			Hidden: chart.Hidden(r.URL.Query(), "coverage"),
			Empty:  "No completed jobs in this range",
		},
		Buckets: cbuckets,
		Step:    st,
	}
	var jobs, sampled, fromArtifact int64
	var coverageSums []float64
	for _, c := range coverageSources {
		values := chart.Densify(cbuckets, counts,
			func(n store.CoverageCount) time.Time { return n.Bucket },
			func(n store.CoverageCount) float64 {
				if n.Source != c.key {
					return 0
				}
				return float64(n.Jobs)
			})
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		jobs += int64(sum)
		switch c.key {
		case "live":
			sampled += int64(sum)
		case "artifact":
			sampled += int64(sum)
			fromArtifact += int64(sum)
		}
		if sum > 0 {
			covered.Series = append(covered.Series, chart.Series{Key: c.key, Label: c.label, Color: c.color, Values: values})
			coverageSums = append(coverageSums, sum)
		}
	}
	coverageLegend := chart.Toggles(covered.Props, r.URL)
	for i, sum := range coverageSums {
		coverageLegend[i].Value = chart.Integer(sum)
	}

	retention := plural(int64(store.DeliveryRetention/(24*time.Hour)), "day")
	v := healthView{
		Description:    "Webhook deliveries, the reconciler's task queue and gauger ingest in " + describe(f) + ".",
		Capped:         df.Since.After(f.Since),
		Retention:      retention,
		Deliveries:     delivered,
		DeliveryLegend: deliveryLegend,
		Tasks:          tasks,
		Coverage:       covered,
		CoverageLegend: coverageLegend,
		Tiles: []tile{
			{ID: "deliveries", Label: "Webhook deliveries", Value: chart.Integer(received), Hint: "kept for " + retention},
			{ID: "queued", Label: "Queued tasks", Value: chart.Integer(float64(queued)), Hint: "right now"},
			{ID: "due", Label: "Tasks due", Value: chart.Integer(float64(due)), Hint: "next run has passed"},
			{
				ID: "coverage", Label: "gauger coverage", Value: ratio(sampled, jobs),
				Hint: chart.Integer(float64(sampled)) + " of " + chart.Integer(float64(jobs)) + " jobs · " +
					chart.Integer(float64(fromArtifact)) + " from artifact",
				Title: "Completed jobs with runner samples, and how many of those came from the fallback artifact.",
			},
		},
	}
	return healthPage(v), nil
}

func taskRowOf(k store.TaskKind, now time.Time) taskRow {
	row := taskRow{
		Kind:     k.Kind,
		Queued:   chart.Integer(float64(k.Queued)),
		Due:      chart.Integer(float64(k.Due)),
		Overdue:  k.Due > 0,
		Attempts: chart.Integer(float64(k.MaxAttempts)),
		Title:    k.Oldest.UTC().Format("2006-01-02 15:04:05") + " UTC",
	}
	if k.Oldest.After(now) {
		row.Oldest = "in " + chart.Seconds(k.Oldest.Sub(now).Seconds())
	} else {
		row.Oldest = chart.Seconds(now.Sub(k.Oldest).Seconds()) + " ago"
	}
	if k.LastError != nil {
		row.Error = *k.LastError
	}
	return row
}

func bucketsOf(st chart.Step) string {
	if st == chart.Hour {
		return "Per hour, UTC"
	}
	return "Per day, UTC"
}
