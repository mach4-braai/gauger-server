package storetest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mach4-braai/gauger-server/internal/runner"
	"github.com/mach4-braai/gauger-server/internal/store"
)

// Fixtures names the seeded rows tests look up. Every seeded time is
// before Now.
type Fixtures struct {
	Now          time.Time
	Repositories []string
	// ReRun is a run whose first attempt failed and second passed.
	ReRun int64
	// FlakySHA has a failing run and then a passing one.
	FlakySHA string
	// SampledJob is a job gauger reported live, with samples.
	SampledJob int64
	// ArtifactJob is a job whose samples came from the fallback artifact.
	ArtifactJob int64
	// UnsampledJob is a completed job with no gauger data.
	UnsampledJob int64
	// ArtifactOnlyJob is a job gauger never reached, whose samples came
	// from the fallback artifact alone.
	ArtifactOnlyJob int64
	// PRWorkflows name the runs of the dynamic CodeQL workflow in acme/api
	// that are named after their pull requests.
	PRWorkflows []string
	// UnratedJob is a completed job on a runner label with no known rate.
	UnratedJob int64
}

// Seed fills st with the dashboard fixtures, anchored at the current time.
func Seed(t testing.TB, st *store.Store) Fixtures {
	t.Helper()
	fx, err := Fill(context.Background(), st, time.Now())
	if err != nil {
		t.Fatalf("seed fixtures: %v", err)
	}
	return fx
}

// Fill inserts the fixtures with every time before now. The same seed
// gives the same rows relative to now. A page's extra fixtures go in a
// seeder method in seed_<page>.go, called once below.
func Fill(ctx context.Context, st *store.Store, now time.Time) (Fixtures, error) {
	s := newSeeder(now)
	s.history()
	s.jobPage()
	s.trends()
	s.actionRefs()
	s.spend()
	return s.fx, s.insert(ctx, st)
}

type seedRepo struct {
	name      string
	private   bool
	workflows []seedWorkflow
}

type seedWorkflow struct {
	name, path string
	events     []string
	jobs       []seedJobDef
}

type seedJobDef struct {
	name   string
	labels []string
	// work is the typical length of the job's main step.
	work time.Duration
}

var (
	ubuntu = []string{"ubuntu-latest"}
	macos  = []string{"macos-latest"}
	arm    = []string{"ubuntu-24.04-arm"}
	hosted = []string{"self-hosted", "linux"}

	ci = seedWorkflow{"CI", ".github/workflows/ci.yml", []string{"push", "pull_request", "pull_request"}, []seedJobDef{
		{"lint", ubuntu, 70 * time.Second},
		{"test", ubuntu, 5 * time.Minute},
		{"build (macos)", macos, 7 * time.Minute},
	}}
	deploy = seedWorkflow{"Deploy", ".github/workflows/deploy.yml", []string{"workflow_dispatch", "release"}, []seedJobDef{
		{"deploy", hosted, 3 * time.Minute},
	}}
	nightly = seedWorkflow{"Nightly", ".github/workflows/nightly.yml", []string{"schedule"}, []seedJobDef{
		{"e2e", arm, 12 * time.Minute},
	}}
	codeql = seedWorkflow{"CodeQL", "dynamic/github-code-scanning/codeql", []string{"dynamic"}, []seedJobDef{
		{"Analyze (go)", ubuntu, 4 * time.Minute},
	}}

	seedRepos = []seedRepo{
		{"acme/api", true, []seedWorkflow{ci, ci, ci, deploy, codeql}},
		{"acme/web", true, []seedWorkflow{ci, ci, nightly}},
		{"oss/gauger", false, []seedWorkflow{ci, ci, codeql}},
		{"oss/docs", false, []seedWorkflow{ci}},
	}
)

type seedRun struct {
	id         int64
	attempt    int
	repo       string
	workflow   seedWorkflow
	branch     string
	sha        string
	event      string
	status     string
	conclusion string
	created    time.Time
	jobs       []*seedJob
}

type seedJob struct {
	id               int64
	run              *seedRun
	name             string
	labels           []string
	status           string
	conclusion       string
	created          time.Time
	started          *time.Time
	completed        *time.Time
	runnerSeen       *time.Time
	runnerDone       *time.Time
	artifactIngested *time.Time
}

type seedStep struct {
	job        int64
	number     int
	name       string
	status     string
	conclusion string
	started    *time.Time
	completed  *time.Time
}

type seeder struct {
	now     time.Time
	rng     *rand.Rand
	runID   int64
	jobID   int64
	fx      Fixtures
	runs    []*seedRun
	jobs    []*seedJob
	steps   []seedStep
	samples map[int64][]runner.Point
}

func newSeeder(now time.Time) *seeder {
	now = now.UTC().Truncate(time.Second)
	s := &seeder{
		now:     now,
		rng:     rand.New(rand.NewPCG(49, 2026)),
		runID:   1_000_000,
		jobID:   50_000_000,
		fx:      Fixtures{Now: now},
		samples: map[int64][]runner.Point{},
	}
	for _, r := range seedRepos {
		s.fx.Repositories = append(s.fx.Repositories, r.name)
	}
	return s
}

// history adds 90 days of runs across every repository, plus the named
// fixtures: a re-run, a flaky SHA, gauger-reported and fallback-artifact
// jobs, and runs still in progress.
func (s *seeder) history() {
	for day := 89; day >= 0; day-- {
		for _, repo := range seedRepos {
			for range s.rng.IntN(4) {
				at := s.now.Add(-time.Duration(day)*24*time.Hour - time.Duration(2*60+s.rng.IntN(20*60))*time.Minute)
				wf := repo.workflows[s.rng.IntN(len(repo.workflows))]
				run := s.run(repo.name, wf, wf.events[s.rng.IntN(len(wf.events))], at, "")
				for _, def := range wf.jobs {
					j := s.job(run, def, s.conclusion())
					if repo.name == "oss/gauger" && day < 7 && j.name == "test" && j.conclusion == "success" {
						s.gauger(j, false)
					}
				}
				s.finish(run)
			}
		}
	}

	at := s.now.Add(-3 * 24 * time.Hour)
	first := s.run("acme/api", ci, "push", at, "")
	s.job(first, ci.jobs[0], "success")
	s.job(first, ci.jobs[1], "failure")
	s.finish(first)
	second := s.rerun(first, at.Add(40*time.Minute))
	s.job(second, ci.jobs[0], "success")
	s.job(second, ci.jobs[1], "success")
	s.finish(second)
	s.fx.ReRun = first.id

	at = s.now.Add(-5 * 24 * time.Hour)
	failing := s.run("oss/gauger", ci, "pull_request", at, "")
	s.job(failing, ci.jobs[1], "failure")
	s.finish(failing)
	passing := s.run("oss/gauger", ci, "pull_request", at.Add(25*time.Minute), failing.sha)
	passing.branch = failing.branch
	s.fx.UnsampledJob = s.job(passing, ci.jobs[0], "success").id
	s.job(passing, ci.jobs[1], "success")
	s.finish(passing)
	s.fx.FlakySHA = failing.sha

	at = s.now.Add(-26 * time.Hour)
	live := s.run("oss/gauger", ci, "push", at, "")
	s.fx.SampledJob = s.gauger(s.job(live, ci.jobs[1], "success"), false).id
	s.fx.ArtifactJob = s.gauger(s.job(live, ci.jobs[0], "success"), true).id
	s.finish(live)

	running := s.run("acme/web", ci, "pull_request", s.now.Add(-10*time.Minute), "")
	s.job(running, ci.jobs[0], "success")
	s.job(running, ci.jobs[1], "")
	s.queued(running, ci.jobs[2])
	s.finish(running)
}

// conclusion picks a job outcome: mostly success, some failures and
// cancellations, a few skips.
func (s *seeder) conclusion() string {
	switch n := s.rng.IntN(100); {
	case n < 82:
		return "success"
	case n < 91:
		return "failure"
	case n < 97:
		return "cancelled"
	default:
		return "skipped"
	}
}

// run adds a first run attempt created at the given time. An empty sha
// picks a new one.
func (s *seeder) run(repo string, wf seedWorkflow, event string, created time.Time, sha string) *seedRun {
	s.runID++
	if sha == "" {
		sha = fmt.Sprintf("%016x%016x%08x", s.rng.Uint64(), s.rng.Uint64(), s.rng.Uint32())
	}
	branch := "main"
	if event == "pull_request" {
		branch = fmt.Sprintf("feature-%d", s.rng.IntN(40))
	}
	r := &seedRun{id: s.runID, attempt: 1, repo: repo, workflow: wf, branch: branch, sha: sha, event: event, created: created}
	s.runs = append(s.runs, r)
	return r
}

// rerun adds the next attempt of run r, created at the given time.
func (s *seeder) rerun(r *seedRun, created time.Time) *seedRun {
	next := &seedRun{
		id: r.id, attempt: r.attempt + 1, repo: r.repo, workflow: r.workflow, branch: r.branch,
		sha: r.sha, event: r.event, created: created,
	}
	s.runs = append(s.runs, next)
	return next
}

// queued adds a job of run r that has not started.
func (s *seeder) queued(r *seedRun, def seedJobDef) *seedJob {
	s.jobID++
	j := &seedJob{id: s.jobID, run: r, name: def.name, labels: def.labels, status: "queued", created: r.created}
	s.jobs = append(s.jobs, j)
	r.jobs = append(r.jobs, j)
	return j
}

// job adds a job of run r with the steps a mise-based workflow has. An
// empty conclusion leaves it in progress in its main step. Skipped jobs
// have no times or steps.
func (s *seeder) job(r *seedRun, def seedJobDef, conclusion string) *seedJob {
	s.jobID++
	queue := time.Duration(1+s.rng.IntN(20)) * time.Second
	if def.labels[0] == "macos-latest" {
		queue += time.Duration(s.rng.IntN(60)) * time.Second
	}
	j := &seedJob{
		id: s.jobID, run: r, name: def.name, labels: def.labels,
		status: "completed", conclusion: conclusion, created: r.created,
	}
	s.jobs = append(s.jobs, j)
	r.jobs = append(r.jobs, j)
	if conclusion == "skipped" {
		return j
	}
	start := r.created.Add(queue)
	j.started = &start
	work := def.work/2 + time.Duration(s.rng.Int64N(int64(def.work)))
	steps := []struct {
		name string
		d    time.Duration
	}{
		{"Set up job", 2 * time.Second},
		{"Run actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", 3 * time.Second},
		{"Run jdx/mise-action@7e36c90d9ab29c415a2384db3006f3ec8a8cc654", 15 * time.Second},
		{"Run mise run " + def.name, work},
		{"Complete job", time.Second},
	}
	main := 3
	t := start
	for i, st := range steps {
		step := seedStep{job: j.id, number: i + 1, name: st.name, status: "completed", conclusion: "success"}
		begin, end := t, t.Add(st.d)
		switch {
		case conclusion == "" && i == main:
			step.status, step.conclusion = "in_progress", ""
			step.started = &begin
		case conclusion == "" && i > main:
			step.status, step.conclusion = "queued", ""
		case i == main && conclusion != "success":
			step.conclusion = conclusion
			step.started, step.completed = &begin, &end
		default:
			step.started, step.completed = &begin, &end
		}
		s.steps = append(s.steps, step)
		t = end
	}
	if conclusion == "" {
		j.status = "in_progress"
		return j
	}
	j.completed = &t
	return j
}

// finish sets a run's status and conclusion from its jobs.
func (s *seeder) finish(r *seedRun) {
	r.status, r.conclusion = "completed", "success"
	for _, j := range r.jobs {
		switch {
		case j.status != "completed":
			r.status, r.conclusion = "in_progress", ""
			return
		case j.conclusion == "failure":
			r.conclusion = "failure"
		case j.conclusion == "cancelled" && r.conclusion != "failure":
			r.conclusion = "cancelled"
		}
	}
}

const gib = float64(1 << 30)

// gauger gives a completed job runner samples every 10 seconds from start
// to end, as gauger sends them. fromArtifact marks them as read from the
// fallback artifact instead of reported live.
func (s *seeder) gauger(j *seedJob, fromArtifact bool) *seedJob {
	seen := *j.started
	j.runnerSeen = &seen
	if fromArtifact {
		ingested := j.completed.Add(3 * time.Minute)
		j.artifactIngested = &ingested
	} else {
		done := *j.completed
		j.runnerDone = &done
	}
	var points []runner.Point
	add := func(t time.Time, metric, series string, v float64) {
		points = append(points, runner.Point{Metric: metric, Series: series, Time: t, Value: v})
	}
	var read, written, rx, tx float64
	for t := *j.started; !t.After(*j.completed); t = t.Add(10 * time.Second) {
		busy := 0.2 + 0.75*s.rng.Float64()
		modes := map[string]float64{
			"user": busy * 0.7, "system": busy * 0.2, "iowait": busy * 0.06, "steal": busy * 0.02,
			"nice": 0, "interrupt": busy * 0.02, "idle": 1 - busy,
		}
		add(t, store.MetricCPUUtilization, "", busy)
		for mode, v := range modes {
			add(t, store.MetricCPUUtilization, "cpu.mode="+mode, v)
		}
		used := (2 + 8*s.rng.Float64()) * gib
		add(t, store.MetricMemoryUsage, store.SeriesMemoryUsed, used)
		add(t, store.MetricMemoryUsage, "system.memory.state=cached", 3*gib)
		add(t, store.MetricMemoryUsage, "system.memory.state=buffers", 0.25*gib)
		add(t, store.MetricMemoryUsage, "system.memory.state=free", 16*gib-used-3.25*gib)
		add(t, "system.linux.memory.available", "", 16*gib-used)
		add(t, store.MetricMemoryLimit, "", 16*gib)
		add(t, store.MetricCPUCount, "", 4)
		read += float64(s.rng.IntN(50 << 20))
		written += float64(s.rng.IntN(20 << 20))
		rx += float64(s.rng.IntN(30 << 20))
		tx += float64(s.rng.IntN(2 << 20))
		add(t, "system.disk.io", "disk.io.direction=read,system.device=nvme0n1", read)
		add(t, "system.disk.io", "disk.io.direction=write,system.device=nvme0n1", written)
		add(t, "system.network.io", "network.interface.name=eth0,network.io.direction=receive", rx)
		add(t, "system.network.io", "network.interface.name=eth0,network.io.direction=transmit", tx)
		add(t, "system.network.io", "network.interface.name=lo,network.io.direction=receive", rx/10)
		add(t, "system.network.io", "network.interface.name=lo,network.io.direction=transmit", rx/10)
	}
	s.samples[j.id] = points
	return j
}

func (s *seeder) insert(ctx context.Context, st *store.Store) error {
	err := st.InTx(ctx, func(tx pgx.Tx) error {
		var rows [][]any
		for i, r := range seedRepos {
			rows = append(rows, []any{r.name, r.private, int64(100 + i)})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"repositories"}, []string{"full_name", "private", "installation_id"}, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("repositories: %w", err)
		}

		rows = rows[:0]
		for _, r := range s.runs {
			rows = append(rows, []any{
				r.id, r.attempt, r.repo, r.workflow.name, r.workflow.path, r.branch, r.sha, r.event,
				r.status, nullIfEmpty(r.conclusion), fmt.Sprintf("https://github.com/%s/actions/runs/%d", r.repo, r.id),
				r.created, r.created, r.created,
			})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"runs"}, []string{
			"id", "attempt", "repository", "workflow_name", "path", "head_branch", "head_sha", "event",
			"status", "conclusion", "html_url", "created_at", "run_started_at", "updated_at",
		}, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("runs: %w", err)
		}

		rows = rows[:0]
		for _, j := range s.jobs {
			rows = append(rows, []any{
				j.id, j.run.id, j.run.attempt, j.run.repo, j.run.workflow.name, j.name, j.run.branch,
				j.status, nullIfEmpty(j.conclusion), j.labels, fmt.Sprintf("runner-%d", j.id%7), "Default",
				fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", j.run.repo, j.run.id, j.id),
				j.created, j.started, j.completed, j.runnerSeen, j.runnerDone, j.artifactIngested,
			})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"jobs"}, []string{
			"id", "run_id", "run_attempt", "repository", "workflow_name", "name", "head_branch",
			"status", "conclusion", "labels", "runner_name", "runner_group_name",
			"html_url", "created_at", "started_at", "completed_at", "runner_seen_at", "runner_done_at", "artifact_ingested_at",
		}, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("jobs: %w", err)
		}

		rows = rows[:0]
		for _, st := range s.steps {
			rows = append(rows, []any{st.job, st.number, st.name, st.status, nullIfEmpty(st.conclusion), st.started, st.completed})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"steps"}, []string{
			"job_id", "number", "name", "status", "conclusion", "started_at", "completed_at",
		}, pgx.CopyFromRows(rows)); err != nil {
			return fmt.Errorf("steps: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for id, points := range s.samples {
		rejected, err := st.InsertSamples(ctx, id, points)
		if err != nil {
			return fmt.Errorf("samples for job %d: %w", id, err)
		}
		if rejected > 0 {
			return fmt.Errorf("samples for job %d: %d points outside retention", id, rejected)
		}
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
