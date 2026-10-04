package stats

import (
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

var capWeekdays = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

type capacityView struct {
	Description   string
	Tiles         []tile
	Labels        []labelRow
	Step          chart.Step
	Minutes       chart.TimeProps
	MinutesLegend []chart.LegendItem
	Queue         chart.TimeProps
	QueueLegend   []chart.LegendItem
	Hours         chart.Props
	HoursLegend   []chart.LegendItem
	Heat          chart.HeatmapProps
	Peak          chart.TimeProps
}

// labelRow is one line of the label table, formatted.
type labelRow struct {
	Name       string
	Color      string
	Jobs       string
	Minutes    string
	Spend      string
	SpendTitle string
	QueueP50   string
	QueueP95   string
}

func (s *Server) capacity(r *http.Request, f store.Filter) (templ.Component, error) {
	ctx := r.Context()
	stats, err := s.Store.LabelStats(ctx, f)
	if err != nil {
		return nil, err
	}
	st := step(f)
	minutes, err := s.Store.LabelMinutesByBucket(ctx, f, string(st))
	if err != nil {
		return nil, err
	}
	loads, err := s.Store.LoadByBucket(ctx, f, string(st))
	if err != nil {
		return nil, err
	}
	hourly, err := s.Store.QueueByHour(ctx, f)
	if err != nil {
		return nil, err
	}
	delay, err := s.Store.RunStartDelay(ctx, f)
	if err != nil {
		return nil, err
	}
	weekHours, err := s.Store.RunsByWeekdayHour(ctx, f)
	if err != nil {
		return nil, err
	}

	var first time.Time
	for _, b := range []time.Time{capFirst(minutes, func(m store.LabelBucketMinutes) time.Time { return m.Bucket }),
		capFirst(loads, func(l store.BucketLoad) time.Time { return l.Bucket })} {
		if !b.IsZero() && (first.IsZero() || b.Before(first)) {
			first = b
		}
	}
	buckets := axis(f, st, first)
	q := r.URL.Query()
	timeProps := func(id, empty string) chart.TimeProps {
		return chart.TimeProps{
			Props:   chart.Props{ID: id, Hidden: chart.Hidden(q, id), Empty: empty},
			Buckets: buckets,
			Step:    st,
		}
	}

	minutesChart := timeProps("minutes", "No job minutes in this range")
	minutesChart.FormatTooltip = chart.Integer
	minutesChart.Series = chart.Pivot(buckets, minutes,
		func(m store.LabelBucketMinutes) time.Time { return m.Bucket },
		func(m store.LabelBucketMinutes) string { return store.LabelName(m.Labels) },
		func(m store.LabelBucketMinutes) float64 { return float64(m.Minutes) }, 0)
	minutesLegend := chart.Toggles(minutesChart.Props, r.URL)
	for i, series := range minutesChart.Series {
		minutesLegend[i].Value = chart.Integer(capSum(series.Values))
	}

	queueChart := timeProps("queue", "No jobs started in this range")
	queueChart.Kind = chart.Line
	queueChart.Format = chart.Seconds
	queueChart.Series = []chart.Series{
		{Key: "p50", Label: "p50", Color: "var(--chart-primary)", Values: capSparse(buckets, loads, func(l store.BucketLoad) (time.Time, *float64) { return l.Bucket, l.QueueP50 })},
		{Key: "p95", Label: "p95", Color: "var(--chart-secondary)", Values: capSparse(buckets, loads, func(l store.BucketLoad) (time.Time, *float64) { return l.Bucket, l.QueueP95 })},
	}

	peakChart := timeProps("peak", "No jobs ran in this range")
	peakChart.Format = chart.Integer
	peakChart.NoTotal = true
	peakChart.Series = []chart.Series{{
		Key: "peak", Label: "Peak concurrent jobs", Color: "var(--chart-primary)",
		Values: chart.Densify(buckets, loads,
			func(l store.BucketLoad) time.Time { return l.Bucket },
			func(l store.BucketLoad) float64 { return float64(l.Peak) }),
	}}
	var peak int64
	for _, l := range loads {
		peak = max(peak, l.Peak)
	}

	rows := make([]labelRow, len(stats))
	byName := map[string][]float64{}
	for _, h := range hourly {
		name := store.LabelName(h.Labels)
		if byName[name] == nil {
			byName[name] = capFilled(24, math.NaN())
		}
		byName[name][h.Hour] = h.P95
	}
	hours := chart.Props{
		ID:     "hours",
		Ticks:  make([]string, 24),
		Titles: make([]string, 24),
		Kind:   chart.Line,
		Format: chart.Seconds,
		Hidden: chart.Hidden(q, "hours"),
		Empty:  "No jobs started in this range",
	}
	for h := range 24 {
		hours.Ticks[h] = capHour(h)
		hours.Titles[h] = capHour(h) + ":00 UTC"
	}
	for i, l := range stats {
		color := chart.SeriesColors[i%len(chart.SeriesColors)]
		cost, unpriced := s.labelSpend(l)
		row := labelRow{
			Name: l.Name(), Color: color,
			Jobs: chart.Integer(float64(l.Jobs)), Minutes: chart.Integer(float64(l.Minutes)),
			Spend:    chart.USD(cost),
			QueueP50: capSeconds(l.QueueP50), QueueP95: capSeconds(l.QueueP95),
		}
		if unpriced > 0 {
			row.SpendTitle = chart.Integer(float64(unpriced)) + " min have no rate"
			if unpriced == l.Minutes {
				row.Spend = "–"
			}
		}
		rows[i] = row
		if values, ok := byName[row.Name]; ok {
			hours.Series = append(hours.Series, chart.Series{Key: row.Name, Label: row.Name, Color: color, Values: values})
		}
	}

	grid := make([][]float64, len(capWeekdays))
	for i := range grid {
		grid[i] = make([]float64, 24)
	}
	for _, c := range weekHours {
		grid[c.Weekday][c.Hour] = float64(c.Runs)
	}
	heat := chart.HeatmapProps{
		ID: "heat", Rows: capWeekdays, Cols: hours.Ticks, ColTitles: hours.Titles,
		Values: grid, Unit: "Runs", Empty: "No runs in this range",
	}

	v := capacityView{
		Description: "Where job minutes run and how long jobs wait, in " + describe(f) + ".",
		Tiles: []tile{
			seconds("start-p50", "Run start p50", delay.P50, "From a run's creation to its first job starting."),
			seconds("start-p95", "Run start p95", delay.P95, "From a run's creation to its first job starting."),
			{ID: "peak", Label: "Peak concurrent jobs", Value: chart.Integer(float64(peak)), Small: true,
				Title: "The most jobs running at once, from job start and end times."},
		},
		Labels:        rows,
		Step:          st,
		Minutes:       minutesChart,
		MinutesLegend: minutesLegend,
		Queue:         queueChart,
		QueueLegend:   chart.Toggles(queueChart.Props, r.URL),
		Hours:         hours,
		HoursLegend:   chart.Toggles(hours, r.URL),
		Heat:          heat,
		Peak:          peakChart,
	}
	return capacityPage(v), nil
}

// labelSpend prices a label set's minutes at s.Rates and returns the cost
// and the minutes no rate covers.
func (s *Server) labelSpend(l store.LabelStat) (cost float64, unpriced int64) {
	for _, b := range l.Billing {
		p := s.Rates.Price(b.Labels, b.Private, b.Minutes)
		cost += p.Cost
		if !p.Known {
			unpriced += b.Minutes
		}
	}
	return cost, unpriced
}

func capSeconds(v *float64) string {
	if v == nil {
		return "–"
	}
	return chart.Seconds(*v)
}

func capHour(h int) string {
	if h < 10 {
		return "0" + strconv.Itoa(h)
	}
	return strconv.Itoa(h)
}

func capFilled(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func capSum(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

// capFirst is the bucket of the first of points, which come in bucket
// order, or the zero time without any.
func capFirst[P any](points []P, at func(P) time.Time) time.Time {
	if len(points) == 0 {
		return time.Time{}
	}
	return at(points[0])
}

// capSparse aligns the values of points with buckets, leaving NaN where a
// bucket has no point or its value is nil.
func capSparse[P any](buckets []time.Time, points []P, at func(P) (time.Time, *float64)) []float64 {
	index := make(map[int64]int, len(buckets))
	for i, b := range buckets {
		index[b.Unix()] = i
	}
	out := capFilled(len(buckets), math.NaN())
	for _, p := range points {
		t, v := at(p)
		if i, ok := index[t.Unix()]; ok && v != nil {
			out[i] = *v
		}
	}
	return out
}

func (v capacityView) bucketLabel() string {
	if v.Step == chart.Hour {
		return "Per hour, UTC"
	}
	return "Per day, UTC"
}
