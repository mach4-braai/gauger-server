package stats

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// jobMaxSlots caps the slices a resource chart divides the job into, so
// the server renders one tooltip per slice for a bounded number of slices.
const jobMaxSlots = 90

// jobDefaultInterval is the gap between samples when the job has too few
// samples to measure it.
const jobDefaultInterval = 10 * time.Second

type jobFact struct {
	ID, Key, Value string
}

type jobChart struct {
	Title, Description string
	Trace              chart.TraceProps
	Legend             []chart.LegendItem
}

type jobStepRow struct {
	Number                                              int
	Name, Href                                          string
	Result                                              string
	Started, Duration                                   string
	PeakMemory, OfMemTotal, PeakCPU, Saturated, Samples string
	Selected                                            bool
}

type jobDrawer struct {
	Number        int
	Name          string
	Result, Tone  string
	Facts         []jobFact
	LogURL, Close string
}

type jobView struct {
	Repository, Workflow, WorkflowURL, Name string
	RunURL, RunLabel                        string
	Branch, BranchURL, SHA, CommitURL       string
	Event, JobURL                           string
	Facts                                   []jobFact
	Status                                  string
	Timeline                                chart.TimelineProps
	Charts                                  []jobChart
	Steps                                   []jobStepRow
	Drawer                                  *jobDrawer
}

func (s *Server) job(r *http.Request, _ store.Filter) (templ.Component, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, errNotFound
	}
	ctx := r.Context()
	d, err := s.Store.Job(ctx, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, errNotFound
	}
	all, err := s.Store.GaugerSeries(ctx, id)
	if err != nil {
		return nil, err
	}
	series := all[id]

	until := s.now().UTC()
	switch {
	case d.CompletedAt != nil:
		until = *d.CompletedAt
	case d.Status == "completed" && d.StartedAt != nil:
		until = *d.StartedAt
	case d.Status == "completed" && d.CreatedAt != nil:
		until = *d.CreatedAt
	}
	spans := store.StepSpans(d.Steps, until)
	ax := newJobAxis(d, spans, series, until)

	v := jobView{
		Repository: d.Repository, Workflow: d.Workflow, Name: cmpOr(d.Name, "job "+strconv.FormatInt(d.ID, 10)),
		WorkflowURL: github.WorkflowURL(s.GitHubURL, d.Repository, d.Path),
		RunURL:      github.RunURL(s.GitHubURL, d.Repository, d.RunID, d.RunAttempt),
		RunLabel:    fmt.Sprintf("Run %d attempt %d", d.RunID, d.RunAttempt),
		Branch:      d.Branch, Event: d.Event, JobURL: d.HTMLURL,
		SHA:    jobShortSHA(d.HeadSHA),
		Status: jobStatus(d),
		Facts:  jobFacts(d),
		Steps:  jobStepRows(r.URL, d),
	}
	if d.Branch != "" {
		v.BranchURL = github.BranchURL(s.GitHubURL, d.Repository, d.Branch)
	}
	if d.HeadSHA != "" {
		v.CommitURL = github.CommitURL(s.GitHubURL, d.Repository, d.HeadSHA)
	}
	v.Timeline = jobTimeline(r.URL, d, spans, ax)
	v.Charts = jobCharts(r.URL, d, series, ax)
	if n, err := strconv.Atoi(r.URL.Query().Get("step")); err == nil {
		for i, st := range d.Steps {
			if st.Number == n {
				v.Steps[i].Selected = true
				v.Drawer = jobStepDrawer(r.URL, d, st, v.Steps[i])
			}
		}
	}
	return jobPage(v), nil
}

// jobStatus says where the job's gauger data came from.
func jobStatus(d *store.JobDetail) string {
	samples := strconv.FormatInt(d.Samples, 10)
	switch {
	case d.RunnerSeen != nil:
		s := "gauger reported " + samples + " samples"
		if d.RunnerDone != nil {
			s += " and finished"
		}
		if d.FromArtifact {
			s += ", some from the fallback artifact"
		}
		return s + "."
	case d.FromArtifact:
		return "gauger never reached the server. Its fallback artifact held " + samples + " samples."
	}
	return "No gauger data for this job."
}

func jobTone(conclusion string) string {
	switch conclusion {
	case "success":
		return "ok"
	case "failure", "timed_out":
		return "bad"
	case "cancelled", "skipped", "":
		return ""
	}
	return "warn"
}

func jobColor(conclusion string) string {
	switch conclusion {
	case "success":
		return "var(--ok)"
	case "failure", "timed_out":
		return "var(--bad)"
	case "cancelled":
		return "var(--ink-3)"
	case "skipped":
		return "var(--ink-4)"
	case "":
		return "var(--chart-primary)"
	}
	return "var(--warn)"
}

const jobNone = "–"

func jobOptional(v *float64, f func(float64) string) string {
	if v == nil {
		return jobNone
	}
	return f(*v)
}

func jobElapsed(from, to *time.Time) string {
	if from == nil || to == nil {
		return jobNone
	}
	return chart.Seconds(to.Sub(*from).Seconds())
}

func jobClock(t *time.Time) string {
	if t == nil {
		return jobNone
	}
	return t.UTC().Format("2006-01-02 15:04:05")
}

func jobFacts(d *store.JobDetail) []jobFact {
	runner := d.RunnerName
	if runner == "" {
		runner = "unknown"
	}
	facts := []jobFact{
		{"branch", "Branch", cmpOr(d.Branch, jobNone)},
		{"sha", "Commit", cmpOr(jobShortSHA(d.HeadSHA), jobNone)},
		{"event", "Event", cmpOr(d.Event, jobNone)},
		{"runner", "Runner", runner},
		{"labels", "Labels", cmpOr(strings.Join(d.Labels, ", "), jobNone)},
		{"result", "Result", cmpOr(d.Conclusion, d.Status)},
		{"attempt", "Attempt", strconv.Itoa(d.RunAttempt)},
		{"queue", "Queue time", jobElapsed(d.CreatedAt, d.StartedAt)},
		{"duration", "Duration", jobElapsed(d.StartedAt, d.CompletedAt)},
		{"memtotal", "MemTotal", jobOptional(d.MemTotal, chart.Bytes)},
		{"nproc", "nproc", jobOptional(d.CPUCount, func(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) })},
	}
	return facts
}

func jobShortSHA(sha string) string { return sha[:min(7, len(sha))] }

// jobAxis is the time range the timeline and the resource charts share:
// from the job's creation to its end, widened to whole chart slices.
type jobAxis struct {
	Start, End time.Time
	Slots      int
	Width      time.Duration
}

// newJobAxis spans the queue wait, every step and every sample. With
// samples, it cuts the span into slices that are a whole multiple of the
// sampling interval, with the first sample in the middle of a slice, so
// jitter in sample times never leaves a slice empty or doubled.
func newJobAxis(d *store.JobDetail, spans []store.StepSpan, series []store.GaugerSeries, until time.Time) jobAxis {
	start, end := until, until
	widen := func(t time.Time) {
		if t.Before(start) {
			start = t
		}
		if t.After(end) {
			end = t
		}
	}
	for _, t := range []*time.Time{d.CreatedAt, d.StartedAt, d.CompletedAt} {
		if t != nil {
			widen(*t)
		}
	}
	for _, sp := range spans {
		widen(sp.Start)
		widen(sp.End)
	}
	var first time.Time
	var longest []store.SeriesPoint
	for _, g := range series {
		if len(g.Points) > len(longest) {
			longest = g.Points
		}
		widen(g.Points[0].Time)
		widen(g.Points[len(g.Points)-1].Time)
		if first.IsZero() || g.Points[0].Time.Before(first) {
			first = g.Points[0].Time
		}
	}
	if !end.After(start) {
		end = start.Add(time.Second)
	}
	if first.IsZero() {
		return jobAxis{Start: start, End: end, Slots: 1, Width: end.Sub(start)}
	}
	interval := jobDefaultInterval
	gap := time.Duration(0)
	for i := 1; i < len(longest); i++ {
		if g := longest[i].Time.Sub(longest[i-1].Time); g > 0 && (gap == 0 || g < gap) {
			gap = g
		}
	}
	if gap > 0 {
		interval = gap
	}
	width := interval * time.Duration(max(1, jobCeilDiv(jobCeilDiv(int64(end.Sub(start)), int64(interval)), jobMaxSlots)))
	back := jobCeilDiv(int64(first.Sub(start)-width/2), int64(width))
	start = first.Add(-width/2 - time.Duration(back)*width)
	slots := int(max(1, jobCeilDiv(int64(end.Sub(start)), int64(width))))
	return jobAxis{Start: start, End: start.Add(time.Duration(slots) * width), Slots: slots, Width: width}
}

func jobCeilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func jobTimeline(u *url.URL, d *store.JobDetail, spans []store.StepSpan, ax jobAxis) chart.TimelineProps {
	p := chart.TimelineProps{
		ID: "timeline", Start: ax.Start, End: ax.End,
		Empty: "No step timings yet.",
	}
	at := func(t *time.Time) string {
		if t == nil {
			return jobNone
		}
		return chart.Offset(t.Sub(ax.Start)) + " · " + t.UTC().Format("15:04:05") + " UTC"
	}
	queued := d.CreatedAt != nil && (d.StartedAt != nil && d.StartedAt.After(*d.CreatedAt) || d.StartedAt == nil && d.Status != "completed")
	if queued {
		end := ax.End
		if d.StartedAt != nil {
			end = *d.StartedAt
		}
		p.Spans = append(p.Spans, chart.Span{
			Label: "Queued", Start: *d.CreatedAt, End: end, Color: "var(--line-4)",
			Details: []chart.Detail{
				{Label: "Created", Value: at(d.CreatedAt)},
				{Label: "Started", Value: at(d.StartedAt)},
				{Label: "Waited", Value: chart.Seconds(end.Sub(*d.CreatedAt).Seconds())},
			},
		})
	}
	for _, st := range d.Steps {
		span := chart.Span{
			Label: strconv.Itoa(st.Number) + ". " + st.Name, Title: st.Name,
			Color: jobColor(st.Conclusion), Href: jobStepHref(u, st.Number),
			Details: []chart.Detail{
				{Label: "Result", Value: cmpOr(st.Conclusion, st.Status)},
				{Label: "Started", Value: at(st.StartedAt)},
				{Label: "Finished", Value: at(st.CompletedAt)},
				{Label: "Duration", Value: jobElapsed(st.StartedAt, st.CompletedAt)},
			},
		}
		if i := slices.IndexFunc(spans, func(sp store.StepSpan) bool { return sp.Number == st.Number }); i >= 0 {
			span.Start, span.End = spans[i].Start, spans[i].End
		}
		p.Spans = append(p.Spans, span)
	}
	return p
}

// jobStepHref is the page's URL with the step's drawer open, or closed
// for step zero.
func jobStepHref(u *url.URL, number int) string {
	q := u.Query()
	if number == 0 {
		q.Del("step")
	} else {
		q.Set("step", strconv.Itoa(number))
	}
	if len(q) == 0 {
		return u.Path
	}
	return u.Path + "?" + q.Encode()
}

func jobStepRows(u *url.URL, d *store.JobDetail) []jobStepRow {
	rows := make([]jobStepRow, len(d.Steps))
	for i, st := range d.Steps {
		rows[i] = jobStepRow{
			Number: st.Number, Name: st.Name, Href: jobStepHref(u, st.Number),
			Result:     cmpOr(st.Conclusion, st.Status),
			Started:    jobClock(st.StartedAt),
			Duration:   jobElapsed(st.StartedAt, st.CompletedAt),
			PeakMemory: jobOptional(st.PeakMemory, chart.Bytes),
			OfMemTotal: jobMemShare(st.PeakMemory, d.MemTotal),
			PeakCPU:    jobCores(st.PeakCPU, d.CPUCount),
			Saturated:  jobOptional(st.Saturated, func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }),
			Samples:    strconv.FormatInt(st.Samples, 10),
		}
	}
	return rows
}

func jobMemShare(peak, total *float64) string {
	if peak == nil || total == nil || *total == 0 {
		return jobNone
	}
	return fmt.Sprintf("%.0f%%", *peak / *total * 100)
}

func jobCores(util, n *float64) string {
	if util == nil || n == nil {
		return jobNone
	}
	return fmt.Sprintf("%.1f of %.0f", *util**n, *n)
}

func jobStepDrawer(u *url.URL, d *store.JobDetail, st store.StepUsage, row jobStepRow) *jobDrawer {
	return &jobDrawer{
		Number: st.Number, Name: st.Name, Result: row.Result, Tone: jobTone(st.Conclusion),
		Facts: []jobFact{
			{"started", "Started (UTC)", row.Started},
			{"finished", "Finished (UTC)", jobClock(st.CompletedAt)},
			{"duration", "Duration", row.Duration},
			{"peak-memory", "Peak memory", row.PeakMemory},
			{"of-memtotal", "of MemTotal", row.OfMemTotal},
			{"peak-cpu", "Peak CPU (cores)", row.PeakCPU},
			{"saturated", "Time ≥90% CPU", row.Saturated},
			{"samples", "Samples", row.Samples},
		},
		LogURL: github.StepURL(d.HTMLURL, st.Number),
		Close:  jobStepHref(u, 0),
	}
}

// jobBin averages points into the axis's slices. A slice with no points
// is NaN.
func jobBin(ax jobAxis, points []store.SeriesPoint) []float64 {
	sums := make([]float64, ax.Slots)
	counts := make([]int, ax.Slots)
	for _, p := range points {
		i := int(p.Time.Sub(ax.Start) / ax.Width)
		if i >= 0 && i < ax.Slots && !p.Time.Before(ax.Start) {
			sums[i] += p.Value
			counts[i]++
		}
	}
	for i := range sums {
		if counts[i] == 0 {
			sums[i] = math.NaN()
		} else {
			sums[i] /= float64(counts[i])
		}
	}
	return sums
}

func jobPeak(values []float64) float64 {
	peak := 0.0
	for _, v := range values {
		if !math.IsNaN(v) {
			peak = max(peak, v)
		}
	}
	return peak
}

func jobPeakOf(series []chart.Series) float64 {
	peak := 0.0
	for _, s := range series {
		peak = max(peak, jobPeak(s.Values))
	}
	return peak
}

// jobUnit is a binary byte unit. Charts plot values divided by div, so
// their axis ticks land on round numbers of the unit.
type jobUnit struct {
	div          float64
	name, suffix string
}

// jobBinaryUnit is the largest unit that peak reaches, with suffix after
// its name, such as "/s".
func jobBinaryUnit(peak float64, suffix string) jobUnit {
	u := jobUnit{1, "B", suffix}
	for _, name := range []string{"KiB", "MiB", "GiB", "TiB"} {
		if peak < u.div*1024*0.9995 {
			break
		}
		u = jobUnit{u.div * 1024, name, suffix}
	}
	return u
}

func (u jobUnit) scale(series []chart.Series) {
	for _, s := range series {
		for i := range s.Values {
			s.Values[i] /= u.div
		}
	}
}

func (u jobUnit) format(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64) + " " + u.name + u.suffix
}

func jobSeriesOf(series []store.GaugerSeries, metric string, keep func(store.GaugerSeries) bool) []store.GaugerSeries {
	return slices.DeleteFunc(slices.Clone(series), func(g store.GaugerSeries) bool {
		return g.Metric != metric || !keep(g)
	})
}

// jobCharts builds the resource charts the job has samples for: CPU by
// mode, memory by state, disk and network traffic.
func jobCharts(u *url.URL, d *store.JobDetail, series []store.GaugerSeries, ax jobAxis) []jobChart {
	var marks []time.Time
	for _, st := range d.Steps {
		if st.StartedAt != nil {
			marks = append(marks, *st.StartedAt)
		}
	}
	trace := func(id string, kind chart.Kind, unstacked bool, format, tooltip func(float64) string, s []chart.Series) chart.TraceProps {
		return chart.TraceProps{
			Props: chart.Props{
				ID: id, Series: s, Kind: kind, Unstacked: unstacked, Height: 200,
				Format: format, FormatTooltip: tooltip, Hidden: chart.Hidden(u.Query(), id),
			},
			Start: ax.Start, End: ax.End, Slots: ax.Slots, Marks: marks,
		}
	}
	card := func(title, description string, tp chart.TraceProps, peak func(float64) string) jobChart {
		legend := chart.Toggles(tp.Props, u)
		for i, s := range tp.Series {
			legend[i].Value = "peak " + peak(jobPeak(s.Values))
		}
		return jobChart{Title: title, Description: description, Trace: tp, Legend: legend}
	}
	wholePercent := func(v float64) string { return strconv.FormatFloat(v*100, 'f', 0, 64) + "%" }
	const note = " Each slice averages its samples. Dashed lines are step starts."
	var out []jobChart

	var cpu []chart.Series
	modes := jobSeriesOf(series, store.MetricCPUUtilization, func(g store.GaugerSeries) bool { return g.Attr(store.AttrCPUMode) != "" })
	for i, mode := range []string{"user", "system", "iowait", "steal", "nice", "interrupt"} {
		if k := slices.IndexFunc(modes, func(g store.GaugerSeries) bool { return g.Attr(store.AttrCPUMode) == mode }); k >= 0 {
			cpu = append(cpu, chart.Series{Key: mode, Label: mode, Color: chart.SeriesColors[i], Values: jobBin(ax, modes[k].Points)})
		}
	}
	if len(cpu) == 0 {
		for _, g := range jobSeriesOf(series, store.MetricCPUUtilization, func(g store.GaugerSeries) bool { return g.Series == "" }) {
			cpu = append(cpu, chart.Series{Key: "total", Label: "total", Color: chart.SeriesColors[0], Values: jobBin(ax, g.Points)})
		}
	}
	if len(cpu) > 0 {
		tp := trace("cpu", chart.Area, false, wholePercent, chart.Percent, cpu)
		tp.YMax = 1
		tp.Empty = "No CPU use reported"
		out = append(out, card("CPU", "Share of all CPUs by mode, stacked."+note, tp, chart.Percent))
	}

	var mem []chart.Series
	states := jobSeriesOf(series, store.MetricMemoryUsage, func(g store.GaugerSeries) bool { return g.Attr(store.AttrMemoryState) != "" })
	for i, state := range []string{"used", "cached", "buffers"} {
		if k := slices.IndexFunc(states, func(g store.GaugerSeries) bool { return g.Attr(store.AttrMemoryState) == state }); k >= 0 {
			mem = append(mem, chart.Series{Key: state, Label: state, Color: chart.SeriesColors[i], Values: jobBin(ax, states[k].Points)})
		}
	}
	if len(mem) > 0 {
		peak := 0.0
		for _, s := range mem {
			peak += jobPeak(s.Values)
		}
		if d.MemTotal != nil {
			peak = max(peak, *d.MemTotal)
		}
		u := jobBinaryUnit(peak, "")
		u.scale(mem)
		tp := trace("memory", chart.Area, false, u.format, u.format, mem)
		if d.MemTotal != nil {
			tp.References = []chart.Reference{{Value: *d.MemTotal / u.div, Label: "MemTotal " + chart.Bytes(*d.MemTotal)}}
		}
		tp.Empty = "No memory use reported"
		out = append(out, card("Memory", "Used, cached and buffered memory, stacked, under the MemTotal limit."+note, tp, u.format))
	}

	var disk []chart.Series
	for _, g := range jobSeriesOf(series, store.MetricDiskIO, func(store.GaugerSeries) bool { return true }) {
		device, dir := g.Attr(store.AttrDevice), g.Attr(store.AttrDiskDirection)
		disk = append(disk, chart.Series{
			Key: dir + "-" + device, Label: strings.TrimSpace(device + " " + dir),
			Values: jobBin(ax, store.Rate(g.Points)),
		})
	}
	if len(disk) > 0 {
		jobSortSeries(disk)
		u := jobBinaryUnit(jobPeakOf(disk), "/s")
		u.scale(disk)
		tp := trace("disk", chart.Line, true, u.format, u.format, disk)
		tp.Empty = "No disk traffic reported"
		out = append(out, card("Disk", "Bytes read and written per second, per device."+note, tp, u.format))
	}

	var net []chart.Series
	for _, g := range jobSeriesOf(series, store.MetricNetworkIO, func(g store.GaugerSeries) bool { return store.CountedInterface(g.Attr(store.AttrInterface)) }) {
		iface, dir := g.Attr(store.AttrInterface), g.Attr(store.AttrNetworkDirection)
		switch dir {
		case "receive":
			dir = "in"
		case "transmit":
			dir = "out"
		}
		net = append(net, chart.Series{
			Key: dir + "-" + iface, Label: strings.TrimSpace(iface + " " + dir),
			Values: jobBin(ax, store.Rate(g.Points)),
		})
	}
	if len(net) > 0 {
		jobSortSeries(net)
		u := jobBinaryUnit(jobPeakOf(net), "/s")
		u.scale(net)
		tp := trace("network", chart.Line, true, u.format, u.format, net)
		tp.Empty = "No network traffic reported"
		out = append(out, card("Network", "Bytes received and sent per second, per interface, without loopback and tailscale0."+note, tp, u.format))
	}
	return out
}

// jobSortSeries orders series by label and colours them in that order.
func jobSortSeries(s []chart.Series) {
	slices.SortFunc(s, func(a, b chart.Series) int { return strings.Compare(a.Label, b.Label) })
	for i := range s {
		s[i].Color = chart.SeriesColors[i%len(chart.SeriesColors)]
	}
}
