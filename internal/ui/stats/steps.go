package stats

import (
	"cmp"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

const (
	stepsTopN      = 10
	stepsRowLimit  = 200
	stepsSetupHue  = "var(--warn)"
	stepsWorkHue   = "var(--chart-primary)"
	stepsSortParam = "sort"
	stepsDirParam  = "dir"
)

// stepsCol is one column of the steps table. Desc says which way its first
// click sorts.
type stepsCol struct {
	Key   string
	Label string
	Right bool
	Desc  bool
}

var stepsCols = []stepsCol{
	{"name", "Step", false, false},
	{"kind", "Kind", false, false},
	{"runs", "Runs", true, true},
	{"total", "Total min", true, true},
	{"p50", "p50", true, true},
	{"p95", "p95", true, true},
	{"failures", "Failures", true, true},
	{"slowest", "Slowest", true, true},
}

const stepsDefaultSort = "total"

// stepsHead is a table header with the link that sorts by it.
type stepsHead struct {
	stepsCol
	Href string
	// Dir is "asc" or "desc" on the sorted column and empty on the rest.
	Dir string
}

type stepsRow struct {
	Name     string
	Setup    bool
	Runs     string
	Minutes  string
	P50      string
	P95      string
	Failures string
	Failed   bool
	Slowest  string
	// JobHref is the dashboard page of the slowest occurrence's job.
	JobHref string
	// GitHubHref is that occurrence's step on GitHub, empty if unknown.
	GitHubHref string
	StepNumber string
	Spark      []float64
}

type stepsView struct {
	Description string
	Tiles       []tile
	Empty       bool
	Top         []chart.BarListItem
	Split       []chart.ShareSegment
	SplitLegend []chart.LegendItem
	Share       chart.TimeProps
	Step        chart.Step
	Heads       []stepsHead
	Rows        []stepsRow
	TableNote   string
	// KeepSort puts the table's sort in the filter form, so a filter change
	// keeps it.
	KeepSort  bool
	Sort, Dir string
}

func (s *Server) steps(r *http.Request, f store.Filter) (templ.Component, error) {
	st := step(f)
	ss, err := s.Store.Steps(r.Context(), f, string(st))
	if err != nil {
		return nil, err
	}

	var setupSecs, workSecs float64
	var runs int64
	for _, row := range ss.Steps {
		runs += row.Runs
		if row.Setup {
			setupSecs += row.Seconds
		} else {
			workSecs += row.Seconds
		}
	}
	totalSecs := setupSecs + workSecs
	share := func(v float64) string {
		if totalSecs == 0 {
			return "–"
		}
		return chart.Percent(v / totalSecs)
	}
	v := stepsView{
		Description: "Where step time goes in " + describe(f) + ", by step name with the @ref of action steps removed.",
		Empty:       len(ss.Steps) == 0,
		Step:        st,
		Tiles: []tile{
			{ID: "names", Label: "Steps", Value: chart.Integer(float64(len(ss.Steps))), Hint: chart.Integer(float64(runs)) + " runs"},
			{ID: "minutes", Label: "Step minutes", Value: stepsMinutes(totalSecs), Hint: "setup plus work",
				Title: "Time spent in steps that ran, not rounded per job."},
			{ID: "setup", Label: "Setup minutes", Value: stepsMinutes(setupSecs), Hint: share(setupSecs) + " of the total",
				Title: "Set up job, Complete job, Post steps, checkout, cache restore and mise-action."},
			{ID: "work", Label: "Work minutes", Value: stepsMinutes(workSecs), Hint: share(workSecs) + " of the total"},
		},
		Split: []chart.ShareSegment{
			{Key: "setup", Label: "Setup", Value: setupSecs, Color: stepsSetupHue},
			{Key: "work", Label: "Work", Value: workSecs, Color: stepsWorkHue},
		},
		SplitLegend: []chart.LegendItem{
			{Key: "setup", Label: "Setup", Color: stepsSetupHue, Value: stepsMinutes(setupSecs) + " min"},
			{Key: "work", Label: "Work", Color: stepsWorkHue, Value: stepsMinutes(workSecs) + " min"},
		},
	}

	for _, row := range ss.Steps[:min(stepsTopN, len(ss.Steps))] {
		v.Top = append(v.Top, chart.BarListItem{
			Key: row.Name, Label: row.Name, Value: row.Seconds / 60,
			Display: stepsMinutes(row.Seconds) + " min", Color: stepsHue(row.Setup),
			Href: "/stats/jobs/" + strconv.FormatInt(row.JobID, 10),
		})
	}

	var first time.Time
	if len(ss.Buckets) > 0 {
		first = ss.Buckets[0].Bucket
	}
	buckets := axis(f, st, first)
	at := func(b store.StepBucket) time.Time { return b.Bucket }
	all := chart.Densify(buckets, ss.Buckets, at, func(b store.StepBucket) float64 { return b.Seconds })
	setup := chart.Densify(buckets, ss.Buckets, at, func(b store.StepBucket) float64 {
		if b.Setup {
			return b.Seconds
		}
		return 0
	})
	ratios := make([]float64, len(buckets))
	for i := range ratios {
		ratios[i] = math.NaN()
		if all[i] > 0 {
			ratios[i] = setup[i] / all[i]
		}
	}
	v.Share = chart.TimeProps{
		Props: chart.Props{
			ID:            "setupshare",
			Kind:          chart.Line,
			Series:        []chart.Series{{Key: "setup", Label: "Setup share", Color: stepsSetupHue, Values: ratios}},
			Format:        stepsWholePercent,
			FormatTooltip: chart.Percent,
			YMax:          1,
			Empty:         "No steps in this range",
		},
		Buckets: buckets,
		Step:    st,
	}

	key, desc := stepsSort(r)
	table := ss.Steps[:min(stepsRowLimit, len(ss.Steps))]
	if len(table) < len(ss.Steps) {
		v.TableNote = "The " + strconv.Itoa(len(table)) + " step names with the most minutes, of " + chart.Integer(float64(len(ss.Steps))) + "."
	}
	table = slices.Clone(table)
	stepsSortRows(table, key, desc)
	v.Sort, v.Dir, v.KeepSort = key, stepsDirName(desc), r.URL.Query().Has(stepsSortParam)
	for _, c := range stepsCols {
		h := stepsHead{stepsCol: c}
		next := c.Desc
		if c.Key == key {
			h.Dir = stepsDirName(desc)
			next = !desc
		}
		h.Href = stepsSortURL(r, c.Key, next)
		v.Heads = append(v.Heads, h)
	}

	byName := map[string][]store.StepBucket{}
	for _, b := range ss.Buckets {
		byName[b.Name] = append(byName[b.Name], b)
	}
	for _, row := range table {
		spark := chart.Densify(buckets, byName[row.Name], at, func(b store.StepBucket) float64 { return b.Seconds / 60 })
		v.Rows = append(v.Rows, stepsRow{
			Name: row.Name, Setup: row.Setup,
			Runs:       chart.Integer(float64(row.Runs)),
			Minutes:    stepsMinutes(row.Seconds),
			P50:        chart.Seconds(row.P50),
			P95:        chart.Seconds(row.P95),
			Failures:   chart.Integer(float64(row.Failures)),
			Failed:     row.Failures > 0,
			Slowest:    chart.Seconds(row.SlowestSeconds),
			JobHref:    "/stats/jobs/" + strconv.FormatInt(row.JobID, 10),
			GitHubHref: github.StepURL(row.JobHTMLURL, row.StepNumber),
			StepNumber: strconv.Itoa(row.StepNumber),
			Spark:      spark,
		})
	}
	return stepsPage(v), nil
}

func stepsHue(setup bool) string {
	if setup {
		return stepsSetupHue
	}
	return stepsWorkHue
}

// stepsMinutes formats seconds as minutes, with a decimal below ten.
func stepsMinutes(secs float64) string {
	m := secs / 60
	if m < 9.95 {
		return strconv.FormatFloat(m, 'f', 1, 64)
	}
	return chart.Integer(m)
}

func stepsWholePercent(v float64) string {
	return strconv.FormatFloat(v*100, 'f', 0, 64) + "%"
}

func stepsDirName(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

// stepsSort reads the table's sort column and direction from the query.
// Unknown or missing values fall back to total minutes, most first.
func stepsSort(r *http.Request) (key string, desc bool) {
	q := r.URL.Query()
	key = stepsDefaultSort
	for _, c := range stepsCols {
		if c.Key == q.Get(stepsSortParam) {
			key = c.Key
		}
	}
	for _, c := range stepsCols {
		if c.Key == key {
			desc = c.Desc
		}
	}
	switch q.Get(stepsDirParam) {
	case "asc":
		desc = false
	case "desc":
		desc = true
	}
	return key, desc
}

// stepsSortURL is the request's URL sorted by key.
func stepsSortURL(r *http.Request, key string, desc bool) string {
	u := *r.URL
	q := u.Query()
	q.Set(stepsSortParam, key)
	q.Set(stepsDirParam, stepsDirName(desc))
	u.RawQuery = q.Encode()
	return u.String()
}

// stepsSortRows orders rows by the column key, equal rows by name.
func stepsSortRows(rows []store.StepStat, key string, desc bool) {
	compare := func(a, b store.StepStat) int {
		switch key {
		case "name":
			return strings.Compare(a.Name, b.Name)
		case "kind":
			switch {
			case a.Setup == b.Setup:
				return 0
			case a.Setup:
				return -1
			}
			return 1
		case "runs":
			return cmp.Compare(a.Runs, b.Runs)
		case "p50":
			return cmp.Compare(a.P50, b.P50)
		case "p95":
			return cmp.Compare(a.P95, b.P95)
		case "failures":
			return cmp.Compare(a.Failures, b.Failures)
		case "slowest":
			return cmp.Compare(a.SlowestSeconds, b.SlowestSeconds)
		}
		return cmp.Compare(a.Seconds, b.Seconds)
	}
	slices.SortStableFunc(rows, func(a, b store.StepStat) int {
		c := compare(a, b)
		if desc {
			c = -c
		}
		return cmp.Or(c, strings.Compare(a.Name, b.Name))
	})
}
