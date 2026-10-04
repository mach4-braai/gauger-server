package storetest

import "time"

// actionRefs adds a run whose jobs pin the same actions to different refs
// and one failing job, so step names have refs to strip and a step has a
// failure to count.
func (s *seeder) actionRefs() {
	run := s.run("acme/api", ci, "push", s.now.Add(-2*time.Hour), "")
	for _, ref := range []string{"aaa", "bbb"} {
		j := s.job(run, ci.jobs[0], "success")
		s.renameStep(j, 2, "Run actions/checkout@"+ref)
		s.renameStep(j, 3, "Run jdx/mise-action@"+ref)
	}
	s.job(run, ci.jobs[1], "failure")
	s.finish(run)
}

// renameStep gives step number of job j a new name.
func (s *seeder) renameStep(j *seedJob, number int, name string) {
	for i := len(s.steps) - 1; i >= 0; i-- {
		if s.steps[i].job == j.id && s.steps[i].number == number {
			s.steps[i].name = name
			return
		}
	}
}
