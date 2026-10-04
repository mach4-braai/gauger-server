package storetest

import "time"

// jobPage adds a job gauger never reached: its samples came from the
// fallback artifact alone, so runner_seen_at is empty.
func (s *seeder) jobPage() {
	run := s.run("oss/gauger", ci, "push", s.now.Add(-30*time.Hour), "")
	j := s.gauger(s.job(run, ci.jobs[1], "success"), true)
	j.runnerSeen = nil
	s.finish(run)
	s.fx.ArtifactOnlyJob = j.id
}
