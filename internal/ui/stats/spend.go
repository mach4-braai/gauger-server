package stats

import (
	"cmp"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// spendTableRows caps each of the per-repository, workflow and job tables.
const spendTableRows = 25

// unknownRate is the series for minutes on labels with no rate.
const unknownRate = "unknown rate"

// spendAgg sums the priced minutes of one table row.
type spendAgg struct {
	name, detail, href string
	local              bool
	jobs, minutes      int64
	unknown            int64
	cost               float64
	spark              []float64
}

type spendRow struct {
	Name, Detail, Href string
	Local              bool
	Jobs, Minutes      string
	Spend, Partial     string
	NoRate             bool
	Spark              []float64
}

type spendTable struct {
	ID, Title, Description string
	Rows                   []spendRow
	Total                  int
}

type monthRow struct {
	Month, Repository, Visibility, Labels string
	Jobs, Minutes, Rate, Cost             string
	NoRate, Free                          bool
}

type spendView struct {
	Description string
	Tiles       []tile
	Step        chart.Step
	Chart       chart.TimeProps
	Legend      []chart.LegendItem
	Minutes     bool
	Tables      []spendTable
	Months      []monthRow
}

// spendPoint is the priced minutes of one runner label in one bucket.
type spendPoint struct {
	bucket time.Time
	key    string
	cost   float64
	min    float64
}

// priceLabel names the series a set of labels is priced under: the label
// spend.Rates.Price picks, self-hosted first.
func priceLabel(rates spend.Rates, labels []string) string {
	if slices.Contains(labels, "self-hosted") {
		return "self-hosted"
	}
	for _, l := range labels {
		if _, ok := rates[l]; ok {
			return l
		}
	}
	return unknownRate
}

func (s *Server) spend(r *http.Request, f store.Filter) (templ.Component, error) {
	ctx := r.Context()
	st := step(f)
	rows, err := s.Store.SpendRows(ctx, f, string(st))
	if err != nil {
		return nil, err
	}
	months, err := s.Store.SpendMonths(ctx, f)
	if err != nil {
		return nil, err
	}
	var first time.Time
	if len(rows) > 0 {
		first = rows[0].Bucket
	}
	buckets := axis(f, st, first)
	slot := make(map[int64]int, len(buckets))
	for i, b := range buckets {
		slot[b.Unix()] = i
	}

	var (
		points                            []spendPoint
		minutes, free, billable, unpriced int64
		total                             float64
		totalSpark                        = make([]float64, len(buckets))
		byRepo                            = map[string]*spendAgg{}
		byWorkflow                        = map[[2]string]*spendAgg{}
		byJob                             = map[[3]string]*spendAgg{}
	)
	for _, row := range rows {
		p := s.Rates.Price(row.Labels, row.Private, row.Minutes)
		i, ok := slot[row.Bucket.Unix()]
		if !ok {
			continue
		}
		points = append(points, spendPoint{bucket: row.Bucket, key: priceLabel(s.Rates, row.Labels), cost: p.Cost, min: float64(row.Minutes)})
		minutes += row.Minutes
		total += p.Cost
		totalSpark[i] += p.Cost
		switch {
		case !p.Known:
			unpriced += row.Minutes
		case p.Free:
			free += row.Minutes
		default:
			billable += row.Minutes
		}

		touch := func(a *spendAgg) {
			if a.spark == nil {
				a.spark = make([]float64, len(buckets))
			}
			a.jobs += row.Jobs
			a.minutes += row.Minutes
			a.cost += p.Cost
			a.spark[i] += p.Cost
			if !p.Known {
				a.unknown += row.Minutes
			}
		}
		repo := byRepo[row.Repository]
		if repo == nil {
			repo = &spendAgg{name: row.Repository, href: repoLink(r.URL, row.Repository), local: true}
			byRepo[row.Repository] = repo
		}
		touch(repo)
		wk := [2]string{row.Repository, row.Workflow}
		wf := byWorkflow[wk]
		if wf == nil {
			wf = &spendAgg{name: row.Workflow, detail: row.Repository, href: github.WorkflowURL(s.GitHubURL, row.Repository, row.Path)}
			byWorkflow[wk] = wf
		}
		touch(wf)
		jk := [3]string{row.Repository, row.Workflow, row.Job}
		job := byJob[jk]
		if job == nil {
			job = &spendAgg{name: row.Job, detail: row.Workflow + " · " + row.Repository}
			byJob[jk] = job
		}
		touch(job)
	}

	asMinutes := r.URL.Query().Get("metric") == "minutes"
	props := chart.TimeProps{
		Props: chart.Props{
			ID:            "spend",
			Hidden:        chart.Hidden(r.URL.Query(), "spend"),
			Empty:         "No completed jobs in this range",
			Format:        axisUSD,
			FormatTooltip: chart.USD,
		},
		Buckets: buckets,
		Step:    st,
	}
	value := func(p spendPoint) float64 { return p.cost }
	format := chart.USD
	if asMinutes {
		props.Format, props.FormatTooltip = chart.Compact, chart.Integer
		value = func(p spendPoint) float64 { return p.min }
		format = chart.Integer
	}
	props.Series = chart.Pivot(buckets, points, func(p spendPoint) time.Time { return p.bucket },
		func(p spendPoint) string { return p.key }, value, len(chart.SeriesColors)-2)
	legend := chart.Toggles(props.Props, r.URL)
	for i, sr := range props.Series {
		sum := 0.0
		for _, v := range sr.Values {
			sum += v
		}
		legend[i].Value = format(sum)
	}

	spendHint := "at GitHub's list prices"
	if unpriced > 0 {
		spendHint = "plus " + chart.Integer(float64(unpriced)) + " min with no known rate"
	}
	unknownHint := "every label has a rate"
	if unpriced > 0 {
		unknownHint = "set GAUGER_RUNNER_RATES"
	}
	v := spendView{
		Description: "Estimated runner spend in " + describe(f) + ".",
		Tiles: []tile{
			{ID: "spend", Label: "Estimated spend", Value: chart.USD(total), Hint: spendHint, Spark: totalSpark,
				Title: "Job minutes times the runner label's rate, each job rounded up to a minute."},
			{ID: "billable", Label: "Billable minutes", Value: chart.Integer(float64(billable)), Hint: "on labels with a rate",
				Title: "Minutes on runners that cost money."},
			{ID: "free", Label: "Free minutes", Value: chart.Integer(float64(free)), Hint: "self-hosted or public",
				Title: "Self-hosted runners, and standard runners in public repositories."},
			{ID: "unknown", Label: "Unknown rate", Value: chart.Integer(float64(unpriced)), Hint: unknownHint,
				Title: "Minutes on runner labels with no known rate. They are left out of the spend."},
		},
		Step:    st,
		Chart:   props,
		Legend:  legend,
		Minutes: asMinutes,
		Tables: []spendTable{
			table("repository", "By repository", "Select a repository to filter the page.", byRepo),
			table("workflow", "By workflow", "", byWorkflow),
			table("job", "By job", "Job names across all runs of the workflow.", byJob),
		},
	}
	for _, g := range months {
		p := s.Rates.Price(g.Labels, g.Private, g.Minutes)
		m := monthRow{
			Month: g.Month.UTC().Format("2006-01"), Repository: g.Repository, Visibility: visibility(g.Private),
			Labels: strings.Join(g.Labels, ", "), Jobs: strconv.FormatInt(g.Jobs, 10), Minutes: strconv.FormatInt(g.Minutes, 10),
			Rate: "unknown", Cost: "unknown", NoRate: !p.Known, Free: p.Free,
		}
		switch {
		case p.Free:
			m.Cost = "free"
		case p.Known:
			m.Cost = chart.USD(p.Cost)
		}
		if p.Known {
			m.Rate = "$" + strconv.FormatFloat(p.Rate, 'f', -1, 64) + "/min"
		}
		v.Months = append(v.Months, m)
	}
	return spendPage(v), nil
}

// repoLink is the page filtered to one repository.
func repoLink(u *url.URL, repo string) string {
	q := u.Query()
	q.Set("repo", repo)
	return u.Path + "?" + q.Encode()
}

func visibility(private *bool) string {
	switch {
	case private == nil:
		return "(visibility unknown)"
	case !*private:
		return "(public)"
	}
	return ""
}

// table ranks aggs by spend, then minutes, and keeps the first spendTableRows.
func table[K comparable](id, title, description string, aggs map[K]*spendAgg) spendTable {
	all := make([]*spendAgg, 0, len(aggs))
	for _, a := range aggs {
		all = append(all, a)
	}
	slices.SortFunc(all, func(a, b *spendAgg) int {
		return cmp.Or(
			cmp.Compare(b.cost, a.cost),
			cmp.Compare(b.minutes, a.minutes),
			cmp.Compare(a.name, b.name),
			cmp.Compare(a.detail, b.detail),
		)
	})
	t := spendTable{ID: id, Title: title, Description: description, Total: len(all)}
	for _, a := range all[:min(len(all), spendTableRows)] {
		row := spendRow{
			Name: a.name, Detail: a.detail, Href: a.href, Local: a.local,
			Jobs: chart.Integer(float64(a.jobs)), Minutes: chart.Integer(float64(a.minutes)),
			Spend: chart.USD(a.cost), Spark: a.spark,
		}
		switch {
		case a.unknown == a.minutes:
			row.Spend, row.NoRate = "unknown", true
		case a.unknown > 0:
			row.Partial = "+ " + chart.Integer(float64(a.unknown)) + " min unknown"
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// axisUSD labels the spend axis: whole dollars, cents under a dollar's
// precision, and K or M above a thousand.
func axisUSD(v float64) string {
	switch {
	case v >= 1000:
		return "$" + chart.Compact(v)
	case v == math.Trunc(v):
		return "$" + chart.Integer(v)
	}
	return chart.USD(v)
}
