package stats

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"

	"github.com/a-h/templ"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
)

const (
	// sizingLimit is the share of a runner's CPU or memory above which a
	// job counts as using it.
	sizingLimit = 0.5
	// sizingJobRows caps the per-job table; candidates sort first.
	sizingJobRows = 200
)

type sizingView struct {
	Description string
	Tiles       []tile
	Labels      []sizingLabelRow
	Jobs        []sizingJobRow
	JobsShown   string
	Steps       []sizingStepRow
}

// sizingLabelRow counts a runner label's sampled jobs.
type sizingLabelRow struct {
	Label                     string
	Jobs, CPU, Memory, Either int64
	Candidates                int64
}

type sizingJobRow struct {
	ID          int64
	Name        string
	Workflow    string
	WorkflowURL string
	Repository  string
	Label       string
	Minutes     string
	Memory      string
	MemoryOf    string
	CPUPeak     string
	CPUP95      string
	Candidate   bool
	Smaller     string
	Saving      string
	SavingWhy   string
}

type sizingStepRow struct {
	Repository  string
	Workflow    string
	WorkflowURL string
	Job         string
	JobID       int64
	Step        string
	StepNumber  int
	StepURL     string
	Runs        string
	PeakMemory  string
	MemoryOf    string
	PeakCPU     string
	Saturated   string
}

// sizingLabel names the runner a job's labels pick: self-hosted, else the
// first label with a rate, else the first label.
func sizingLabel(rates spend.Rates, labels []string) string {
	if slices.Contains(labels, "self-hosted") {
		return "self-hosted"
	}
	if l := rates.Label(labels); l != "" {
		return l
	}
	if len(labels) > 0 {
		return labels[0]
	}
	return "unknown"
}

// sizingUse is a sampled job's use of its runner. A share is nil when the
// job has no samples of it or no total to compare with.
type sizingUse struct {
	memory, cpu *float64
}

func sizingUseOf(j store.SizingJob) sizingUse {
	u := sizingUse{cpu: j.PeakCPU}
	if j.PeakMemory != nil && j.MemTotal != nil && *j.MemTotal > 0 {
		u.memory = new(*j.PeakMemory / *j.MemTotal)
	}
	return u
}

func (u sizingUse) overMemory() bool { return u.memory != nil && *u.memory > sizingLimit }
func (u sizingUse) overCPU() bool    { return u.cpu != nil && *u.cpu > sizingLimit }

// candidate is a job measured on both that never passed the limit on
// either.
func (u sizingUse) candidate() bool {
	return u.memory != nil && u.cpu != nil && !u.overMemory() && !u.overCPU()
}

func (u sizingUse) peak() float64 {
	p := 0.0
	for _, v := range []*float64{u.memory, u.cpu} {
		if v != nil {
			p = max(p, *v)
		}
	}
	return p
}

func (s *Server) sizing(r *http.Request, f store.Filter) (templ.Component, error) {
	ctx := r.Context()
	jobs, total, err := s.Store.SizingJobs(ctx, f)
	if err != nil {
		return nil, err
	}
	steps, err := s.Store.Sizing(ctx, f)
	if err != nil {
		return nil, err
	}

	type judged struct {
		job  store.SizingJob
		use  sizingUse
		down spend.Downsize
		rank int
	}
	byLabel := map[string]*sizingLabelRow{}
	rows := make([]judged, len(jobs))
	var candidates int64
	var saving float64
	for i, j := range jobs {
		u := sizingUseOf(j)
		label := sizingLabel(s.Rates, j.Labels)
		l := byLabel[label]
		if l == nil {
			l = &sizingLabelRow{Label: label}
			byLabel[label] = l
		}
		l.Jobs++
		if u.overCPU() {
			l.CPU++
		}
		if u.overMemory() {
			l.Memory++
		}
		if u.overCPU() || u.overMemory() {
			l.Either++
		}
		rows[i] = judged{job: j, use: u, rank: 1}
		if u.candidate() {
			rows[i].rank = 0
			l.Candidates++
			candidates++
			rows[i].down = s.Rates.Downsize(j.Labels, j.Private, j.Minutes)
			if rows[i].down.Priced {
				saving += rows[i].down.Saving
			}
		}
	}
	slices.SortFunc(rows, func(a, b judged) int {
		return cmp.Or(
			cmp.Compare(a.rank, b.rank),
			-cmp.Compare(a.down.Saving, b.down.Saving),
			-cmp.Compare(a.use.peak(), b.use.peak()),
			-cmp.Compare(a.job.ID, b.job.ID),
		)
	})

	v := sizingView{
		Description: "How much of its runner each job with gauger samples used in " + describe(f) + ", and what a smaller runner would save.",
		Tiles: []tile{
			{
				ID: "coverage", Label: "Sampled jobs", Value: ratio(int64(len(jobs)), total),
				Hint:  fmt.Sprintf("%s of %s jobs", chart.Integer(float64(len(jobs))), chart.Integer(float64(total))),
				Title: "Jobs with runner samples out of all jobs in the window. Only these can be sized.",
			},
			{
				ID: "candidates", Label: "Smaller runner candidates", Value: chart.Integer(float64(candidates)),
				Hint:  "never over 50% of CPU or memory",
				Title: "Sampled jobs whose peak CPU and peak memory both stayed at or under 50% of the runner.",
			},
			{
				ID: "saving", Label: "Estimated saving", Value: chart.USD(saving),
				Hint:  "at GitHub's list prices",
				Title: "Each candidate's minutes at its label's rate, minus the same minutes at the next smaller label. Free minutes and labels with no smaller runner add nothing.",
			},
		},
	}
	for _, l := range byLabel {
		v.Labels = append(v.Labels, *l)
	}
	slices.SortFunc(v.Labels, func(a, b sizingLabelRow) int {
		return cmp.Or(-cmp.Compare(a.Jobs, b.Jobs), cmp.Compare(a.Label, b.Label))
	})

	if len(rows) > sizingJobRows {
		v.JobsShown = fmt.Sprintf("Showing %d of %s jobs, candidates first.", sizingJobRows, chart.Integer(float64(len(rows))))
		rows = rows[:sizingJobRows]
	}
	for _, r := range rows {
		j := r.job
		row := sizingJobRow{
			ID: j.ID, Name: j.Name, Workflow: j.Workflow,
			WorkflowURL: github.WorkflowURL(s.GitHubURL, j.Repository, j.Path),
			Repository:  j.Repository,
			Label:       sizingLabel(s.Rates, j.Labels),
			Minutes:     chart.Integer(float64(j.Minutes)),
			Memory:      sizingGiB(j.PeakMemory),
			MemoryOf:    sizingPct(r.use.memory),
			CPUPeak:     sizingPct(j.PeakCPU),
			CPUP95:      sizingPct(j.P95CPU),
			Candidate:   r.use.candidate(),
			Smaller:     "–",
			Saving:      "–",
		}
		if row.Candidate {
			switch {
			case r.down.Label == "":
				row.SavingWhy = "No smaller runner for this label."
			default:
				row.Smaller = r.down.Label
				if r.down.Priced {
					row.Saving = chart.USD(r.down.Saving)
				} else {
					row.SavingWhy = "These minutes are free."
				}
			}
		}
		v.Jobs = append(v.Jobs, row)
	}

	for _, st := range steps {
		v.Steps = append(v.Steps, sizingStepRow{
			Repository: st.Repository, Workflow: st.Workflow,
			WorkflowURL: github.WorkflowURL(s.GitHubURL, st.Repository, st.Path),
			Job:         st.Job, JobID: st.JobID, Step: st.Step, StepNumber: st.StepNumber,
			StepURL:    github.StepURL(st.JobHTMLURL, st.StepNumber),
			Runs:       chart.Integer(float64(st.Runs)),
			PeakMemory: sizingGiB(st.PeakMemory),
			MemoryOf:   sizingShare(st.PeakMemory, st.MemTotal),
			PeakCPU:    sizingCores(st.PeakCPU, st.CPUCount),
			Saturated:  sizingPct(st.Saturated),
		})
	}
	return sizingPage(v), nil
}

func sizingGiB(v *float64) string {
	if v == nil {
		return "–"
	}
	return fmt.Sprintf("%.2f GiB", *v/(1<<30))
}

func sizingPct(v *float64) string {
	if v == nil {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", *v*100)
}

func sizingShare(num, den *float64) string {
	if num == nil || den == nil || *den == 0 {
		return "–"
	}
	return fmt.Sprintf("%.0f%%", *num / *den * 100)
}

func sizingCores(util, n *float64) string {
	if util == nil || n == nil {
		return "–"
	}
	return fmt.Sprintf("%.1f of %.0f", *util**n, *n)
}
