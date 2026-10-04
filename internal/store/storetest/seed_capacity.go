package storetest

import (
	"fmt"
	"time"
)

// CapacityRepo holds the capacity fixtures alone: filter by it to see only
// those jobs.
const CapacityRepo = "acme/capacity"

// CapacityDay is day 0, the UTC day of the capacity fixtures for fixtures
// made at now. Two jobs overlap on it, 10:15 to 10:30, and a third runs
// alone at 14:00, so its peak concurrency is 2. A job on day 1 runs from
// 11:05 to 13:40, so the 12:00 hour has no start or end and a peak of 1.
// Two jobs on day 2 end and start at the same instant, 09:30, and do not
// overlap, so its peak is 1.
func CapacityDay(now time.Time) time.Time {
	return now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -20)
}

// capacity adds the jobs of CapacityRepo.
func (s *seeder) capacity() {
	day := CapacityDay(s.now)
	at := func(d, h, m, sec int) time.Time {
		return day.AddDate(0, 0, d).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second)
	}
	sha := func(n int) string { return fmt.Sprintf("%040x", n) }

	morning := s.run(CapacityRepo, ci, "push", at(0, 10, 10, 0), sha(1))
	s.span(morning, ci.jobs[1], at(0, 10, 10, 5), at(0, 10, 30, 0))
	s.span(morning, ci.jobs[2], at(0, 10, 15, 0), at(0, 10, 45, 0))
	s.finish(morning)

	afternoon := s.run(CapacityRepo, ci, "push", at(0, 14, 0, 0), sha(2))
	s.span(afternoon, ci.jobs[0], at(0, 14, 0, 20), at(0, 14, 10, 0))
	s.finish(afternoon)

	long := s.run(CapacityRepo, ci, "push", at(1, 11, 5, 0), sha(3))
	s.span(long, ci.jobs[1], at(1, 11, 5, 5), at(1, 13, 40, 0))
	s.finish(long)

	first := s.run(CapacityRepo, ci, "push", at(2, 9, 0, 0), sha(4))
	s.span(first, ci.jobs[0], at(2, 9, 0, 5), at(2, 9, 30, 0))
	s.finish(first)

	second := s.run(CapacityRepo, ci, "push", at(2, 9, 29, 50), sha(5))
	s.span(second, ci.jobs[0], at(2, 9, 30, 0), at(2, 10, 0, 0))
	s.finish(second)
}

// span adds a successful job of run r that started and completed at the
// given times, with no steps.
func (s *seeder) span(r *seedRun, def seedJobDef, start, end time.Time) *seedJob {
	s.jobID++
	j := &seedJob{
		id: s.jobID, run: r, name: def.name, labels: def.labels,
		status: "completed", conclusion: "success", created: r.created, started: &start, completed: &end,
	}
	s.jobs = append(s.jobs, j)
	r.jobs = append(r.jobs, j)
	return j
}
