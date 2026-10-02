package ui

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
)

type filterForm struct {
	Repo  string
	Days  int
	Repos []string
}

func (u *UI) filter(r *http.Request, defaultDays int) (store.Filter, filterForm, error) {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days <= 0 || days > 3650 {
		days = defaultDays
	}
	f := filterForm{Repo: r.URL.Query().Get("repo"), Days: days}
	f.Repos, err = u.Store.Repositories(r.Context())
	return store.Filter{Repository: f.Repo, Since: time.Now().AddDate(0, 0, -days)}, f, err
}

func floatParam(r *http.Request, key string, def float64) float64 {
	v, err := strconv.ParseFloat(r.URL.Query().Get(key), 64)
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func (u *UI) jobs(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 7)
	if err != nil {
		u.fail(w, err)
		return
	}
	jobs, err := u.Store.RecentJobs(r.Context(), f, 100)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "jobs", map[string]any{"Filter": form, "Rows": jobs})
}

func (u *UI) job(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := u.Store.Job(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	if d == nil {
		http.NotFound(w, r)
		return
	}
	series, err := u.Store.JobSeries(r.Context(), id)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "job", map[string]any{"Job": d, "Chart": chart(d, series)})
}

func (u *UI) steps(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 30)
	if err != nil {
		u.fail(w, err)
		return
	}
	rows, err := u.Store.SlowSteps(r.Context(), f)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "steps", map[string]any{"Filter": form, "Rows": rows})
}

func (u *UI) regressions(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 14)
	if err != nil {
		u.fail(w, err)
		return
	}
	ratio := floatParam(r, "ratio", 1.25)
	minSecs := floatParam(r, "min", 5)
	rows, err := u.Store.Regressions(r.Context(), f, ratio, minSecs)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "regressions", map[string]any{
		"Filter": form, "Rows": rows, "Ratio": ratio, "Min": minSecs, "BaselineDays": store.BaselineDays,
	})
}

func (u *UI) sizing(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 30)
	if err != nil {
		u.fail(w, err)
		return
	}
	rows, err := u.Store.Sizing(r.Context(), f)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "sizing", map[string]any{"Filter": form, "Rows": rows})
}

const maxDailyBuckets = 60

type dailyCell struct {
	Median float64
	Runs   int64
}

type dailyRow struct {
	Repository string
	Workflow   string
	Job        string
	Cells      []dailyCell
}

// dailyBucket truncates a time to its bucket's start, steps to the next
// bucket and formats a bucket's label.
type dailyBucket struct {
	trunc func(time.Time) time.Time
	next  func(time.Time) time.Time
	label func(time.Time) string
}

func startOfWeek(t time.Time) time.Time {
	t = t.Truncate(24 * time.Hour)
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t.AddDate(0, 0, 1-wd)
}

var dailyBuckets = map[string]dailyBucket{
	"day": {
		trunc: func(t time.Time) time.Time { return t.Truncate(24 * time.Hour) },
		next:  func(t time.Time) time.Time { return t.AddDate(0, 0, 1) },
		label: func(t time.Time) string { return t.Format("01-02") },
	},
	"week": {
		trunc: startOfWeek,
		next:  func(t time.Time) time.Time { return t.AddDate(0, 0, 7) },
		label: func(t time.Time) string { _, w := t.ISOWeek(); return fmt.Sprintf("W%02d", w) },
	},
	"month": {
		trunc: func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC) },
		next:  func(t time.Time) time.Time { return t.AddDate(0, 1, 0) },
		label: func(t time.Time) string { return t.Format("2006-01") },
	},
}

// bucketName reads "bucket" from the query, falling back to "day" when
// it is absent or unknown.
func bucketName(r *http.Request) string {
	name := r.URL.Query().Get("bucket")
	if _, ok := dailyBuckets[name]; ok {
		return name
	}
	return "day"
}

// bucketStarts lists each bucket's start from days ago through the
// current bucket, trimmed to the most recent maxDailyBuckets.
func bucketStarts(bucketParam string, days int, now time.Time) []time.Time {
	b := dailyBuckets[bucketParam]
	first := b.trunc(now.AddDate(0, 0, 1-days))
	var starts []time.Time
	for t := first; !t.After(b.trunc(now)); t = b.next(t) {
		starts = append(starts, t)
	}
	if len(starts) > maxDailyBuckets {
		starts = starts[len(starts)-maxDailyBuckets:]
	}
	return starts
}

func (u *UI) daily(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 14)
	if err != nil {
		u.fail(w, err)
		return
	}
	bucketParam := bucketName(r)
	starts := bucketStarts(bucketParam, form.Days, time.Now().UTC())
	f.Since = starts[0]
	durations, err := u.Store.DailyDurations(r.Context(), f, bucketParam)
	if err != nil {
		u.fail(w, err)
		return
	}
	index := make(map[int64]int, len(starts))
	for i, t := range starts {
		index[t.Unix()] = i
	}
	var workflows, jobs []dailyRow
	for _, d := range durations {
		rows := &jobs
		if d.Job == "" {
			rows = &workflows
		}
		n := len(*rows)
		if n == 0 || (*rows)[n-1].Repository != d.Repository || (*rows)[n-1].Workflow != d.Workflow || (*rows)[n-1].Job != d.Job {
			*rows = append(*rows, dailyRow{Repository: d.Repository, Workflow: d.Workflow, Job: d.Job, Cells: make([]dailyCell, len(starts))})
			n++
		}
		if i, ok := index[d.Bucket.UTC().Unix()]; ok {
			(*rows)[n-1].Cells[i] = dailyCell{Median: d.Median, Runs: d.Runs}
		}
	}
	labels := make([]string, len(starts))
	for i, t := range starts {
		labels[i] = dailyBuckets[bucketParam].label(t)
	}
	u.render(w, "daily", map[string]any{
		"Filter": form, "Labels": labels, "Bucket": bucketParam, "Workflows": workflows, "Jobs": jobs,
		"Chart": dailyChart(labels, workflows, workflowLabel(form.Repo == "")),
	})
}

func workflowLabel(allRepos bool) func(dailyRow) string {
	return func(row dailyRow) string {
		if allRepos {
			return row.Repository + " · " + row.Workflow
		}
		return row.Workflow
	}
}

const (
	dailyChartW, dailyChartH   = 960.0, 300.0
	dailyPadL, dailyPadR       = 44.0, 10.0
	dailyPadT, dailyPadB       = 10.0, 54.0
	dailyGroupPad, dailyBarGap = 4.0, 2.0
)

var dailyPalette = []string{
	"#0969da", "#8250df", "#1a7f37", "#bf3989", "#9a6700",
	"#cf222e", "#116329", "#57606a", "#6639ba", "#0550ae",
}

// dailySeriesColor picks a palette colour by the series' position among
// the chart's ordered series, wrapping past the palette's length.
func dailySeriesColor(i int) string {
	return dailyPalette[i%len(dailyPalette)]
}

// niceStep picks a round axis step (1/2/5 × a power of ten) that divides
// max into roughly count ticks.
func niceStep(max float64, count int) float64 {
	if max <= 0 {
		return 1
	}
	raw := max / float64(count)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	switch norm := raw / mag; {
	case norm <= 1:
		return mag
	case norm <= 2:
		return 2 * mag
	case norm <= 5:
		return 5 * mag
	default:
		return 10 * mag
	}
}

func fmtTick(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func plural(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// dailyChart draws a grouped SVG bar chart: one group per label, one bar
// per row in each group, scaled to the row's median duration. seriesLabel
// names a row for its legend entry, hover title and colour.
func dailyChart(labels []string, rows []dailyRow, seriesLabel func(dailyRow) string) template.HTML {
	if len(rows) == 0 || len(labels) == 0 {
		return ""
	}
	var maxVal float64
	for _, row := range rows {
		for _, c := range row.Cells {
			if c.Runs > 0 && c.Median > maxVal {
				maxVal = c.Median
			}
		}
	}
	if maxVal <= 0 {
		return ""
	}
	unit, scale := "s", 1.0
	if maxVal >= 90 {
		unit, scale = "min", 60.0
	}
	step := niceStep(maxVal/scale, 4)
	axisMax := step * math.Ceil(maxVal/scale/step)

	usableW := dailyChartW - dailyPadL - dailyPadR
	usableH := dailyChartH - dailyPadT - dailyPadB
	groupW := usableW / float64(len(labels))
	barW := (groupW - 2*dailyGroupPad - float64(len(rows)-1)*dailyBarGap) / float64(len(rows))
	if barW < 1 {
		barW = 1
	}
	x := func(i int) float64 { return dailyPadL + float64(i)*groupW }
	y := func(v float64) float64 { return dailyPadT + usableH - v/(axisMax*scale)*usableH }

	seriesLabels := make([]string, len(rows))
	colors := make([]string, len(rows))
	for j, row := range rows {
		seriesLabels[j] = seriesLabel(row)
		colors[j] = dailySeriesColor(j)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" role="img">`, dailyChartW, dailyChartH)
	for v := 0.0; v <= axisMax+step*0.001; v += step {
		gy := y(v * scale)
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" stroke="#d0d7de"/><text x="2" y="%.1f" font-size="10" fill="#656d76">%s%s</text>`,
			dailyPadL, dailyChartW-dailyPadR, gy, gy, gy+3, fmtTick(v), unit)
	}
	for i, l := range labels {
		lx := x(i) + groupW/2
		ly := dailyChartH - dailyPadB + 12
		fmt.Fprintf(&b, `<text x="%.1f" y="%.0f" font-size="9" fill="#656d76" text-anchor="end" transform="rotate(-60 %.1f %.0f)">%s</text>`,
			lx, ly, lx, ly, template.HTMLEscapeString(l))
	}
	for i, l := range labels {
		for j, row := range rows {
			c := row.Cells[i]
			if c.Runs == 0 {
				continue
			}
			bx := x(i) + dailyGroupPad + float64(j)*(barW+dailyBarGap)
			bh := c.Median / (axisMax * scale) * usableH
			by := dailyPadT + usableH - bh
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"><title>%s · %s · %s · %d run%s</title></rect>`,
				bx, by, barW, bh, colors[j],
				template.HTMLEscapeString(seriesLabels[j]), template.HTMLEscapeString(l), fmtDuration(time.Duration(c.Median*float64(time.Second))),
				c.Runs, plural(c.Runs))
		}
	}
	b.WriteString(`</svg><div class="legend">`)
	for j := range rows {
		fmt.Fprintf(&b, `<span class="legend-item"><span class="swatch" style="background:%s"></span>%s</span>`, colors[j], template.HTMLEscapeString(seriesLabels[j]))
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

type spendRow struct {
	store.SpendGroup
	spend.Price
}

func (u *UI) spend(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 90)
	if err != nil {
		u.fail(w, err)
		return
	}
	groups, err := u.Store.SpendGroups(r.Context(), f)
	if err != nil {
		u.fail(w, err)
		return
	}
	var (
		rows    []spendRow
		total   float64
		unknown int64
	)
	for _, g := range groups {
		p := u.Rates.Price(g.Labels, g.Private, g.Minutes)
		rows = append(rows, spendRow{g, p})
		total += p.Cost
		if !p.Known {
			unknown += g.Minutes
		}
	}
	u.render(w, "spend", map[string]any{"Filter": form, "Rows": rows, "Total": total, "UnknownMinutes": unknown})
}

const (
	chartW, chartH = 960.0, 220.0
	padL, padR     = 40.0, 10.0
	padT, padB     = 10.0, 20.0
	maxPoints      = 1500
)

// chart draws the runner's CPU utilization and memory used as a share of
// MemTotal across the job, with a line at the start of each step.
func chart(d *store.JobDetail, samples []store.Sample) template.HTML {
	if len(samples) == 0 {
		return ""
	}
	start, end := samples[0].Time, samples[len(samples)-1].Time
	if d.StartedAt != nil && d.StartedAt.Before(start) {
		start = *d.StartedAt
	}
	if d.CompletedAt != nil && d.CompletedAt.After(end) {
		end = *d.CompletedAt
	}
	span := end.Sub(start).Seconds()
	if span <= 0 {
		return ""
	}
	x := func(t time.Time) float64 { return padL + t.Sub(start).Seconds()/span*(chartW-padL-padR) }
	y := func(frac float64) float64 {
		frac = max(0, min(1, frac))
		return padT + (1-frac)*(chartH-padT-padB)
	}

	var cpu, mem []store.Sample
	for _, s := range samples {
		switch s.Metric {
		case store.MetricCPUUtilization:
			cpu = append(cpu, s)
		case store.MetricMemoryUsage:
			if d.MemTotal != nil && *d.MemTotal > 0 {
				mem = append(mem, store.Sample{Time: s.Time, Value: s.Value / *d.MemTotal})
			}
		}
	}
	line := func(pts []store.Sample, color string) string {
		stride := max(1, len(pts)/maxPoints)
		var b strings.Builder
		for i := 0; i < len(pts); i += stride {
			fmt.Fprintf(&b, "%.1f,%.1f ", x(pts[i].Time), y(pts[i].Value))
		}
		return fmt.Sprintf(`<polyline fill="none" stroke="%s" stroke-width="1.5" points="%s"/>`, color, b.String())
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %.0f %.0f" width="100%%" role="img">`, chartW, chartH)
	for _, frac := range []float64{0, 0.5, 1} {
		fmt.Fprintf(&b, `<line x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f" stroke="#d0d7de"/><text x="2" y="%.1f" font-size="10" fill="#656d76">%.0f%%</text>`,
			padL, chartW-padR, y(frac), y(frac), y(frac)+3, frac*100)
	}
	for _, st := range d.Steps {
		if st.StartedAt == nil {
			continue
		}
		sx := x(*st.StartedAt)
		fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%.0f" y2="%.0f" stroke="#afb8c1" stroke-dasharray="2,2"><title>%s</title></line>`,
			sx, sx, padT, chartH-padB, template.HTMLEscapeString(st.Name))
	}
	b.WriteString(line(cpu, "#0969da"))
	if len(mem) > 0 {
		b.WriteString(line(mem, "#8250df"))
	}
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" font-size="10" fill="#0969da">CPU utilization</text>`, padL, chartH-4)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" font-size="10" fill="#8250df">memory used / MemTotal</text>`, padL+100, chartH-4)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
