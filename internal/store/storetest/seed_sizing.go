package storetest

import (
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
)

// sizing adds jobs that use a small part of their runner: a test job on
// ubuntu-latest in a private repository and in a public one, and a macOS
// job in a private one, which has no smaller runner.
func (s *seeder) sizing() {
	at := s.now.Add(-2 * 24 * time.Hour)
	add := func(repo string, def seedJobDef) int64 {
		at = at.Add(time.Hour)
		run := s.run(repo, ci, "push", at, "")
		j := s.light(s.job(run, def, "success"))
		s.finish(run)
		return j.id
	}
	s.fx.LightPrivateJob = add("acme/api", ci.jobs[1])
	s.fx.LightPublicJob = add("oss/gauger", ci.jobs[1])
	s.fx.LightMacJob = add("acme/api", ci.jobs[2])
}

// light gives a completed job runner samples that stay under 40% of the
// CPU and of the 16 GiB of memory.
func (s *seeder) light(j *seedJob) *seedJob {
	s.gauger(j, false)
	points := s.samples[j.id]
	for i, p := range points {
		if (p.Metric == store.MetricCPUUtilization && p.Series == "") ||
			(p.Metric == store.MetricMemoryUsage && p.Series == store.SeriesMemoryUsed) {
			points[i].Value = p.Value * 0.4
		}
	}
	return j
}
