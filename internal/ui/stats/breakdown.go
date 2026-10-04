package stats

import (
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

// maxBreakdownRows caps the table. The totals row still counts every group.
const maxBreakdownRows = 200

// shareSegments is how many groups the minutes bar names before folding
// the rest into "Other".
const shareSegments = 8

type breakdownDimension struct {
	Key   store.BreakdownDimension
	Label string
	// Noun names one group in prose.
	Noun string
}

var breakdownDimensions = []breakdownDimension{
	{store.BreakdownByRepository, "Repository", "repository"},
	{store.BreakdownByWorkflow, "Workflow", "workflow"},
	{store.BreakdownByJob, "Job", "job"},
	{store.BreakdownByEvent, "Event", "event"},
	{store.BreakdownByBranch, "Branch", "branch"},
}

// breakdownColumn is a table header. Sorting a column links to the same
// page ordered by it.
type breakdownColumn struct {
	Key, Label, Title string
	Right             bool
	Sorted            string
	Href              string
}

// breakdownRow is one group with what the table shows.
type breakdownRow struct {
	Name, Sub  string
	Empty      bool
	External   string
	RunsHref   string
	TrendsHref string
	Runs       int64
	Jobs       int64
	Minutes    int64
	Spend      float64
	Unpriced   int64
	Success    *float64
	Failure    *float64
	P50, P95   *float64
	Queue      *float64
	Spark      []float64
}

type breakdownView struct {
	Description string
	By          breakdownDimension
	Dimensions  []breakdownDimension
	Sort, Dir   string
	Columns     []breakdownColumn
	Rows        []breakdownRow
	Hidden      int
	Total       breakdownRow
	// TotalRuns is false when a run counts under several groups.
	TotalRuns bool
	Branch    bool
	Share     []chart.ShareSegment
	Legend    []chart.LegendItem
	Step      chart.Step
}

func (s *Server) breakdown(r *http.Request, f store.Filter) (templ.Component, error) {
	q := r.URL.Query()
	by := breakdownDimensions[0]
	for _, d := range breakdownDimensions {
		if string(d.Key) == q.Get("by") {
			by = d
		}
	}
	st := step(f)
	groups, err := s.Store.Breakdown(r.Context(), f, by.Key, string(st))
	if err != nil {
		return nil, err
	}

	first := f.Until
	if f.Since.IsZero() {
		for _, g := range groups {
			if len(g.Spark) > 0 && g.Spark[0].Bucket.Before(first) {
				first = g.Spark[0].Bucket
			}
		}
	}
	buckets := axis(f, st, first)

	links := url.Values{}
	for _, k := range []string{"range", "repo", "event"} {
		if v := q.Get(k); v != "" {
			links.Set(k, v)
		}
	}

	rows := make([]breakdownRow, len(groups))
	var total breakdownRow
	for i, g := range groups {
		row := breakdownRow{
			Runs: g.Runs, Jobs: g.Jobs, P50: g.P50, P95: g.P95, Queue: g.QueueP95,
			Spark: chart.Densify(buckets, g.Spark,
				func(c store.BucketCount) time.Time { return c.Bucket },
				func(c store.BucketCount) float64 { return float64(c.Runs) }),
		}
		for _, m := range g.Minutes {
			row.Minutes += m.Minutes
			p := s.Rates.Price(m.Labels, m.Private, m.Minutes)
			row.Spend += p.Cost
			if !p.Known {
				row.Unpriced += m.Minutes
			}
		}
		if g.Decided > 0 {
			ok := float64(g.Succeeded) / float64(g.Decided)
			bad := float64(g.Decided-g.Succeeded) / float64(g.Decided)
			row.Success, row.Failure = &ok, &bad
		}
		s.nameBreakdownRow(&row, by.Key, g, links)
		rows[i] = row
		total.Runs += row.Runs
		total.Jobs += row.Jobs
		total.Minutes += row.Minutes
		total.Spend += row.Spend
		total.Unpriced += row.Unpriced
	}

	sortKey, desc := breakdownSort(q, by.Key)
	sortBreakdown(rows, sortKey, desc)
	v := breakdownView{
		Description: "Runs, jobs and job minutes in " + describe(f) + ", by " + by.Noun + ".",
		By:          by,
		Dimensions:  breakdownDimensions,
		Sort:        sortKey,
		Dir:         dirName(desc),
		Total:       total,
		TotalRuns:   by.Key != store.BreakdownByJob,
		Branch:      by.Key == store.BreakdownByBranch,
		Step:        st,
	}
	v.Share, v.Legend = shareOfMinutes(rows)
	if len(rows) > maxBreakdownRows {
		v.Hidden = len(rows) - maxBreakdownRows
		rows = rows[:maxBreakdownRows]
	}
	v.Rows = rows
	v.Columns = breakdownColumns(r.URL, by, v.Branch, sortKey, desc)
	return breakdownPage(v), nil
}

// nameBreakdownRow fills what identifies the group: its label, a line
// under it, the GitHub page it names and the Runs and Trends links
// narrowed to it. links holds the shell's filter. A row links only to
// the pages that can narrow to it: Runs has no job filter and matches a
// workflow by run name, which a dynamic workflow's runs don't share, and
// Trends ignores event and branch.
func (s *Server) nameBreakdownRow(row *breakdownRow, by store.BreakdownDimension, g store.BreakdownRow, links url.Values) {
	row.Name = g.Name
	row.Empty = g.Name == ""
	href := func(page string, kv ...string) string {
		if row.Empty {
			return ""
		}
		return withQuery(&url.URL{Path: page, RawQuery: links.Encode()}, kv...)
	}
	switch by {
	case store.BreakdownByRepository:
		row.RunsHref = href("/stats/runs", "repo", g.Name)
		row.TrendsHref = href("/stats/trends", "repo", g.Name)
	case store.BreakdownByWorkflow:
		row.Sub = g.Repository
		row.External = github.WorkflowURL(s.GitHubURL, g.Repository, g.Path)
		if !strings.HasPrefix(g.Path, "dynamic/") {
			row.RunsHref = href("/stats/runs", "repo", g.Repository, "workflow", g.Workflow)
		}
		row.TrendsHref = href("/stats/trends", "repo", g.Repository, "workflow", g.Workflow)
	case store.BreakdownByJob:
		row.Sub = g.Repository
		if g.Workflow != "" {
			row.Sub += " · " + g.Workflow
			row.TrendsHref = href("/stats/trends", "repo", g.Repository, "workflow", g.Workflow, "job", g.Name)
		}
	case store.BreakdownByEvent:
		row.RunsHref = href("/stats/runs", "event", g.Name)
	case store.BreakdownByBranch:
		row.RunsHref = href("/stats/runs", "branch", g.Name)
	}
}

// breakdownSort reads the sort column and direction from the query. A
// column of the other dimensions' tables, or none, sorts by minutes; the
// direction defaults to descending, ascending for names.
func breakdownSort(q url.Values, by store.BreakdownDimension) (key string, desc bool) {
	key = "minutes"
	for _, c := range breakdownColumnKeys(by == store.BreakdownByBranch) {
		if c == q.Get("sort") {
			key = c
		}
	}
	desc = key != "name"
	switch q.Get("dir") {
	case "asc":
		desc = false
	case "desc":
		desc = true
	}
	return key, desc
}

func breakdownColumnKeys(branch bool) []string {
	rate := "success"
	if branch {
		rate = "failure"
	}
	return []string{"name", "runs", "jobs", "minutes", "spend", rate, "p50", "p95", "queue"}
}

func dirName(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

// sortBreakdown orders rows by a column. Rows without a value for it go
// last whichever way it sorts.
func sortBreakdown(rows []breakdownRow, key string, desc bool) {
	slices.SortStableFunc(rows, func(a, b breakdownRow) int {
		if key == "name" {
			return cmpDir(compareStrings(a.Name, b.Name), desc)
		}
		x, xok := a.value(key)
		y, yok := b.value(key)
		switch {
		case !xok && !yok:
			return 0
		case !xok:
			return 1
		case !yok:
			return -1
		}
		switch {
		case x < y:
			return cmpDir(-1, desc)
		case x > y:
			return cmpDir(1, desc)
		}
		return compareStrings(a.Name, b.Name)
	})
}

func cmpDir(c int, desc bool) int {
	if desc {
		return -c
	}
	return c
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (r breakdownRow) value(key string) (float64, bool) {
	opt := func(p *float64) (float64, bool) {
		if p == nil {
			return 0, false
		}
		return *p, true
	}
	switch key {
	case "runs":
		return float64(r.Runs), true
	case "jobs":
		return float64(r.Jobs), true
	case "minutes":
		return float64(r.Minutes), true
	case "spend":
		return r.Spend, true
	case "success":
		return opt(r.Success)
	case "failure":
		return opt(r.Failure)
	case "p50":
		return opt(r.P50)
	case "p95":
		return opt(r.P95)
	case "queue":
		return opt(r.Queue)
	}
	return 0, false
}

func breakdownColumns(u *url.URL, by breakdownDimension, branch bool, sorted string, desc bool) []breakdownColumn {
	rateLabel, rateTitle := "Success", "Completed with success, out of those that completed other than cancelled or skipped."
	if branch {
		rateLabel, rateTitle = "Failure rate", "Completed other than with success or cancelled or skipped, out of those that completed other than cancelled or skipped."
	}
	runsTitle := "Run attempts that started in the window."
	if by.Key == store.BreakdownByJob {
		runsTitle = "Run attempts that ran this job. A run counts under each of its jobs. Success and duration are the job's own."
	}
	cols := []breakdownColumn{
		{Key: "name", Label: by.Label},
		{Key: "runs", Label: "Runs", Title: runsTitle, Right: true},
		{Key: "jobs", Label: "Jobs", Right: true},
		{Key: "minutes", Label: "Minutes", Title: "Job minutes, each job rounded up to a minute.", Right: true},
		{Key: "spend", Label: "Spend", Title: "Job minutes times the runner label's rate.", Right: true},
		{Key: breakdownColumnKeys(branch)[5], Label: rateLabel, Title: rateTitle, Right: true},
		{Key: "p50", Label: "p50", Title: "Median duration, from a run's first job start to its last job end.", Right: true},
		{Key: "p95", Label: "p95", Title: "95th percentile duration.", Right: true},
		{Key: "queue", Label: "Queue p95", Title: "95th percentile of a job's start minus its creation.", Right: true},
	}
	if by.Key == store.BreakdownByJob {
		cols[6].Title = "Median duration of the job."
		cols[7].Title = "95th percentile duration of the job."
	}
	for i, c := range cols {
		next := dirName(c.Key != "name")
		if c.Key == sorted {
			cols[i].Sorted = dirName(desc)
			next = dirName(!desc)
		}
		cols[i].Href = withQuery(u, "sort", c.Key, "dir", next)
	}
	return cols
}

// withQuery is u's path with the query's parameters set to the given
// key and value pairs.
func withQuery(u *url.URL, kv ...string) string {
	q := u.Query()
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	c := *u
	c.RawQuery = q.Encode()
	return c.RequestURI()
}

// shareOfMinutes splits the minutes among the groups with the most, the
// rest folded into "Other".
func shareOfMinutes(rows []breakdownRow) ([]chart.ShareSegment, []chart.LegendItem) {
	ranked := slices.Clone(rows)
	slices.SortStableFunc(ranked, func(a, b breakdownRow) int { return int(b.Minutes - a.Minutes) })
	var segments []chart.ShareSegment
	var legend []chart.LegendItem
	var other int64
	for i, row := range ranked {
		if row.Minutes == 0 {
			break
		}
		if i >= shareSegments {
			other += row.Minutes
			continue
		}
		label := cmpOr(row.Name, "(none)")
		color := chart.SeriesColors[i%len(chart.SeriesColors)]
		segments = append(segments, chart.ShareSegment{Key: label, Label: label, Value: float64(row.Minutes), Color: color})
		legend = append(legend, chart.LegendItem{Key: label, Label: label, Color: color, Value: chart.Integer(float64(row.Minutes))})
	}
	if other > 0 {
		segments = append(segments, chart.ShareSegment{Key: "other", Label: "Other", Value: float64(other), Color: chart.OtherColor})
		legend = append(legend, chart.LegendItem{Key: "other", Label: "Other", Color: chart.OtherColor, Value: chart.Integer(float64(other))})
	}
	return segments, legend
}

func optSeconds(v *float64) string {
	if v == nil {
		return "–"
	}
	return chart.Seconds(*v)
}

func optPercent(v *float64) string {
	if v == nil {
		return "–"
	}
	return chart.Percent(*v)
}

func (r breakdownRow) spendText() string {
	if r.Minutes > 0 && r.Unpriced == r.Minutes {
		return "–"
	}
	return chart.USD(r.Spend)
}

func (r breakdownRow) spendTitle() string {
	if r.Unpriced > 0 {
		return chart.Integer(float64(r.Unpriced)) + " min on unpriced labels"
	}
	return ""
}

func raw(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
