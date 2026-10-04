package chart

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestGroupedBarHeightsFollowValues(t *testing.T) {
	p := Props{
		ID: "g", Ticks: []string{"10-01", "10-02"}, Unstacked: true,
		Format: Seconds, FormatTooltip: Seconds,
		Series: []Series{
			{Key: "ci", Label: "CI", Values: []float64{100, math.NaN()}},
			{Key: "deploy", Label: "Deploy", Values: []float64{50, 25}},
		},
	}
	c := p.plan()

	var heights []float64
	for _, s := range c.Slots {
		for _, b := range s.Bars {
			heights = append(heights, b.H)
		}
	}
	if len(heights) != 3 {
		t.Fatalf("got %d bars, want 3 (a gap leaves its slot empty)", len(heights))
	}
	if !(heights[0] > heights[1] && heights[1] > heights[2]) {
		t.Fatalf("heights = %.1f, want strictly decreasing for values 100 > 50 > 25", heights)
	}
	if ratio := heights[0] / heights[1]; ratio < 1.99 || ratio > 2.01 {
		t.Errorf("CI/Deploy height ratio = %.3f, want 2 for 100 against 50", ratio)
	}

	tip := c.Slots[0].Tip
	if tip.Title != "10-01" || len(tip.Rows) != 2 || tip.Rows[0].Label != "CI" || tip.Rows[0].Value != "1m 40s" || tip.Total != "" {
		t.Errorf("tooltip = %+v, want CI at 1m 40s then Deploy, with no total for grouped bars", tip)
	}
	if rows := c.Slots[1].Tip.Rows; len(rows) != 1 || rows[0].Label != "Deploy" {
		t.Errorf("gap tooltip rows = %+v, want only Deploy", rows)
	}

	var b strings.Builder
	if err := Chart(p).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"CI", "Deploy"} {
		if !strings.Contains(b.String(), `<span class="chart-tooltip-label">`+label+`</span>`) {
			t.Errorf("tooltips miss series %s", label)
		}
	}
}

func TestGroupedBarsStayInsideTheirSlot(t *testing.T) {
	for _, tc := range []struct{ series, slots int }{{32, 14}, {2, 14}, {40, 60}, {1, 60}, {10, 1}} {
		p := Props{ID: "g", Unstacked: true}
		for i := range tc.slots {
			p.Ticks = append(p.Ticks, fmt.Sprintf("b%02d", i))
		}
		for j := range tc.series {
			values := make([]float64, tc.slots)
			for i := range values {
				values[i] = float64(10 + j)
			}
			p.Series = append(p.Series, Series{Key: fmt.Sprintf("w%02d", j), Values: values})
		}
		c := p.plan()

		bars := 0
		for i, s := range c.Slots {
			for _, b := range s.Bars {
				bars++
				if b.W < 1 || b.X < s.X-0.05 || b.X+b.W > s.X+s.W+0.05 {
					t.Fatalf("%d series over %d slots: bar x=%v w=%v in slot %d, want width >= 1 inside [%v, %v]", tc.series, tc.slots, b.X, b.W, i, s.X, s.X+s.W)
				}
			}
		}
		if bars != tc.series*tc.slots {
			t.Fatalf("%d series over %d slots: %d bars, want %d", tc.series, tc.slots, bars, tc.series*tc.slots)
		}
	}
}

func TestChartKeepsAFixedViewBoxAtFullWidth(t *testing.T) {
	var b strings.Builder
	p := Props{ID: "w", Unstacked: true, Ticks: []string{"a", "b"}, Series: []Series{{Key: "x", Values: []float64{1, 2}}}}
	if err := Chart(p).Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `viewBox="0 0 760.0 220.0"`) {
		t.Errorf("chart should draw in a fixed viewBox that CSS scales to the container:\n%s", b.String())
	}
}

func TestRightAxisLineDoesNotStackWithGroupedBars(t *testing.T) {
	p := Props{
		ID: "r", Unstacked: true, Ticks: []string{"a", "b"},
		FormatRight: Compact,
		Series: []Series{
			{Key: "median", Values: []float64{60, 90}},
			{Key: "runs", Values: []float64{1200, 3400}, Kind: Line, Right: true},
		},
	}
	c := p.plan()
	if len(c.RightTicks) == 0 || len(c.Lines) != 1 {
		t.Fatalf("right ticks %v, lines %v; want a right axis with one line", c.RightTicks, c.Lines)
	}
	if top := c.LeftTicks[len(c.LeftTicks)-1].Label; top != "100" {
		t.Errorf("left axis tops at %s, want 100 from the median alone", top)
	}
}
