package store

import (
	"context"
	"strings"
	"time"
)

// Metrics and attribute keys gauger sends beyond the ones reports.go
// reads. A series is its attributes as k=v,k=v; ParseAttrs splits it.
const (
	MetricDiskIO         = "system.disk.io"
	MetricNetworkIO      = "system.network.io"
	AttrCPUMode          = "cpu.mode"
	AttrMemoryState      = "system.memory.state"
	AttrDevice           = "system.device"
	AttrDiskDirection    = "disk.io.direction"
	AttrInterface        = "network.interface.name"
	AttrNetworkDirection = "network.io.direction"
)

// SeriesPoint is one sample of a series.
type SeriesPoint struct {
	Time  time.Time
	Value float64
}

// GaugerSeries is one metric's points for one attribute set, oldest first.
type GaugerSeries struct {
	Metric string
	// Series is the raw attribute string, empty for the metric's total.
	Series string
	Attrs  map[string]string
	Points []SeriesPoint
}

// Attr returns the attribute key, empty when the series has none.
func (g GaugerSeries) Attr(key string) string { return g.Attrs[key] }

// ParseAttrs splits a series string into its attributes. An empty series
// has none.
func ParseAttrs(series string) map[string]string {
	if series == "" {
		return nil
	}
	attrs := map[string]string{}
	for _, part := range strings.Split(series, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			attrs[k] = v
		}
	}
	return attrs
}

// GaugerSeries returns every series gauger reported for each of the jobs,
// keyed by job ID, ordered by metric and series, with each series' points
// in time order. A job without samples has no entry.
func (s *Store) GaugerSeries(ctx context.Context, jobIDs ...int64) (map[int64][]GaugerSeries, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT job_id, metric, series, ts, value FROM samples
		WHERE job_id = ANY($1)
		ORDER BY job_id, metric, series, ts`, jobIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]GaugerSeries{}
	for rows.Next() {
		var (
			id             int64
			metric, series string
			p              SeriesPoint
		)
		if err := rows.Scan(&id, &metric, &series, &p.Time, &p.Value); err != nil {
			return nil, err
		}
		p.Time = p.Time.UTC()
		list := out[id]
		if n := len(list); n == 0 || list[n-1].Metric != metric || list[n-1].Series != series {
			list = append(list, GaugerSeries{Metric: metric, Series: series, Attrs: ParseAttrs(series)})
		}
		last := &list[len(list)-1]
		last.Points = append(last.Points, p)
		out[id] = list
	}
	return out, rows.Err()
}

// CountedInterface reports whether a network interface carries traffic
// that crossed the runner's boundary. Loopback and tailscale0 do not.
func CountedInterface(name string) bool {
	return name != "lo" && name != "tailscale0"
}

// Rate turns a cumulative counter into its growth per second between
// consecutive points, stamped at the later point. A step where the counter
// fell, as after a restart, or time did not advance has no rate.
func Rate(points []SeriesPoint) []SeriesPoint {
	var out []SeriesPoint
	for i := 1; i < len(points); i++ {
		dt := points[i].Time.Sub(points[i-1].Time).Seconds()
		dv := points[i].Value - points[i-1].Value
		if dt > 0 && dv >= 0 {
			out = append(out, SeriesPoint{points[i].Time, dv / dt})
		}
	}
	return out
}

// Increase is a cumulative counter's total growth over its points. A fall
// counts as a restart from zero.
func Increase(points []SeriesPoint) float64 {
	total := 0.0
	for i := 1; i < len(points); i++ {
		if dv := points[i].Value - points[i-1].Value; dv >= 0 {
			total += dv
		} else {
			total += points[i].Value
		}
	}
	return total
}

// StepSpan is a step's window on the job's clock, the same one Job uses to
// attribute samples: from its start to one second after its completion,
// cut short where a later step starts. GitHub times steps to the second,
// so a step that ends in the second the next begins does not overlap it.
type StepSpan struct {
	Number     int
	Start, End time.Time
}

// StepSpans returns the window of each step that has started, in step
// order. A step with no completion and no later step runs until until.
func StepSpans(steps []StepUsage, until time.Time) []StepSpan {
	var out []StepSpan
	for i, st := range steps {
		if st.StartedAt == nil {
			continue
		}
		end, bounded := until, false
		if st.CompletedAt != nil {
			end, bounded = st.CompletedAt.Add(time.Second), true
		}
		for _, later := range steps[i+1:] {
			if later.StartedAt != nil && (!bounded || later.StartedAt.Before(end)) {
				end, bounded = *later.StartedAt, true
			}
		}
		if end.Before(*st.StartedAt) {
			end = *st.StartedAt
		}
		out = append(out, StepSpan{Number: st.Number, Start: *st.StartedAt, End: end})
	}
	return out
}
