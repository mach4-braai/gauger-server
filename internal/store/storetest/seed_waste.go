package storetest

import "time"

// waste adds a private-repository run whose test job fails on attempt 1
// and fails again on attempt 2, next to a cancelled macOS job. The SHA is
// a re-run that never passed, so it is not a flaky candidate.
func (s *seeder) waste() {
	at := s.now.Add(-2 * 24 * time.Hour)
	first := s.run("acme/web", ci, "push", at, "")
	s.job(first, ci.jobs[0], "success")
	s.job(first, ci.jobs[1], "failure")
	s.job(first, ci.jobs[2], "cancelled")
	s.finish(first)
	second := s.rerun(first, at.Add(30*time.Minute))
	s.job(second, ci.jobs[1], "failure")
	s.finish(second)
	s.fx.FailedTwiceSHA = first.sha
}
