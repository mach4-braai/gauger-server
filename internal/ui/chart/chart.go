// Package chart draws the dashboard's charts as SVG on the server. The
// geometry is a port of omp's stats charts (packages/stats/src/client/charts
// in can1357/oh-my-pi, MIT). Charts use a fixed viewBox and scale to their
// container's width; hover shows a tooltip rendered per slot on the server.
package chart

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Kind is how a series is drawn.
type Kind string

const (
	Bars Kind = "bars"
	Area Kind = "area"
	Line Kind = "line"
)

// Series is one plotted series. Values align index for index with the
// chart's slots; NaN leaves a gap in a line and an empty slot in bars.
type Series struct {
	Key    string
	Label  string
	Color  string
	Values []float64
	// Kind overrides the chart's Kind for this series.
	Kind Kind
	// Right plots against the right axis. Right-axis series never stack.
	Right bool
	// Hatch fills bars and areas with diagonal hatching.
	Hatch bool
	// Dashed strokes a line dashed.
	Dashed bool
	// NoTooltip leaves the series out of the tooltip.
	NoTooltip bool
}

// Reference is a horizontal guide across the plot.
type Reference struct {
	Value float64
	Label string
	Right bool
}

// Props describes a categorical chart. len(Ticks) is the slot count.
type Props struct {
	// ID names the chart's element, its hover signal and its legend
	// toggles in the URL. Use letters and digits, unique on the page.
	ID string
	// Ticks labels each slot on the x axis.
	Ticks []string
	// Titles heads each slot's tooltip; nil uses Ticks.
	Titles []string
	Series []Series
	// Kind is the mark for series without their own. Default Bars.
	Kind Kind
	// Unstacked draws left-axis bars side by side and areas overlapping.
	Unstacked bool
	// Width and Height size the viewBox. Default 760 by 220.
	Width, Height float64
	// Format labels the left axis and tooltip values. Default Compact.
	Format func(float64) string
	// FormatRight labels the right axis. Default Format.
	FormatRight func(float64) string
	// FormatTooltip formats left-axis tooltip values. Default Format.
	FormatTooltip func(float64) string
	// YMax and YMaxRight fix an axis maximum; zero rounds up from the data.
	YMax, YMaxRight float64
	// Hidden holds the keys of series to leave out, as Hidden reads them.
	Hidden     map[string]bool
	References []Reference
	// NoTotal drops the tooltip's total row, shown by default when two or
	// more left-axis series stack.
	NoTotal bool
	// Empty is shown when every visible value is zero.
	Empty string
	// Select links each slot; clicking a slot follows its link in place.
	// Empty entries are not clickable.
	Select []string
}

const (
	padTop         = 10.0
	padBottom      = 24.0
	minTickSpacing = 68.0
	yTicks         = 4
)

// Scale is an axis from zero to Max with evenly spaced Ticks.
type Scale struct {
	Max   float64
	Ticks []float64
}

// NiceScale rounds max up to 1, 2, 2.5 or 5 times a power of ten and returns
// ticks from zero in steps of that size.
func NiceScale(max float64) Scale {
	if !(max > 0) || math.IsInf(max, 0) {
		return Scale{Max: 1, Ticks: []float64{0}}
	}
	rough := max / yTicks
	magnitude := math.Pow(10, math.Floor(math.Log10(rough)))
	step := 10 * magnitude
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*magnitude >= rough {
			step = m * magnitude
			break
		}
	}
	top := round12(math.Ceil(max/step) * step)
	var ticks []float64
	for t := 0.0; t <= top+step/2; t += step {
		ticks = append(ticks, round12(t))
	}
	return Scale{Max: top, Ticks: ticks}
}

// round12 drops float noise past 12 significant digits, so 0.30000000000000004
// reads as 0.3.
func round12(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 12, 64), 64)
	return r
}

// Stack returns each stacked series' base per slot: the sum of the series
// before it. NaN counts as zero.
func Stack(series []Series, slots int) [][]float64 {
	tops := make([]float64, slots)
	bases := make([][]float64, len(series))
	for k, s := range series {
		bases[k] = slices.Clone(tops)
		for i := range slots {
			tops[i] += value(s.Values, i)
		}
	}
	return bases
}

func value(values []float64, i int) float64 {
	if i >= len(values) || math.IsNaN(values[i]) {
		return 0
	}
	return values[i]
}

type plot struct {
	ID         string
	Hover      string
	W, H       float64
	Patterns   []pattern
	Grid       []hline
	LeftTicks  []tick
	RightTicks []tick
	XTicks     []tick
	Areas      []area
	Slots      []slot
	Lines      []line
	Dots       []dot
	Refs       []reference
	Baseline   hline
	PlotX      float64
	PlotY      float64
	PlotW      float64
	PlotH      float64
	Empty      bool
	EmptyLabel string
}

type pattern struct{ ID, Color string }

type hline struct{ X1, X2, Y float64 }

type tick struct {
	X, Y  float64
	Label string
}

type area struct {
	Fill    string
	Opacity float64
	D, Line string
	Color   string
}

type slot struct {
	X, W float64
	Bars []bar
	Tip  tooltip
	Href string
}

type bar struct {
	X, Y, W, H float64
	Fill       string
}

type tooltip struct {
	Title string
	Rows  []tipRow
	Total string
	// Side and Offset place the tooltip: Side is "left" or "right" and
	// Offset the distance from that edge as a share of the width.
	Side   string
	Offset float64
	Top    float64
}

type tipRow struct{ Color, Label, Value string }

type line struct {
	D, Color string
	Dashed   bool
}

type dot struct {
	X, Y  float64
	Color string
}

type reference struct {
	Line   hline
	Label  string
	LabelX float64
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9]`)

// plan lays out the chart. It follows omp's Chart.tsx: stacked left series
// share per-slot bases, grouped bars split a slot, and the axes take the
// width their labels need.
func (p Props) plan() plot {
	w, h := p.Width, p.Height
	if w == 0 {
		w = 760
	}
	if h == 0 {
		h = 220
	}
	kind := p.Kind
	if kind == "" {
		kind = Bars
	}
	format := p.Format
	if format == nil {
		format = Compact
	}
	formatRight := p.FormatRight
	if formatRight == nil {
		formatRight = format
	}
	formatTooltip := p.FormatTooltip
	if formatTooltip == nil {
		formatTooltip = format
	}
	slots := len(p.Ticks)
	kindOf := func(s Series) Kind {
		if s.Kind != "" {
			return s.Kind
		}
		return kind
	}
	stackable := func(s Series) bool { return !p.Unstacked && kindOf(s) != Line && !s.Right }

	var visible, left, right, stacked []Series
	for _, s := range p.Series {
		if p.Hidden[s.Key] {
			continue
		}
		visible = append(visible, s)
		switch {
		case s.Right:
			right = append(right, s)
		case stackable(s):
			stacked = append(stacked, s)
			left = append(left, s)
		default:
			left = append(left, s)
		}
	}
	bases := map[string][]float64{}
	for k, b := range Stack(stacked, slots) {
		bases[stacked[k].Key] = b
	}
	var leftMax, rightMax float64
	for i := range slots {
		top := 0.0
		for _, s := range stacked {
			top += value(s.Values, i)
		}
		leftMax = max(leftMax, top)
		for _, s := range left {
			if !stackable(s) {
				leftMax = max(leftMax, value(s.Values, i))
			}
		}
		for _, s := range right {
			rightMax = max(rightMax, value(s.Values, i))
		}
	}
	for _, r := range p.References {
		if r.Right {
			rightMax = max(rightMax, r.Value)
		} else {
			leftMax = max(leftMax, r.Value)
		}
	}
	leftScale := NiceScale(cmpOr(p.YMax, leftMax))
	var rightScale *Scale
	if len(right) > 0 {
		s := NiceScale(cmpOr(p.YMaxRight, rightMax))
		rightScale = &s
	}
	empty := true
	for _, s := range visible {
		for _, v := range s.Values {
			if v != 0 && !math.IsNaN(v) {
				empty = false
			}
		}
	}

	leftLabels := make([]string, len(leftScale.Ticks))
	for i, t := range leftScale.Ticks {
		leftLabels[i] = format(t)
	}
	padLeft := max(28, labelWidth(leftLabels)+12)
	padRight := 8.0
	var rightLabels []string
	if rightScale != nil {
		for _, t := range rightScale.Ticks {
			rightLabels = append(rightLabels, formatRight(t))
		}
		padRight = max(28, labelWidth(rightLabels)+12)
	}
	plotW := max(0, w-padLeft-padRight)
	plotH := h - padTop - padBottom
	slotW := 0.0
	if slots > 0 {
		slotW = plotW / float64(slots)
	}
	yLeft := func(v float64) float64 { return padTop + plotH - v/leftScale.Max*plotH }
	yRight := func(v float64) float64 {
		if rightScale == nil {
			return padTop + plotH
		}
		return padTop + plotH - v/rightScale.Max*plotH
	}
	yOf := func(s Series) func(float64) float64 {
		if s.Right {
			return yRight
		}
		return yLeft
	}
	cx := func(i int) float64 { return padLeft + (float64(i)+0.5)*slotW }

	id := nonIdent.ReplaceAllString(p.ID, "_")
	out := plot{
		ID: "chart-" + id, Hover: "$_chart." + id, W: w, H: h,
		PlotX: padLeft, PlotY: padTop, PlotW: plotW, PlotH: plotH,
		Baseline: hline{padLeft, padLeft + plotW, yLeft(0)},
		Empty:    empty, EmptyLabel: cmpOr(p.Empty, "No data in this range"),
	}
	fill := func(s Series) string {
		if s.Hatch {
			return "url(#" + out.ID + "-" + nonIdent.ReplaceAllString(s.Key, "_") + ")"
		}
		return s.Color
	}
	for _, s := range visible {
		if s.Hatch {
			out.Patterns = append(out.Patterns, pattern{out.ID + "-" + nonIdent.ReplaceAllString(s.Key, "_"), s.Color})
		}
	}
	for i, t := range leftScale.Ticks {
		out.Grid = append(out.Grid, hline{padLeft, padLeft + plotW, yLeft(t)})
		out.LeftTicks = append(out.LeftTicks, tick{padLeft - 8, yLeft(t), leftLabels[i]})
	}
	if rightScale != nil {
		for i, t := range rightScale.Ticks {
			out.RightTicks = append(out.RightTicks, tick{padLeft + plotW + 8, yRight(t), rightLabels[i]})
		}
	}
	tickEvery := 1
	if slots > 0 {
		tickEvery = max(1, int(math.Ceil(float64(slots)/max(1, math.Floor(plotW/minTickSpacing)))))
	}
	for i := 0; i < slots; i += tickEvery {
		out.XTicks = append(out.XTicks, tick{cx(i), h - 6, p.Ticks[i]})
	}

	for _, s := range visible {
		if kindOf(s) != Area {
			continue
		}
		y, base := yOf(s), bases[s.Key]
		top := func(i int) float64 { return value(base, i) + value(s.Values, i) }
		d := pathThrough(slots, cx, func(i int) (float64, bool) { return y(top(i)), true })
		var floor strings.Builder
		for i := slots - 1; i >= 0; i-- {
			fmt.Fprintf(&floor, "L%.1f,%.1f", cx(i), y(value(base, i)))
		}
		opacity := 0.2
		if s.Hatch {
			opacity = 1
		}
		out.Areas = append(out.Areas, area{Fill: fill(s), Opacity: opacity, D: d + floor.String() + "Z", Line: d, Color: s.Color})
	}

	var barSeries, grouped []Series
	for _, s := range visible {
		if kindOf(s) == Bars {
			barSeries = append(barSeries, s)
			if !stackable(s) {
				grouped = append(grouped, s)
			}
		}
	}
	barW := max(1, min(56, slotW*0.9))
	if slotW > 6 {
		barW = max(1, min(56, slotW*0.72))
	}
	var rows []Series
	for _, s := range visible {
		if !s.NoTooltip {
			rows = append(rows, s)
		}
	}
	withTotal := !p.NoTotal && !p.Unstacked && len(left) > 1
	for i := range slots {
		sl := slot{X: padLeft + float64(i)*slotW, W: slotW}
		if i < len(p.Select) {
			sl.Href = p.Select[i]
		}
		for _, s := range barSeries {
			v := value(s.Values, i)
			if v == 0 {
				continue
			}
			y := yOf(s)
			group := slices.IndexFunc(grouped, func(g Series) bool { return g.Key == s.Key })
			subW, b, x := barW, value(bases[s.Key], i), cx(i)-barW/2
			if group >= 0 {
				subW = barW / float64(len(grouped))
				b = 0
				x += float64(group) * subW
			}
			y0, y1 := y(b), y(b+v)
			gap := 0.0
			if group >= 0 && len(grouped) > 1 {
				gap = 1
			}
			minH := 0.0
			if v > 0 {
				minH = 1
			}
			sl.Bars = append(sl.Bars, bar{X: x, Y: min(y0, y1), W: max(1, subW-gap), H: max(minH, math.Abs(y0-y1)), Fill: fill(s)})
		}
		sl.Tip = p.tooltip(i, rows, len(visible), withTotal, format, formatRight, formatTooltip)
		off := min(slotW/2, 24) + 8
		if cx(i) > w/2 {
			sl.Tip.Side, sl.Tip.Offset = "right", (w-cx(i)+off)/w
		} else {
			sl.Tip.Side, sl.Tip.Offset = "left", (cx(i)+off)/w
		}
		sl.Tip.Top = padTop / h
		out.Slots = append(out.Slots, sl)
	}

	for _, s := range visible {
		if kindOf(s) != Line {
			continue
		}
		y := yOf(s)
		at := func(i int) (float64, bool) {
			if i < 0 || i >= len(s.Values) || math.IsNaN(s.Values[i]) {
				return 0, false
			}
			return y(s.Values[i]), true
		}
		out.Lines = append(out.Lines, line{D: pathThrough(slots, cx, at), Color: s.Color, Dashed: s.Dashed})
		for i := range slots {
			yi, ok := at(i)
			_, prev := at(i - 1)
			_, next := at(i + 1)
			if ok && !(i > 0 && prev) && !(i < slots-1 && next) {
				out.Dots = append(out.Dots, dot{cx(i), yi, s.Color})
			}
		}
	}
	for _, r := range p.References {
		y := yLeft(r.Value)
		if r.Right {
			y = yRight(r.Value)
		}
		out.Refs = append(out.Refs, reference{Line: hline{padLeft, padLeft + plotW, y}, Label: r.Label, LabelX: padLeft + plotW - 4})
	}
	return out
}

// tooltip lists slot i's values, largest first when several series share
// the chart. With several series, zero rows are left out.
func (p Props) tooltip(i int, series []Series, visible int, withTotal bool, format, formatRight, formatTooltip func(float64) string) tooltip {
	title := p.Ticks[i]
	if i < len(p.Titles) {
		title = p.Titles[i]
	}
	type row struct {
		s Series
		v float64
	}
	var rows []row
	for _, s := range series {
		if i >= len(s.Values) || math.IsNaN(s.Values[i]) || (s.Values[i] == 0 && visible > 1) {
			continue
		}
		rows = append(rows, row{s, s.Values[i]})
	}
	if visible > 1 {
		slices.SortStableFunc(rows, func(a, b row) int {
			switch {
			case a.v > b.v:
				return -1
			case a.v < b.v:
				return 1
			}
			return 0
		})
	}
	t := tooltip{Title: title}
	total := 0.0
	for _, r := range rows {
		f := formatTooltip
		if r.s.Right {
			f = formatRight
		} else {
			total += r.v
		}
		t.Rows = append(t.Rows, tipRow{r.s.Color, r.s.Label, f(r.v)})
	}
	if withTotal && len(rows) > 1 {
		t.Total = formatTooltip(total)
	}
	return t
}

func cmpOr[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}

func labelWidth(labels []string) float64 {
	w := 0.0
	for _, l := range labels {
		w = max(w, float64(len([]rune(l)))*6.3)
	}
	return w
}

// pathThrough draws straight segments through slot centres; a missing
// point breaks the line.
func pathThrough(slots int, x func(int) float64, y func(int) (float64, bool)) string {
	var b strings.Builder
	pen := false
	for i := range slots {
		yi, ok := y(i)
		if !ok {
			pen = false
			continue
		}
		cmd := "M"
		if pen {
			cmd = "L"
		}
		fmt.Fprintf(&b, "%s%.1f,%.1f", cmd, x(i), yi)
		pen = true
	}
	return b.String()
}

// HideParam is the query parameter that lists hidden series, each as
// chart ID, a colon and the series key.
const HideParam = "hide"

// Form is the id of the dashboard's filter form. Inputs that carry page
// state join it with form="filters", so a filter change keeps them.
const Form = "filters"

// Hidden reads the series of chart id that the query hides.
func Hidden(q url.Values, id string) map[string]bool {
	hidden := map[string]bool{}
	for _, v := range q[HideParam] {
		if key, ok := strings.CutPrefix(v, id+":"); ok {
			hidden[key] = true
		}
	}
	return hidden
}

// toggleURL is u with series key of chart id shown if hidden and hidden if
// shown.
func toggleURL(u *url.URL, id, key string) string {
	q := u.Query()
	v := id + ":" + key
	hide := slices.DeleteFunc(slices.Clone(q[HideParam]), func(h string) bool { return h == v })
	if len(hide) == len(q[HideParam]) {
		hide = append(hide, v)
	}
	if len(hide) == 0 {
		q.Del(HideParam)
	} else {
		q[HideParam] = hide
	}
	if len(q) == 0 {
		return u.Path
	}
	return u.Path + "?" + q.Encode()
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

func pct(v float64) string { return strconv.FormatFloat(v*100, 'f', 2, 64) + "%" }
