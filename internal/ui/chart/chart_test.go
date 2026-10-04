package chart

import (
	"context"
	"math"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNiceScale(t *testing.T) {
	cases := []struct {
		max   float64
		top   float64
		ticks []float64
	}{
		{0, 1, []float64{0}},
		{-3, 1, []float64{0}},
		{math.NaN(), 1, []float64{0}},
		{1, 1, []float64{0, 0.25, 0.5, 0.75, 1}},
		{7, 8, []float64{0, 2, 4, 6, 8}},
		{10, 10, []float64{0, 2.5, 5, 7.5, 10}},
		{37, 40, []float64{0, 10, 20, 30, 40}},
		{1234, 1500, []float64{0, 500, 1000, 1500}},
		{0.3, 0.3, []float64{0, 0.1, 0.2, 0.3}},
	}
	for _, c := range cases {
		got := NiceScale(c.max)
		if got.Max != c.top || !slices.Equal(got.Ticks, c.ticks) {
			t.Errorf("NiceScale(%v) = %v %v, want %v %v", c.max, got.Max, got.Ticks, c.top, c.ticks)
		}
	}
}

func TestStackPutsEachSeriesOnTheOnesBefore(t *testing.T) {
	series := []Series{
		{Key: "a", Values: []float64{1, 2, math.NaN()}},
		{Key: "b", Values: []float64{3, 0, 4}},
		{Key: "c", Values: []float64{5, 6, 7}},
	}
	bases := Stack(series, 3)
	want := [][]float64{{0, 0, 0}, {1, 2, 0}, {4, 2, 4}}
	for i := range want {
		if !slices.Equal(bases[i], want[i]) {
			t.Errorf("base of %s = %v, want %v", series[i].Key, bases[i], want[i])
		}
	}
}

func TestStackedBarsRecomputeWhenASeriesIsHidden(t *testing.T) {
	p := Props{
		ID:    "runs",
		Ticks: []string{"a", "b"},
		Series: []Series{
			{Key: "ok", Label: "Succeeded", Color: "green", Values: []float64{6, 2}},
			{Key: "bad", Label: "Failed", Color: "red", Values: []float64{2, 2}},
		},
	}
	all := p.plan()
	if got := len(all.Slots[0].Bars); got != 2 {
		t.Fatalf("slot 0 has %d bars, want 2", got)
	}
	ok, bad := all.Slots[0].Bars[0], all.Slots[0].Bars[1]
	if math.Abs(ok.Y-(bad.Y+bad.H)) > 1e-9 {
		t.Errorf("failed bar ends at %.2f, want it on top of the succeeded bar at %.2f", bad.Y+bad.H, ok.Y)
	}
	if all.LeftTicks[len(all.LeftTicks)-1].Label != "8" {
		t.Errorf("top tick = %q, want 8 for a stack of 6+2", all.LeftTicks[len(all.LeftTicks)-1].Label)
	}
	tip := all.Slots[0].Tip
	if len(tip.Rows) != 2 || tip.Rows[0].Label != "Succeeded" || tip.Total != "8" {
		t.Errorf("tooltip = %+v, want both rows, largest first, and a total of 8", tip)
	}

	p.Hidden = map[string]bool{"ok": true}
	hidden := p.plan()
	if got := len(hidden.Slots[0].Bars); got != 1 {
		t.Fatalf("slot 0 has %d bars with ok hidden, want 1", got)
	}
	if b := hidden.Slots[0].Bars[0]; math.Abs(b.Y+b.H-hidden.Baseline.Y) > 1e-9 {
		t.Errorf("failed bar ends at %.2f, want it on the baseline at %.2f once succeeded is hidden", b.Y+b.H, hidden.Baseline.Y)
	}
	if top := hidden.LeftTicks[len(hidden.LeftTicks)-1].Label; top != "2" {
		t.Errorf("top tick = %q, want 2 with only failures shown", top)
	}
	if tip := hidden.Slots[0].Tip; len(tip.Rows) != 1 || tip.Total != "" {
		t.Errorf("tooltip = %+v, want one row and no total", tip)
	}
}

func TestGroupedBarsSplitTheSlot(t *testing.T) {
	p := Props{
		ID: "g", Ticks: []string{"a"}, Unstacked: true,
		Series: []Series{{Key: "x", Values: []float64{4}}, {Key: "y", Values: []float64{2}}},
	}
	bars := p.plan().Slots[0].Bars
	if len(bars) != 2 || bars[1].X <= bars[0].X || bars[0].Y+bars[0].H != bars[1].Y+bars[1].H {
		t.Errorf("bars = %+v, want two side by side on the baseline", bars)
	}
}

func TestEmptyChart(t *testing.T) {
	p := Props{ID: "e", Ticks: []string{"a", "b"}, Series: []Series{{Key: "x", Values: []float64{0, math.NaN()}}}}
	c := p.plan()
	if !c.Empty || c.EmptyLabel != "No data in this range" {
		t.Errorf("Empty = %v %q, want the default empty label", c.Empty, c.EmptyLabel)
	}
	var b strings.Builder
	if err := Chart(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `<div class="chart-empty">No data in this range</div>`) || strings.Contains(b.String(), "chart-tooltip") {
		t.Errorf("empty chart should show its label and no tooltips:\n%s", b.String())
	}
	if (Props{ID: "n"}).plan().Empty != true {
		t.Error("a chart with no slots should be empty")
	}
}

func TestLineBreaksAtGapsAndMarksLonePoints(t *testing.T) {
	p := Props{ID: "l", Kind: Line, Ticks: []string{"a", "b", "c", "d"},
		Series: []Series{{Key: "x", Color: "c", Values: []float64{1, 2, math.NaN(), 3}}}}
	c := p.plan()
	if d := c.Lines[0].D; strings.Count(d, "M") != 2 || strings.Count(d, "L") != 1 {
		t.Errorf("path %q, want a two-point segment and a lone point", d)
	}
	if len(c.Dots) != 1 {
		t.Errorf("dots = %v, want one for the lone last point", c.Dots)
	}
}

func TestBucketsFillEveryBucketInRange(t *testing.T) {
	since := time.Date(2026, 10, 1, 13, 20, 0, 0, time.UTC)
	until := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	days := Buckets(since, until, Day)
	if len(days) != 4 || !days[0].Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !days[3].Equal(time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("day buckets = %v, want Oct 1 through Oct 4", days)
	}
	hours := Buckets(until.Add(-24*time.Hour), until, Hour)
	if len(hours) != 24 || !hours[23].Equal(until.Add(-time.Hour)) {
		t.Errorf("got %d hour buckets ending %v, want 24 ending at 08:00 since until is exclusive", len(hours), hours[len(hours)-1])
	}
	if got := Buckets(time.Time{}, until, Day); len(got) != 1 {
		t.Errorf("zero since gave %d buckets, want only until's", len(got))
	}
	if got := Buckets(until.AddDate(-10, 0, 0), until, Day); len(got) != MaxBuckets {
		t.Errorf("ten years gave %d buckets, want the cap of %d", len(got), MaxBuckets)
	}

	type point struct {
		at time.Time
		n  float64
	}
	points := []point{
		{days[0], 2}, {days[0], 3}, {days[2], 1},
		{days[0].AddDate(0, 0, -1), 9},
	}
	got := Densify(days, points, func(p point) time.Time { return p.at }, func(p point) float64 { return p.n })
	if !slices.Equal(got, []float64{5, 0, 1, 0}) {
		t.Errorf("Densify = %v, want [5 0 1 0] with the empty days kept and the point before the axis dropped", got)
	}
}

func TestPivotRanksAndFoldsOther(t *testing.T) {
	b := []time.Time{time.Unix(0, 0).UTC(), time.Unix(86400, 0).UTC()}
	type point struct {
		at  time.Time
		key string
		n   float64
	}
	points := []point{{b[0], "a", 1}, {b[1], "b", 5}, {b[0], "c", 2}, {b[1], "c", 1}, {b[0], "d", 0.5}}
	got := Pivot(b, points, func(p point) time.Time { return p.at }, func(p point) string { return p.key }, func(p point) float64 { return p.n }, 2)
	if len(got) != 3 || got[0].Key != "b" || got[1].Key != "c" || got[2].Label != "Other (2)" || !slices.Equal(got[2].Values, []float64{1.5, 0}) {
		t.Errorf("Pivot = %+v, want b, c, then Other (2) holding a and d", got)
	}
}

func TestTogglesFlipHideParam(t *testing.T) {
	u, _ := url.Parse("/stats/?range=7d&hide=runs:failure&hide=other:x")
	p := Props{ID: "runs", Series: []Series{{Key: "success"}, {Key: "failure"}}}
	p.Hidden = Hidden(u.Query(), "runs")
	items := Toggles(p, u)
	if items[0].Off || items[0].Href != "/stats/?hide=runs%3Afailure&hide=other%3Ax&hide=runs%3Asuccess&range=7d" {
		t.Errorf("success item = %+v, want shown, linking to hide it too", items[0])
	}
	if !items[1].Off || items[1].Href != "/stats/?hide=other%3Ax&range=7d" {
		t.Errorf("failure item = %+v, want hidden, linking to show it", items[1])
	}
}

func TestFormats(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{Integer(1234567.4), "1,234,567"},
		{Integer(-1200), "-1,200"},
		{Compact(950), "950"},
		{Compact(1234), "1.2K"},
		{Compact(34_500), "35K"},
		{Compact(5_600_000), "5.6M"},
		{Compact(2.5), "2.5"},
		{Percent(0.975), "97.5%"},
		{Seconds(42.4), "42s"},
		{Seconds(185), "3m 05s"},
		{Seconds(3720), "1h 02m"},
		{USD(0), "$0"},
		{USD(1234.5), "$1,234.50"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
