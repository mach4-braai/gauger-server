package storetest

import "time"

// UnratedLabel is a runner label no default rate covers.
const UnratedLabel = "ubuntu-latest-8-cores"

var loadTest = seedWorkflow{"Load test", ".github/workflows/load-test.yml", []string{"workflow_dispatch"}, []seedJobDef{
	{"load", []string{UnratedLabel}, 6 * time.Minute},
}}

// spend adds a job on a larger runner whose label has no default rate.
func (s *seeder) spend() {
	run := s.run("acme/api", loadTest, "workflow_dispatch", s.now.Add(-36*time.Hour), "")
	s.fx.UnratedJob = s.job(run, loadTest.jobs[0], "success").id
	s.finish(run)
}
