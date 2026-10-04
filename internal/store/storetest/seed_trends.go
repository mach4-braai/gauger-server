package storetest

import "time"

var trendBench = seedWorkflow{"Bench", ".github/workflows/bench.yml", []string{"push"}, []seedJobDef{
	{"bench", ubuntu, time.Minute},
}}

// trends adds what the Trends page tests need: two runs of the dynamic
// CodeQL workflow in acme/api, each named after its pull request, and a
// bench step that took a minute for 15 days and 2.5 minutes yesterday.
func (s *seeder) trends() {
	day := s.now.Truncate(24 * time.Hour)

	for _, pr := range []string{"PR #41", "PR #42"} {
		wf := codeql
		wf.name = pr
		run := s.run("acme/api", wf, "pull_request", day.AddDate(0, 0, -2).Add(3*time.Hour), "")
		s.job(run, wf.jobs[0], "success")
		s.finish(run)
		s.fx.PRWorkflows = append(s.fx.PRWorkflows, pr)
	}

	for ago := 16; ago >= 1; ago-- {
		work := time.Minute
		if ago == 1 {
			work = 150 * time.Second
		}
		for n := range 2 {
			run := s.run("acme/api", trendBench, "push", day.AddDate(0, 0, -ago).Add(time.Hour+time.Duration(n)*30*time.Minute), "")
			s.timed(run, trendBench.jobs[0], work)
			s.finish(run)
		}
	}
}

// timed adds a successful job whose main step lasts work.
func (s *seeder) timed(r *seedRun, def seedJobDef, work time.Duration) *seedJob {
	j := s.job(r, def, "success")
	main, last := &s.steps[len(s.steps)-2], &s.steps[len(s.steps)-1]
	end := main.started.Add(work)
	done := end.Add(time.Second)
	main.completed = &end
	last.started, last.completed = &end, &done
	j.completed = &done
	return j
}
