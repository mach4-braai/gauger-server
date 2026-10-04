package chart

import (
	"fmt"
	"strconv"
	"time"

	"github.com/a-h/templ"
)

// Detail is one labelled value in a span's tooltip.
type Detail struct{ Label, Value string }

// Span is one row of a Timeline: a label and a bar from Start to End. A
// span with a zero Start has no bar but keeps its row.
type Span struct {
	Label      string
	Start, End time.Time
	// Color fills the bar. Default is the primary chart colour.
	Color string
	// Href is followed in place when the bar or label is clicked.
	Href string
	// Title heads the tooltip. Default is Label.
	Title   string
	Details []Detail
}

// TimelineProps lays spans out against one time axis from Start to End.
type TimelineProps struct {
	// ID names the timeline's element and its hover signal. Use letters
	// and digits, unique on the page.
	ID         string
	Start, End time.Time
	Spans      []Span
	// Empty is shown when there are no spans.
	Empty string
}

type timeline struct {
	ID string
	// Signal is the key under _chart that holds the hovered row.
	Signal string
	Hover  string
	Ticks  []timelineTick
	Rows   []timelineRow
	Empty  string
}

type timelineTick struct {
	Left  float64
	Label string
}

type timelineRow struct {
	Label, Href string
	// Left and Width place the bar as percentages of the lane.
	Left, Width float64
	Placed      bool
	Color       string
	Tip         spanTip
}

type spanTip struct {
	Title   string
	Details []Detail
	// Side is "left" or "right", the lane edge the tooltip sticks to, and
	// Offset is its distance from that edge as a percentage of the lane.
	Side   string
	Offset float64
}

// axisSteps are the tick spacings a timeline picks from.
var axisSteps = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour,
}

const maxAxisTicks = 8

// axisStep is the smallest spacing that puts at most maxAxisTicks ticks
// on a span.
func axisStep(span time.Duration) time.Duration {
	for _, s := range axisSteps {
		if span/s <= maxAxisTicks {
			return s
		}
	}
	return axisSteps[len(axisSteps)-1]
}

// Offset formats an elapsed time as 0:05, 3:20 or 1:02:30.
func Offset(d time.Duration) string {
	n := int64(d.Round(time.Second) / time.Second)
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	if n >= 3600 {
		return fmt.Sprintf("%s%d:%02d:%02d", sign, n/3600, n%3600/60, n%60)
	}
	return fmt.Sprintf("%s%d:%02d", sign, n/60, n%60)
}

// fraction is where t falls between start and end, clamped to [0, 1].
func fraction(start, end, t time.Time) float64 {
	span := end.Sub(start)
	if span <= 0 {
		return 0
	}
	return max(0, min(1, float64(t.Sub(start))/float64(span)))
}

func (p TimelineProps) plan() timeline {
	end := p.End
	if !end.After(p.Start) {
		end = p.Start.Add(time.Second)
	}
	out := timeline{ID: "timeline-" + nonIdent.ReplaceAllString(p.ID, "_"), Empty: cmpOr(p.Empty, "Nothing to show")}
	out.Signal = nonIdent.ReplaceAllString(p.ID, "_")
	out.Hover = "$_chart." + out.Signal
	step := axisStep(end.Sub(p.Start))
	for at := time.Duration(0); at <= end.Sub(p.Start); at += step {
		out.Ticks = append(out.Ticks, timelineTick{
			Left:  float64(at) / float64(end.Sub(p.Start)) * 100,
			Label: Offset(at),
		})
	}
	for _, s := range p.Spans {
		row := timelineRow{Label: s.Label, Href: s.Href, Color: cmpOr(s.Color, "var(--chart-primary)")}
		row.Tip = spanTip{Title: cmpOr(s.Title, s.Label), Details: s.Details, Side: "left"}
		if !s.Start.IsZero() {
			from, to := fraction(p.Start, end, s.Start), fraction(p.Start, end, s.End)
			row.Placed = true
			row.Left, row.Width = from*100, (to-from)*100
			if (from+to)/2 > 0.5 {
				row.Tip.Side, row.Tip.Offset = "right", (1-to)*100
			} else {
				row.Tip.Offset = from * 100
			}
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}

func (r timelineRow) barStyle() templ.SafeCSS {
	return templ.SafeCSS(fmt.Sprintf("left: %.3f%%; width: %.3f%%; background: %s", r.Left, r.Width, r.Color))
}

func (r spanTip) style() templ.SafeCSS {
	return templ.SafeCSS(fmt.Sprintf("display: none; %s: %.3f%%", r.Side, r.Offset))
}

func percent(v float64) templ.SafeCSS {
	return templ.SafeCSS("left: " + strconv.FormatFloat(v, 'f', 3, 64) + "%")
}

// TraceProps is a Chart whose slots are equal slices of the time from
// Start to End, so it shares a Timeline's axis. Leave Ticks and Titles
// unset; Trace fills them. Series values align with the slices.
type TraceProps struct {
	Props
	Start, End time.Time
	Slots      int
	// Marks draw dashed vertical lines, such as where each step starts.
	Marks []time.Time
}

func (p TraceProps) chart() Props {
	c := p.Props
	c.Ticks = make([]string, p.Slots)
	c.Titles = make([]string, p.Slots)
	width := p.End.Sub(p.Start) / time.Duration(max(1, p.Slots))
	for i := range p.Slots {
		at := p.Start.Add(time.Duration(i) * width)
		c.Ticks[i] = Offset(at.Sub(p.Start))
		c.Titles[i] = c.Ticks[i] + " · " + at.UTC().Format("15:04:05") + " UTC"
	}
	return c
}

// marks places each mark inside the plot area of the planned chart as an
// overlay style, in percentages of the chart's size.
func (p TraceProps) marks(c plot) []templ.SafeCSS {
	var out []templ.SafeCSS
	for _, m := range p.Marks {
		if m.Before(p.Start) || m.After(p.End) {
			continue
		}
		x := c.PlotX + fraction(p.Start, p.End, m)*c.PlotW
		out = append(out, templ.SafeCSS(fmt.Sprintf("left: %.3f%%; top: %.3f%%; height: %.3f%%",
			x/c.W*100, c.PlotY/c.H*100, c.PlotH/c.H*100)))
	}
	return out
}
