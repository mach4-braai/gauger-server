package chart

import "math"

// HeatmapProps describes a grid of counts, one cell per row and column.
type HeatmapProps struct {
	// ID names the chart's element and its hover signal. Use letters and
	// digits, unique on the page.
	ID string
	// Rows label the grid top to bottom, Cols left to right.
	Rows, Cols []string
	// ColTitles head a cell's tooltip after its row label; nil uses Cols.
	ColTitles []string
	// Values[r][c] is the cell at row r and column c. Missing cells are
	// zero.
	Values [][]float64
	// Unit names the value in the tooltip. Default "Count".
	Unit string
	// Format labels tooltip values. Default Integer.
	Format func(float64) string
	// Color is the cell colour at full intensity. Default the theme's
	// primary chart colour.
	Color string
	// Width sizes the viewBox; CellHeight is the height of a row. Default
	// 760 and 26.
	Width, CellHeight float64
	// Empty is shown when every cell is zero.
	Empty string
}

const (
	heatPadTop    = 4.0
	heatPadBottom = 24.0
	heatGap       = 2.0
	heatMinAlpha  = 0.18
)

type heatmap struct {
	ID         string
	Hover      string
	W, H       float64
	Cells      []heatCell
	RowTicks   []tick
	ColTicks   []tick
	Total      float64
	Empty      bool
	EmptyLabel string
}

type heatCell struct {
	X, Y, W, H float64
	Fill       string
	Opacity    float64
	Value      float64
	// Slot is the cell's position in reading order, which the hover
	// signal holds while the pointer is over it.
	Slot int
	Tip  tooltip
}

// plan lays the grid out: the row labels take the width they need and the
// columns share the rest. A cell's opacity grows with its share of the
// largest cell, so the busiest cell is solid and an empty one is a bare
// grid square.
func (p HeatmapProps) plan() heatmap {
	w := cmpOr(p.Width, 760)
	cellH := cmpOr(p.CellHeight, 26)
	format := p.Format
	if format == nil {
		format = Integer
	}
	color := cmpOr(p.Color, "var(--chart-primary)")
	unit := cmpOr(p.Unit, "Count")
	rows, cols := len(p.Rows), len(p.Cols)

	padLeft := max(28, labelWidth(p.Rows)+12)
	padRight := 8.0
	plotW := max(0, w-padLeft-padRight)
	cellW := 0.0
	if cols > 0 {
		cellW = plotW / float64(cols)
	}
	h := heatPadTop + float64(rows)*cellH + heatPadBottom

	id := nonIdent.ReplaceAllString(p.ID, "_")
	out := heatmap{ID: "chart-" + id, Hover: "$_chart." + id, W: w, H: h, EmptyLabel: cmpOr(p.Empty, "No data in this range")}

	peak := 0.0
	for r := range rows {
		for c := range cols {
			v := p.at(r, c)
			out.Total += v
			peak = max(peak, v)
		}
	}
	out.Empty = peak == 0

	for r, label := range p.Rows {
		out.RowTicks = append(out.RowTicks, tick{padLeft - 8, heatPadTop + (float64(r)+0.5)*cellH, label})
	}
	every := 1
	if cellW > 0 {
		every = max(1, int(math.Ceil((labelWidth(p.Cols)+6)/cellW)))
	}
	for c := 0; c < cols; c += every {
		out.ColTicks = append(out.ColTicks, tick{padLeft + (float64(c)+0.5)*cellW, h - 6, p.Cols[c]})
	}

	for r := range rows {
		for c := range cols {
			v := p.at(r, c)
			cell := heatCell{
				X: padLeft + float64(c)*cellW + heatGap/2, Y: heatPadTop + float64(r)*cellH + heatGap/2,
				W: max(1, cellW-heatGap), H: max(1, cellH-heatGap),
				Fill: "var(--chart-grid)", Opacity: 1, Value: v, Slot: r*cols + c,
			}
			if v > 0 {
				cell.Fill = color
				cell.Opacity = heatMinAlpha + (1-heatMinAlpha)*v/peak
			}
			title := p.Rows[r] + " " + p.Cols[c]
			if c < len(p.ColTitles) {
				title = p.Rows[r] + " " + p.ColTitles[c]
			}
			cell.Tip = tooltip{Title: title, Rows: []tipRow{{color, unit, format(v)}}}
			centre := cell.X + cell.W/2
			off := min(cell.W/2, 24) + 8
			if centre > w/2 {
				cell.Tip.Side, cell.Tip.Offset = "right", (w-centre+off)/w
			} else {
				cell.Tip.Side, cell.Tip.Offset = "left", (centre+off)/w
			}
			cell.Tip.Top = heatPadTop / h
			out.Cells = append(out.Cells, cell)
		}
	}
	return out
}

func (p HeatmapProps) at(r, c int) float64 {
	if r >= len(p.Values) {
		return 0
	}
	return value(p.Values[r], c)
}
