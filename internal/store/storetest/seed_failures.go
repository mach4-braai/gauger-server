package storetest

import "time"

// failureCases adds failing jobs with known causes. "migrate" fails in its
// checkout step and again in its main step, so its first failed step is
// number 2. "backfill" fails with no failed step. A cancelled run sits in
// the same hour, so cancellations have a bucket of their own.
func (s *seeder) failureCases() {
	wf := seedWorkflow{"Migrations", ".github/workflows/migrations.yml", []string{"push"}, nil}
	at := s.now.Add(-90 * time.Minute)

	failed := s.run("acme/web", wf, "push", at, "")
	migrate := s.job(failed, seedJobDef{"migrate", ubuntu, 2 * time.Minute}, "failure")
	s.failureStep(migrate, 2, "failure")
	backfill := s.job(failed, seedJobDef{"backfill", ubuntu, 2 * time.Minute}, "failure")
	s.failureStep(backfill, 4, "success")
	s.finish(failed)

	cancelled := s.run("acme/web", wf, "push", at.Add(5*time.Minute), "")
	s.job(cancelled, seedJobDef{"migrate", ubuntu, 2 * time.Minute}, "cancelled")
	s.finish(cancelled)
}

// failureStep gives step number of job j a new conclusion.
func (s *seeder) failureStep(j *seedJob, number int, conclusion string) {
	for i := len(s.steps) - 1; i >= 0; i-- {
		if s.steps[i].job == j.id && s.steps[i].number == number {
			s.steps[i].conclusion = conclusion
			return
		}
	}
}
