package chart

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestOffsetAndAxisStep(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0:00", 5 * time.Second: "0:05", 200 * time.Second: "3:20", 3750 * time.Second: "1:02:30", -90 * time.Second: "-1:30",
	} {
		if got := Offset(d); got != want {
			t.Errorf("Offset(%v) = %q, want %q", d, got, want)
		}
	}
	for span, want := range map[time.Duration]time.Duration{
		time.Second: time.Second, 8 * time.Second: time.Second, 9 * time.Second: 2 * time.Second,
		5 * time.Minute: 1 * time.Minute, 7 * time.Hour: 1 * time.Hour,
	} {
		if got := axisStep(span); got != want {
			t.Errorf("axisStep(%v) = %v, want %v", span, got, want)
		}
	}
}

func TestTimelinePlacesSpansOnTheAxis(t *testing.T) {
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	p := TimelineProps{
		ID: "tl", Start: t0, End: sec(100),
		Spans: []Span{
			{Label: "queue", Start: sec(0), End: sec(20)},
			{Label: "a", Start: sec(20), End: sec(60)},
			{Label: "b", Start: sec(60), End: sec(60)},
			{Label: "late", Start: sec(80), End: sec(100)},
			{Label: "skipped"},
			{Label: "outside", Start: sec(-50), End: sec(500)},
		},
	}
	tl := p.plan()
	if tl.Hover != "$_chart.tl" || tl.Signal != "tl" || tl.ID != "timeline-tl" {
		t.Errorf("ids = %q %q %q", tl.ID, tl.Signal, tl.Hover)
	}
	type bar struct {
		placed      bool
		left, width float64
		side        string
	}
	want := []bar{{true, 0, 20, "left"}, {true, 20, 40, "left"}, {true, 60, 0, "right"}, {true, 80, 20, "right"}, {false, 0, 0, "left"}, {true, 0, 100, "left"}}
	for i, w := range want {
		r := tl.Rows[i]
		if r.Placed != w.placed || math.Abs(r.Left-w.left) > 1e-9 || math.Abs(r.Width-w.width) > 1e-9 || r.Tip.Side != w.side {
			t.Errorf("row %q = placed %v left %v width %v side %s, want %+v", r.Label, r.Placed, r.Left, r.Width, r.Tip.Side, w)
		}
	}
	for i := 1; i < 3; i++ {
		if prev, r := tl.Rows[i-1], tl.Rows[i]; r.Left < prev.Left+prev.Width-1e-9 {
			t.Errorf("row %q starts at %v inside %q, which ends at %v", r.Label, r.Left, prev.Label, prev.Left+prev.Width)
		}
	}
	if n := len(tl.Ticks); n != 7 || tl.Ticks[1].Label != "0:15" || tl.Ticks[6].Left != 90 {
		t.Errorf("ticks = %+v, want seven from 0:00 every 15s of the 100s axis", tl.Ticks)
	}
}

func TestTimelineRendersRowsAndEmptyState(t *testing.T) {
	var b strings.Builder
	p := TimelineProps{ID: "tl", Start: t0, End: t0.Add(time.Minute), Spans: []Span{
		{Label: "1. Set up", Start: t0, End: t0.Add(30 * time.Second), Href: "/x?step=1", Details: []Detail{{"Result", "success"}}},
	}}
	if err := Timeline(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`class="timeline-span"`, `href="/x?step=1" data-nav`, `data-show="$_chart.tl === 0"`, `Result`, `data-signals="{_chart: {tl: -1}}"`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("timeline is missing %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	if err := Timeline(TimelineProps{ID: "tl", Start: t0, End: t0, Empty: "No step timings yet."}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "No step timings yet.") || strings.Contains(b.String(), "timeline-row") {
		t.Errorf("empty timeline = %s", b.String())
	}
}

func TestTraceSharesTheTimelineAxis(t *testing.T) {
	p := TraceProps{
		Props: Props{ID: "cpu", Series: []Series{{Key: "u", Values: []float64{1, 2, 3, 4}}}, Kind: Area},
		Start: t0, End: t0.Add(40 * time.Second), Slots: 4,
		Marks: []time.Time{t0, t0.Add(20 * time.Second), t0.Add(41 * time.Second), t0.Add(-time.Second)},
	}
	c := p.chart()
	if got := strings.Join(c.Ticks, " "); got != "0:00 0:10 0:20 0:30" {
		t.Errorf("ticks = %q", got)
	}
	if c.Titles[1] != "0:10 · 12:00:10 UTC" {
		t.Errorf("title = %q", c.Titles[1])
	}
	pl := c.plan()
	marks := p.marks(pl)
	if len(marks) != 2 {
		t.Fatalf("marks = %v, want the two inside the axis", marks)
	}
	// The mark in the middle of the axis is in the middle of the plot area.
	mid := (pl.PlotX + pl.PlotW/2) / pl.W * 100
	if want := fmt.Sprintf("left: %.3f%%", mid); !strings.HasPrefix(string(marks[1]), want) {
		t.Errorf("middle mark = %q, want it to start with %q", marks[1], want)
	}
	var b strings.Builder
	if err := Trace(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(b.String(), `class="trace-mark"`); n != 2 {
		t.Errorf("rendered %d marks, want 2", n)
	}
}

func TestBytes(t *testing.T) {
	for v, want := range map[float64]string{
		0: "0 B", 512: "512 B", 1023: "1023 B", 1024: "1 KiB", 1536: "1.5 KiB", 12 << 20: "12 MiB", 16 << 30: "16 GiB", 1.5 * (1 << 30): "1.5 GiB",
	} {
		if got := Bytes(v); got != want {
			t.Errorf("Bytes(%v) = %q, want %q", v, got, want)
		}
	}
	if got := BytesPerSecond(3 << 20); got != "3 MiB/s" {
		t.Errorf("BytesPerSecond = %q", got)
	}
}
