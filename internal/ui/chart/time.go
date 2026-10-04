package chart

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// Step is a time bucket size. Its value is the unit Postgres date_trunc
// takes, so queries can bucket with it.
type Step string

const (
	Hour Step = "hour"
	Day  Step = "day"
)

// MaxBuckets caps a time axis; Buckets keeps the most recent ones.
const MaxBuckets = 1500

// Trunc returns the start of t's bucket in UTC.
func (s Step) Trunc(t time.Time) time.Time {
	t = t.UTC()
	if s == Hour {
		return t.Truncate(time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (s Step) next(t time.Time) time.Time {
	if s == Hour {
		return t.Add(time.Hour)
	}
	return t.AddDate(0, 0, 1)
}

func (s Step) tick(t time.Time) string {
	if s == Hour {
		return t.Format("15:04")
	}
	return t.Format("Jan 2")
}

func (s Step) title(t time.Time) string {
	if s == Hour {
		return t.Format("Mon Jan 2 15:04 UTC")
	}
	return t.Format("Mon Jan 2")
}

// Buckets returns every bucket start from since's bucket through the one
// holding the last instant before until, in UTC. A zero since starts at
// until's bucket. Empty buckets are included, so a chart's axis is
// continuous.
func Buckets(since, until time.Time, step Step) []time.Time {
	last := step.Trunc(until.Add(-time.Nanosecond))
	first := last
	if !since.IsZero() {
		first = step.Trunc(since)
	}
	var out []time.Time
	for t := first; !t.After(last); t = step.next(t) {
		out = append(out, t)
	}
	if len(out) > MaxBuckets {
		out = out[len(out)-MaxBuckets:]
	}
	return out
}

// Densify sums value(p) into the bucket each point's at(p) starts, as a
// slice aligned with buckets. Points outside the axis are dropped.
func Densify[P any](buckets []time.Time, points []P, at func(P) time.Time, value func(P) float64) []float64 {
	index := bucketIndex(buckets)
	out := make([]float64, len(buckets))
	for _, p := range points {
		if i, ok := index[at(p).Unix()]; ok {
			out[i] += value(p)
		}
	}
	return out
}

// Pivot makes one series per key, ranked by total, largest first, coloured
// by rank. With limit above zero, keys past the limit fold into one
// "Other" series.
func Pivot[P any](buckets []time.Time, points []P, at func(P) time.Time, key func(P) string, value func(P) float64, limit int) []Series {
	index := bucketIndex(buckets)
	byKey := map[string][]float64{}
	for _, p := range points {
		i, ok := index[at(p).Unix()]
		if !ok {
			continue
		}
		k := key(p)
		if byKey[k] == nil {
			byKey[k] = make([]float64, len(buckets))
		}
		byKey[k][i] += value(p)
	}
	type ranked struct {
		key    string
		values []float64
		total  float64
	}
	var all []ranked
	for k, v := range byKey {
		total := 0.0
		for _, x := range v {
			total += x
		}
		if total != 0 {
			all = append(all, ranked{k, v, total})
		}
	}
	slices.SortFunc(all, func(a, b ranked) int {
		return cmp.Or(cmp.Compare(b.total, a.total), cmp.Compare(a.key, b.key))
	})
	var out []Series
	for i, r := range all {
		if limit > 0 && i == limit {
			other := make([]float64, len(buckets))
			for _, rest := range all[limit:] {
				for j, v := range rest.values {
					other[j] += v
				}
			}
			out = append(out, Series{Key: "other", Label: fmt.Sprintf("Other (%d)", len(all)-limit), Color: OtherColor, Values: other})
			break
		}
		out = append(out, Series{Key: r.key, Label: r.key, Color: SeriesColors[i%len(SeriesColors)], Values: r.values})
	}
	return out
}

func bucketIndex(buckets []time.Time) map[int64]int {
	index := make(map[int64]int, len(buckets))
	for i, t := range buckets {
		index[t.Unix()] = i
	}
	return index
}

// TimeProps is a Chart over UTC time buckets. Leave Ticks, Titles and
// Select unset; TimeChart fills them from Buckets.
type TimeProps struct {
	Props
	// Buckets are ascending bucket starts, as Buckets returns them.
	Buckets []time.Time
	Step    Step
	// SelectBucket links a bucket; clicking it follows the link in place.
	SelectBucket func(start time.Time) string
}

func (p TimeProps) chart() Props {
	c := p.Props
	c.Ticks = make([]string, len(p.Buckets))
	c.Titles = make([]string, len(p.Buckets))
	for i, t := range p.Buckets {
		c.Ticks[i] = p.Step.tick(t)
		c.Titles[i] = p.Step.title(t)
	}
	if p.SelectBucket != nil {
		c.Select = make([]string, len(p.Buckets))
		for i, t := range p.Buckets {
			c.Select[i] = p.SelectBucket(t)
		}
	}
	return c
}

// SeriesColors are ten mid-luminance hues that read on the light and dark
// themes, from omp's palette.
var SeriesColors = []string{
	"#5ad8e6", "#ed4abf", "#9d7bff", "#f5b54a", "#4ade80",
	"#5b8cff", "#ff7a59", "#2dd4bf", "#c3e94f", "#fb7185",
}

// OtherColor is the neutral for "Other" rollups.
const OtherColor = "#6c6c74"
