package stats

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// resourcesMaxGroups is how many groups the charts draw, the ones with
// the most sampled jobs. The tables list every group.
const resourcesMaxGroups = 10

// resourcesModes are the CPU modes the charts draw, in stack order and
// coloured by position as the job page does. Idle is left out.
var resourcesModes = []string{"user", "system", "iowait", "steal", "nice", "interrupt"}

var resourcesMemoryStates = []string{"used", "cached", "buffers"}

type resourcesEmpty struct {
	Title, Hint string
}

// resourcesCell is a table cell: a mean, and the peak beside it.
type resourcesCell struct {
	Col, Mean, Peak string
}

type resourcesRow struct {
	Group string
	Jobs  string
	Cells []resourcesCell
}

type resourcesMemoryRow struct {
	Group        string
	Jobs         string
	Limit        string
	Cells        []resourcesCell
	PeakShare    string
	MeanShare    string
	MinAvailable string
}

type resourcesTrafficRow struct {
	Group, Name, In, Out string
}

type resourcesView struct {
	Description string
	GroupName   string
	Tiles       []tile
	Empty       *resourcesEmpty
	Note        string

	CPU        chart.Props
	CPULegend  []chart.LegendItem
	CPUCallout []tile
	CPUColumns []string
	CPURows    []resourcesRow

	Memory       chart.Props
	MemoryLegend []chart.LegendItem
	MemoryRows   []resourcesMemoryRow

	Disk    []resourcesTrafficRow
	Network []resourcesTrafficRow

	TopDisk    []chart.BarListItem
	TopNetwork []chart.BarListItem

	Trend       chart.TimeProps
	TrendLegend []chart.LegendItem
}

func (s *Server) resources(r *http.Request, f store.Filter) (templ.Component, error) {
	by := store.ByLabel
	if r.URL.Query().Get("by") == store.ByWorkflow {
		by = store.ByWorkflow
	}
	st := step(f)
	res, err := s.Store.Resources(r.Context(), f, by, string(st))
	if err != nil {
		return nil, err
	}
	retention := s.Store.Retention()
	v := resourcesView{
		Description: "CPU, memory, disk and network use per runner " + by + " in " + describe(f) + ".",
		GroupName:   by,
		Tiles:       resourcesTiles(res, retention),
	}
	if res.Sampled == 0 {
		v.Empty = resourcesEmptyState(f, res, retention)
		return resourcesPage(v, by), nil
	}
	if res.Since.After(f.Since) {
		v.Note = "Samples are kept for " + resourcesDays(retention) + ", so this range is read from " + res.Since.Format("2006-01-02") + "."
	}
	shown := res.Groups[:min(len(res.Groups), resourcesMaxGroups)]
	if len(shown) < len(res.Groups) {
		v.Note = strings.TrimSpace(v.Note + fmt.Sprintf(" The charts draw the %d %ss with the most sampled jobs. The tables list all %d.", len(shown), by, len(res.Groups)))
	}
	v.addCPU(r.URL, res.Groups, shown)
	v.addMemory(r.URL, res.Groups, shown)
	v.addTraffic(res)
	v.TopDisk = resourcesTopItems(res.TopDisk)
	v.TopNetwork = resourcesTopItems(res.TopNetwork)
	v.addTrend(r.URL, f, st, res.Timeline)
	return resourcesPage(v, by), nil
}

func resourcesTiles(res *store.Resources, retention time.Duration) []tile {
	oldest := tile{
		ID: "resources-oldest", Label: "Oldest sample kept", Value: "–",
		Hint:  "samples are kept for " + resourcesDays(retention),
		Title: "Samples older than the retention period are deleted, a day at a time.",
	}
	if res.Oldest != nil {
		oldest.Value = res.Oldest.Format("2006-01-02")
	}
	return []tile{
		{
			ID: "resources-sampled", Label: "Jobs with samples", Value: chart.Integer(float64(res.Sampled)),
			Hint:  chart.Integer(float64(res.FromArtifact)) + " from the fallback artifact",
			Title: "Jobs with runner samples in this range. Everything below counts only these.",
		},
		{
			ID: "resources-share", Label: "Share of all jobs", Value: ratio(res.Sampled, res.Jobs),
			Hint: chart.Integer(float64(res.Sampled)) + " of " + chart.Integer(float64(res.Jobs)) + " jobs",
		},
		oldest,
	}
}

func resourcesDays(d time.Duration) string {
	if d%(24*time.Hour) != 0 {
		return d.String()
	}
	days := int64(d / (24 * time.Hour))
	if days == 1 {
		return "1 day"
	}
	return strconv.FormatInt(days, 10) + " days"
}

// resourcesEmptyState says why nothing is drawn: no job ran, the range
// reaches back past what retention keeps, or no job reported samples.
func resourcesEmptyState(f store.Filter, res *store.Resources, retention time.Duration) *resourcesEmpty {
	switch {
	case res.Jobs == 0:
		return &resourcesEmpty{"No jobs in this range", "No job started in " + describe(f) + "."}
	case f.Since.Before(res.Since):
		return &resourcesEmpty{
			"No samples in this range",
			"gauger keeps samples for " + resourcesDays(retention) + ", so nothing before " + res.Since.Format("2006-01-02") +
				" is available. " + chart.Integer(float64(res.Jobs)) + " jobs in this range have none left.",
		}
	}
	return &resourcesEmpty{
		"No jobs with samples in this range",
		chart.Integer(float64(res.Jobs)) + " jobs started in " + describe(f) + ", and none reported runner samples.",
	}
}

func resourcesWholePercent(v float64) string {
	return strconv.FormatFloat(v*100, 'f', 0, 64) + "%"
}

func resourcesTick(key string) string {
	if r := []rune(key); len(r) > 12 {
		return string(r[:11]) + "…"
	}
	return key
}

func resourcesBytes(v *float64) string {
	if v == nil {
		return "–"
	}
	return chart.Bytes(*v)
}

func resourcesPercent(v *float64) string {
	if v == nil {
		return "–"
	}
	return chart.Percent(*v)
}

func resourcesMode(g store.ResourceGroup, mode string) *store.ResourceMode {
	if mode == "total" {
		return g.Total
	}
	for i := range g.Modes {
		if g.Modes[i].Mode == mode {
			return &g.Modes[i]
		}
	}
	return nil
}

func resourcesState(g store.ResourceGroup, state string) *store.ResourceState {
	for i := range g.States {
		if g.States[i].State == state {
			return &g.States[i]
		}
	}
	return nil
}

// resourcesMean is a mode's mean over every sample of every group, and
// the group where it is highest.
func resourcesMean(groups []store.ResourceGroup, mode string) (mean float64, top string, topMean float64, ok bool) {
	var sum float64
	var n int64
	for _, g := range groups {
		if m := resourcesMode(g, mode); m != nil {
			sum += m.Mean * float64(m.Samples)
			n += m.Samples
			if m.Mean > topMean || top == "" {
				top, topMean = g.Key, m.Mean
			}
		}
	}
	if n == 0 {
		return 0, "", 0, false
	}
	return sum / float64(n), top, topMean, true
}

func resourcesModeTile(groups []store.ResourceGroup, mode, what string) tile {
	t := tile{ID: "resources-" + mode, Label: mode, Value: "–", Small: true, Title: mode + " is the share of CPU time spent " + what + "."}
	if mean, top, topMean, ok := resourcesMean(groups, mode); ok {
		t.Value = chart.Percent(mean)
		t.Hint = "highest in " + top + ", " + chart.Percent(topMean)
	}
	return t
}

// addCPU stacks each shown group's mean share of all CPUs by mode. A
// group whose runner sent no mode split shows its total instead.
func (v *resourcesView) addCPU(u *url.URL, all, shown []store.ResourceGroup) {
	v.CPU = chart.Props{
		ID: "resourcescpu", Kind: chart.Bars, Hidden: chart.Hidden(u.Query(), "resourcescpu"),
		Format: resourcesWholePercent, FormatTooltip: chart.Percent, Empty: "No CPU use reported",
	}
	for _, g := range shown {
		v.CPU.Ticks = append(v.CPU.Ticks, resourcesTick(g.Key))
		v.CPU.Titles = append(v.CPU.Titles, g.Key)
	}
	var drawn []string
	for i, mode := range resourcesModes {
		values := make([]float64, len(shown))
		used := false
		for k, g := range shown {
			if m := resourcesMode(g, mode); m != nil {
				values[k] = m.Mean
				used = used || m.Mean > 0
			}
		}
		if used {
			drawn = append(drawn, mode)
			v.CPU.Series = append(v.CPU.Series, chart.Series{Key: mode, Label: mode, Color: chart.SeriesColors[i], Values: values})
		}
	}
	unsplit := make([]float64, len(shown))
	hasUnsplit := false
	for k, g := range shown {
		if len(g.Modes) == 0 && g.Total != nil {
			unsplit[k] = g.Total.Mean
			hasUnsplit = true
		}
	}
	if hasUnsplit {
		v.CPU.Series = append(v.CPU.Series, chart.Series{Key: "total", Label: "total", Color: chart.OtherColor, Values: unsplit})
	}

	v.CPULegend = chart.Toggles(v.CPU, u)
	for i, s := range v.CPU.Series {
		if mean, _, _, ok := resourcesMean(all, s.Key); ok && s.Key != "total" {
			v.CPULegend[i].Value = chart.Percent(mean)
		}
	}
	v.CPUCallout = []tile{
		resourcesModeTile(all, "iowait", "waiting on disk"),
		resourcesModeTile(all, "steal", "taken by the host for other virtual machines, a sign of a busy host"),
	}

	cols := drawn
	for _, g := range all {
		if g.Total != nil {
			cols = append([]string{"total"}, drawn...)
			break
		}
	}
	v.CPUColumns = cols
	for _, g := range all {
		row := resourcesRow{Group: g.Key, Jobs: chart.Integer(float64(g.Jobs))}
		for _, col := range cols {
			cell := resourcesCell{Col: col, Mean: "–"}
			if m := resourcesMode(g, col); m != nil {
				cell.Mean, cell.Peak = chart.Percent(m.Mean), chart.Percent(m.Peak)
			}
			row.Cells = append(row.Cells, cell)
		}
		v.CPURows = append(v.CPURows, row)
	}
}

// addMemory draws each shown group's peak and mean used memory as a share
// of system.memory.limit, and tables every group's states.
func (v *resourcesView) addMemory(u *url.URL, all, shown []store.ResourceGroup) {
	v.Memory = chart.Props{
		ID: "resourcesmemory", Kind: chart.Bars, Unstacked: true, Hidden: chart.Hidden(u.Query(), "resourcesmemory"),
		Format: resourcesWholePercent, FormatTooltip: chart.Percent, YMax: 1,
		References: []chart.Reference{{Value: 1, Label: "limit"}},
		Empty:      "No memory use reported",
	}
	peak := make([]float64, len(shown))
	mean := make([]float64, len(shown))
	for k, g := range shown {
		v.Memory.Ticks = append(v.Memory.Ticks, resourcesTick(g.Key))
		v.Memory.Titles = append(v.Memory.Titles, g.Key)
		if g.PeakShare != nil {
			peak[k] = *g.PeakShare
		}
		if g.MeanShare != nil {
			mean[k] = *g.MeanShare
		}
	}
	v.Memory.Series = []chart.Series{
		{Key: "mean", Label: "mean used", Color: chart.SeriesColors[0], Values: mean},
		{Key: "peak", Label: "peak used", Color: chart.SeriesColors[1], Values: peak},
	}
	v.MemoryLegend = chart.Toggles(v.Memory, u)

	for _, g := range all {
		row := resourcesMemoryRow{
			Group: g.Key, Jobs: chart.Integer(float64(g.Jobs)), Limit: resourcesBytes(g.Limit),
			PeakShare: resourcesPercent(g.PeakShare), MeanShare: resourcesPercent(g.MeanShare),
			MinAvailable: resourcesBytes(g.MinAvailable),
		}
		for _, state := range resourcesMemoryStates {
			cell := resourcesCell{Col: state, Mean: "–"}
			if st := resourcesState(g, state); st != nil {
				cell.Mean, cell.Peak = chart.Bytes(st.Mean), chart.Bytes(st.Peak)
			}
			row.Cells = append(row.Cells, cell)
		}
		v.MemoryRows = append(v.MemoryRows, row)
	}
}

func (v *resourcesView) addTraffic(res *store.Resources) {
	rows := func(t []store.ResourceTraffic) []resourcesTrafficRow {
		out := make([]resourcesTrafficRow, len(t))
		for i, r := range t {
			out[i] = resourcesTrafficRow{r.Group, r.Name, chart.Bytes(r.In), chart.Bytes(r.Out)}
		}
		return out
	}
	v.Disk = rows(res.Disk)
	v.Network = rows(res.Network)
}

func resourcesTopItems(jobs []store.ResourceJob) []chart.BarListItem {
	items := make([]chart.BarListItem, len(jobs))
	for i, j := range jobs {
		id := strconv.FormatInt(j.ID, 10)
		total := j.In + j.Out
		items[i] = chart.BarListItem{
			Key: id, Label: j.Repository + " · " + j.Workflow + " · " + j.Name,
			Value: total, Display: chart.Bytes(total), Href: "/stats/jobs/" + id,
		}
	}
	return items
}

// addTrend draws the mean CPU use and the mean memory use, over the mean
// limit, of every sample in each bucket. A bucket with no samples is a
// gap.
func (v *resourcesView) addTrend(u *url.URL, f store.Filter, st chart.Step, timeline []store.ResourceBucket) {
	var first time.Time
	if len(timeline) > 0 {
		first = timeline[0].Bucket
	}
	buckets := axis(f, st, first)
	index := make(map[int64]int, len(buckets))
	cpu := make([]float64, len(buckets))
	mem := make([]float64, len(buckets))
	for i, b := range buckets {
		index[b.Unix()] = i
		cpu[i], mem[i] = math.NaN(), math.NaN()
	}
	for _, b := range timeline {
		i, ok := index[b.Bucket.Unix()]
		if !ok {
			continue
		}
		if b.CPU != nil {
			cpu[i] = *b.CPU
		}
		if b.Memory != nil {
			mem[i] = *b.Memory
		}
	}
	v.Trend = chart.TimeProps{
		Props: chart.Props{
			ID: "resourcestrend", Kind: chart.Line, Unstacked: true, Hidden: chart.Hidden(u.Query(), "resourcestrend"),
			Format: resourcesWholePercent, FormatTooltip: chart.Percent, YMax: 1, Empty: "No samples in this range",
			Series: []chart.Series{
				{Key: "cpu", Label: "CPU", Color: chart.SeriesColors[0], Values: cpu},
				{Key: "memory", Label: "memory used", Color: chart.SeriesColors[1], Values: mem},
			},
		},
		Buckets: buckets,
		Step:    st,
	}
	v.TrendLegend = chart.Toggles(v.Trend.Props, u)
}
