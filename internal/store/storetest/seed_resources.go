package storetest

import "time"

var (
	gpuJob   = seedJobDef{"train", []string{"self-hosted", "gpu"}, 4 * time.Minute}
	training = seedWorkflow{"Train", ".github/workflows/train.yml", []string{"workflow_dispatch"}, []seedJobDef{gpuJob}}
)

// resources adds the sampled jobs the Resources page compares: one on a
// runner label and a workflow no other job has, so its group is that job
// alone, and one sampled lint job every six days back through the 90, so
// samples fill many daily partitions.
func (s *seeder) resources() {
	run := s.run("oss/gauger", training, "workflow_dispatch", s.now.Add(-48*time.Hour), "")
	s.fx.ResourcesJob = s.gauger(s.job(run, gpuJob, "success"), false).id
	s.finish(run)

	for day := 4; day < 90; day += 6 {
		run := s.run("oss/docs", ci, "push", s.now.Add(-time.Duration(day)*24*time.Hour-3*time.Hour), "")
		s.gauger(s.job(run, ci.jobs[0], "success"), false)
		s.finish(run)
	}
}
