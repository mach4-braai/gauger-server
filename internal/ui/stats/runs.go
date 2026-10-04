package stats

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

// runsColumns are the table's columns. A column with a Key sorts; Desc is
// the direction its first click sorts in.
var runsColumns = []struct {
	Key, Label string
	Num, Desc  bool
}{
	{"started", "Started (UTC)", false, true},
	{"repo", "Repository", false, false},
	{"workflow", "Workflow", false, false},
	{"job", "Job", false, false},
	{"", "Run", false, false},
	{"", "Commit", false, false},
	{"branch", "Branch", false, false},
	{"event", "Event", false, false},
	{"outcome", "Result", false, false},
	{"duration", "Duration", true, true},
	{"queue", "Queue", true, true},
	{"samples", "Samples", true, true},
}

type runsColumn struct {
	Label  string
	Align  string
	Href   string
	Active bool
	Arrow  string
}

type runsChoice struct{ Key, Label string }

type runsChip struct{ Label, Clear string }

type runsHidden struct{ Name, Value string }

type runsRow struct {
	Href           string
	Started        string
	Repository     string
	Workflow       string
	WorkflowURL    string
	WorkflowFilter string
	Job            string
	Run            string
	RunURL         string
	Attempt        int
	Commit         string
	CommitURL      string
	Branch         string
	BranchURL      string
	BranchFilter   string
	Event          string
	Outcome        string
	Result         string
	Duration       string
	Queue          string
	Samples        string
	FromArtifact   bool
}

type runsView struct {
	Description string
	NoRepos     bool
	Step        chart.Step
	Chart       chart.TimeProps
	Legend      []chart.LegendItem
	Search      string
	Outcome     string
	Outcomes    []runsChoice
	Carry       []runsHidden
	Chips       []runsChip
	Columns     []runsColumn
	Rows        []runsRow
	Showing     string
	Prev, Next  string
}

func (s *Server) runs(r *http.Request, f store.Filter) (templ.Component, error) {
	ctx := r.Context()
	u := r.URL
	q := u.Query()
	st := step(f)

	repos, err := s.Store.Repositories(ctx)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return runsPage(runsView{Description: "Jobs in " + describe(f) + ".", NoRepos: true}), nil
	}

	rq := store.RunsQuery{
		Workflow: q.Get("workflow"),
		Branch:   q.Get("branch"),
		Search:   strings.TrimSpace(q.Get("q")),
	}
	outcome := ""
	for _, c := range conclusions {
		if c.key == q.Get("outcome") {
			outcome = c.key
		}
	}
	rq.Outcome = outcome
	if t, err := time.Parse(time.RFC3339, q.Get("bucket")); err == nil {
		rq.BucketStart = st.Trunc(t)
		rq.BucketEnd = rq.BucketStart.Add(time.Hour)
		if st == chart.Day {
			rq.BucketEnd = rq.BucketStart.AddDate(0, 0, 1)
		}
	}
	sortKey := q.Get("sort")
	if !store.IsRunsSort(sortKey) {
		sortKey = "started"
	}
	rq.Sort = sortKey
	rq.Desc = defaultDesc(sortKey)
	switch q.Get("dir") {
	case "asc":
		rq.Desc = false
	case "desc":
		rq.Desc = true
	}
	if n, err := strconv.Atoi(q.Get("page")); err == nil && n > 1 {
		rq.Offset = (n - 1) * store.RunsPageSize
	}

	counts, err := s.Store.RunsByOutcome(ctx, f, rq, string(st))
	if err != nil {
		return nil, err
	}
	page, err := s.Store.Runs(ctx, f, rq)
	if err != nil {
		return nil, err
	}

	var first time.Time
	if len(counts) > 0 {
		first = counts[0].Bucket
	}
	buckets := axis(f, st, first)
	jobs := chart.TimeProps{
		Props: chart.Props{
			ID:     "jobs",
			Hidden: chart.Hidden(q, "jobs"),
			Empty:  "No jobs in this range",
		},
		Buckets: buckets,
		Step:    st,
		SelectBucket: func(start time.Time) string {
			if start.Equal(rq.BucketStart) {
				return with(u, "bucket", "", "page", "")
			}
			return with(u, "bucket", start.Format(time.RFC3339), "page", "")
		},
	}
	var sums []float64
	for _, c := range conclusions {
		values := chart.Densify(buckets, counts,
			func(n store.OutcomeCount) time.Time { return n.Bucket },
			func(n store.OutcomeCount) float64 {
				if n.Outcome != c.key {
					return 0
				}
				return float64(n.Jobs)
			})
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		if sum > 0 {
			jobs.Series = append(jobs.Series, chart.Series{Key: c.key, Label: c.label, Color: c.color, Values: values})
			sums = append(sums, sum)
		}
	}
	legend := chart.Toggles(jobs.Props, u)
	for i, sum := range sums {
		legend[i].Value = chart.Integer(sum)
	}

	v := runsView{
		Description: "Jobs in " + describe(f) + ".",
		Step:        st,
		Chart:       jobs,
		Legend:      legend,
		Search:      rq.Search,
		Outcome:     outcome,
		Outcomes:    []runsChoice{{"", "All"}},
	}
	for _, c := range conclusions {
		v.Outcomes = append(v.Outcomes, runsChoice{c.key, c.label})
	}
	for _, c := range []struct{ name, value string }{
		{"workflow", rq.Workflow}, {"branch", rq.Branch}, {"bucket", q.Get("bucket")}, {"sort", q.Get("sort")}, {"dir", q.Get("dir")},
	} {
		if c.value != "" {
			v.Carry = append(v.Carry, runsHidden{c.name, c.value})
		}
	}
	if rq.Workflow != "" {
		v.Chips = append(v.Chips, runsChip{"Workflow: " + rq.Workflow, with(u, "workflow", "", "page", "")})
	}
	if rq.Branch != "" {
		v.Chips = append(v.Chips, runsChip{"Branch: " + rq.Branch, with(u, "branch", "", "page", "")})
	}
	if !rq.BucketStart.IsZero() {
		label := rq.BucketStart.Format("Jan 2")
		if st == chart.Hour {
			label = rq.BucketStart.Format("Jan 2 15:04")
		}
		v.Chips = append(v.Chips, runsChip{"Bucket: " + label + " UTC", with(u, "bucket", "", "page", "")})
	}

	for _, c := range runsColumns {
		col := runsColumn{Label: c.Label}
		if c.Num {
			col.Align = "right"
		}
		if c.Key != "" {
			col.Active = c.Key == rq.Sort
			desc := c.Desc
			if col.Active {
				desc = !rq.Desc
				col.Arrow = "↑"
				if rq.Desc {
					col.Arrow = "↓"
				}
			}
			dir := "asc"
			if desc {
				dir = "desc"
			}
			col.Href = with(u, "sort", c.Key, "dir", dir, "page", "")
		}
		v.Columns = append(v.Columns, col)
	}

	for _, row := range page.Rows {
		v.Rows = append(v.Rows, s.runsRow(u, row))
	}
	if page.Total > 0 {
		v.Showing = fmt.Sprintf("%s–%s of %s jobs",
			chart.Integer(float64(page.Offset+1)), chart.Integer(float64(page.Offset+len(page.Rows))), chart.Integer(float64(page.Total)))
	}
	if n := page.Offset / store.RunsPageSize; n > 0 {
		v.Prev = with(u, "page", pageParam(n))
	}
	if page.Offset+len(page.Rows) < page.Total {
		v.Next = with(u, "page", pageParam(page.Offset/store.RunsPageSize+2))
	}
	return runsPage(v), nil
}

func defaultDesc(key string) bool {
	for _, c := range runsColumns {
		if c.Key == key {
			return c.Desc
		}
	}
	return true
}

// pageParam is the page query value for the 1-based page n; the first page
// has none.
func pageParam(n int) string {
	if n <= 1 {
		return ""
	}
	return strconv.Itoa(n)
}

func (s *Server) runsRow(u *url.URL, row store.RunRow) runsRow {
	out := runsRow{
		Href:         "/stats/jobs/" + strconv.FormatInt(row.ID, 10),
		Repository:   row.Repository,
		Workflow:     row.Workflow,
		WorkflowURL:  github.WorkflowURL(s.GitHubURL, row.Repository, row.Path),
		Job:          cmpOr(row.Name, strconv.FormatInt(row.ID, 10)),
		Run:          strconv.FormatInt(row.RunID, 10),
		RunURL:       github.RunURL(s.GitHubURL, row.Repository, row.RunID, row.RunAttempt),
		Attempt:      row.RunAttempt,
		Branch:       row.Branch,
		Event:        row.Event,
		Outcome:      row.Outcome,
		Result:       cmpOr(row.Conclusion, row.Status),
		Started:      "–",
		Queue:        "–",
		Duration:     "–",
		Samples:      chart.Integer(float64(row.Samples)),
		FromArtifact: row.FromArtifact,
	}
	if row.Outcome == store.OutcomeRunning {
		out.Result = row.Status
	}
	if row.StartedAt != nil {
		out.Started = row.StartedAt.UTC().Format("2006-01-02 15:04")
		if row.CompletedAt != nil {
			out.Duration = chart.Seconds(row.CompletedAt.Sub(*row.StartedAt).Seconds())
		}
	}
	if row.Queue != nil {
		out.Queue = chart.Seconds(*row.Queue)
	}
	if row.Workflow != "" {
		out.WorkflowFilter = with(u, "workflow", row.Workflow, "page", "")
	}
	if row.HeadSHA != "" {
		out.Commit = row.HeadSHA[:min(7, len(row.HeadSHA))]
		out.CommitURL = github.CommitURL(s.GitHubURL, row.Repository, row.HeadSHA)
	}
	if row.Branch != "" {
		out.BranchURL = github.BranchURL(s.GitHubURL, row.Repository, row.Branch)
		out.BranchFilter = with(u, "branch", row.Branch, "page", "")
	}
	return out
}

// with is u's path and query with each key of pairs set to the value after
// it, or dropped when the value is empty.
func with(u *url.URL, pairs ...string) string {
	q := u.Query()
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			q.Del(pairs[i])
		} else {
			q.Set(pairs[i], pairs[i+1])
		}
	}
	if len(q) == 0 {
		return u.Path
	}
	return u.Path + "?" + q.Encode()
}

func (v runsView) bucketLabel() string {
	if v.Step == chart.Hour {
		return "Per hour, UTC"
	}
	return "Per day, UTC"
}

// runsTone colours a result badge.
func runsTone(outcome string) string {
	switch outcome {
	case store.OutcomeSuccess:
		return "ok"
	case store.OutcomeFailure:
		return "bad"
	case store.OutcomeOther:
		return "warn"
	case store.OutcomeRunning:
		return "accent"
	}
	return ""
}
