package chart

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
)

func TestHeatmapCellsAddUpToTheInput(t *testing.T) {
	p := HeatmapProps{
		ID:     "heat",
		Rows:   []string{"Mon", "Tue", "Wed"},
		Cols:   []string{"00", "01", "02", "03"},
		Values: [][]float64{{1, 0, 4, 2}, {0, 0, 0, 7}, {3, 3}},
	}
	h := p.plan()
	if got, want := len(h.Cells), 12; got != want {
		t.Fatalf("%d cells, want %d", got, want)
	}
	if h.Total != 20 {
		t.Errorf("total = %v, want 20", h.Total)
	}
	sum := 0.0
	for _, c := range h.Cells {
		sum += c.Value
	}
	if sum != 20 {
		t.Errorf("cells add up to %v, want 20", sum)
	}
	if h.Empty {
		t.Error("a grid with counts is empty")
	}
}

func TestHeatmapCellsTileTheGridInReadingOrder(t *testing.T) {
	p := HeatmapProps{
		ID:     "heat",
		Rows:   []string{"Mon", "Tue"},
		Cols:   []string{"a", "b", "c"},
		Values: [][]float64{{1, 2, 3}, {4, 5, 6}},
		Width:  400, CellHeight: 20,
	}
	h := p.plan()
	if h.W != 400 || h.H != heatPadTop+2*20+heatPadBottom {
		t.Errorf("viewBox %v by %v", h.W, h.H)
	}
	for i, c := range h.Cells {
		if c.Slot != i || c.Value != float64(i+1) {
			t.Errorf("cell %d has slot %d and value %v", i, c.Slot, c.Value)
		}
		if c.X < 0 || c.X+c.W > h.W || c.Y < heatPadTop || c.Y+c.H > h.H-heatPadBottom {
			t.Errorf("cell %d at %v,%v size %v by %v leaves the plot", i, c.X, c.Y, c.W, c.H)
		}
	}
	a, b, below := h.Cells[0], h.Cells[1], h.Cells[3]
	if math.Abs((b.X-a.X)-(a.W+heatGap)) > 1e-9 {
		t.Errorf("columns are %v apart, want %v", b.X-a.X, a.W+heatGap)
	}
	if a.Y != b.Y || math.Abs((below.Y-a.Y)-(a.H+heatGap)) > 1e-9 || a.X != below.X {
		t.Errorf("rows do not line up: %+v %+v %+v", a, b, below)
	}
	if len(h.RowTicks) != 2 || h.RowTicks[0].Label != "Mon" || h.RowTicks[0].Y != a.Y-heatGap/2+10 {
		t.Errorf("row ticks %+v", h.RowTicks)
	}
	if len(h.ColTicks) != 3 || h.ColTicks[1].X != b.X+b.W/2 {
		t.Errorf("column ticks %+v", h.ColTicks)
	}
}

func TestHeatmapOpacityFollowsShareOfTheLargestCell(t *testing.T) {
	h := HeatmapProps{
		ID:     "heat",
		Rows:   []string{"r"},
		Cols:   []string{"a", "b", "c"},
		Values: [][]float64{{0, 5, 10}},
		Color:  "red",
	}.plan()
	zero, half, full := h.Cells[0], h.Cells[1], h.Cells[2]
	if zero.Fill != "var(--chart-grid)" || zero.Opacity != 1 {
		t.Errorf("empty cell is %s at %v", zero.Fill, zero.Opacity)
	}
	if full.Fill != "red" || full.Opacity != 1 {
		t.Errorf("largest cell is %s at %v, want solid", full.Fill, full.Opacity)
	}
	want := heatMinAlpha + (1-heatMinAlpha)*0.5
	if half.Fill != "red" || math.Abs(half.Opacity-want) > 1e-9 {
		t.Errorf("half cell is %s at %v, want %v", half.Fill, half.Opacity, want)
	}
}

func TestHeatmapColumnLabelsThinOutWhenTheyDoNotFit(t *testing.T) {
	cols := make([]string, 24)
	for i := range cols {
		cols[i] = "00:00 UTC"
	}
	h := HeatmapProps{ID: "heat", Rows: []string{"r"}, Cols: cols, Values: [][]float64{make([]float64, 24)}, Width: 300}.plan()
	if len(h.ColTicks) >= 24 || len(h.ColTicks) == 0 {
		t.Errorf("%d of 24 column labels shown on a narrow grid", len(h.ColTicks))
	}
	for i := 1; i < len(h.ColTicks); i++ {
		if gap := h.ColTicks[i].X - h.ColTicks[i-1].X; gap < labelWidth(cols) {
			t.Errorf("labels %d and %d are %v apart, less than a label's %v", i-1, i, gap, labelWidth(cols))
		}
	}
}

func TestHeatmapTooltipSitsAwayFromTheCellOnTheNearerSide(t *testing.T) {
	h := HeatmapProps{
		ID: "heat", Rows: []string{"Mon"}, Cols: []string{"a", "b", "c", "d"}, ColTitles: []string{"00:00 UTC", "01:00 UTC", "02:00 UTC", "03:00 UTC"},
		Values: [][]float64{{1, 2, 3, 4}}, Unit: "Runs",
	}.plan()
	if h.Cells[0].Tip.Side != "left" || h.Cells[3].Tip.Side != "right" {
		t.Errorf("tooltips on %s and %s, want left and right", h.Cells[0].Tip.Side, h.Cells[3].Tip.Side)
	}
	tip := h.Cells[2].Tip
	if tip.Title != "Mon 02:00 UTC" || len(tip.Rows) != 1 || tip.Rows[0].Label != "Runs" || tip.Rows[0].Value != "3" {
		t.Errorf("tooltip %+v", tip)
	}
}

func TestHeatmapWithoutCountsIsEmpty(t *testing.T) {
	p := HeatmapProps{ID: "heat", Rows: []string{"Mon"}, Cols: []string{"a"}, Values: [][]float64{{0}}, Empty: "No runs"}
	h := p.plan()
	if !h.Empty || h.EmptyLabel != "No runs" {
		t.Errorf("plan = empty %v, label %q", h.Empty, h.EmptyLabel)
	}
	var out bytes.Buffer
	if err := Heatmap(p).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No runs") || strings.Contains(out.String(), "chart-tooltip") {
		t.Errorf("empty heatmap renders %s", out.String())
	}
}

func TestHeatmapRendersEachCellWithItsValueAndTooltip(t *testing.T) {
	var out bytes.Buffer
	err := Heatmap(HeatmapProps{
		ID: "heat", Rows: []string{"Mon", "Tue"}, Cols: []string{"a", "b"},
		Values: [][]float64{{1, 2}, {3, 1234}}, Unit: "Runs",
	}).Render(context.Background(), &out)
	if err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		`id="chart-heat"`, `data-value="1234"`, `1,234`, `data-show="$_chart.heat === 3"`, `data-on:pointerenter="$_chart.heat = 3"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("heatmap lacks %s:\n%s", want, html)
		}
	}
	if got := strings.Count(html, `class="heatmap-cell"`); got != 4 {
		t.Errorf("%d cells rendered, want 4", got)
	}
	if got := strings.Count(html, `class="chart-tooltip"`); got != 4 {
		t.Errorf("%d tooltips rendered, want 4", got)
	}
}
