package storetest

import (
	"fmt"
	"time"
)

// Dynamic workflow fixtures: DynamicRepo has two pull requests that each
// ran DynamicPath. GitHub names every run of a dynamic workflow after its
// trigger, so the runs carry two different workflow names for one file.
const (
	DynamicRepo = "oss/docs"
	DynamicPath = "dynamic/github-code-scanning/codeql"
)

// DynamicPullRequests are the pull requests whose runs of DynamicPath
// the fixtures hold.
var DynamicPullRequests = []int{17, 18}

// dynamicPullRequests adds one dynamic workflow run per pull request, each
// under its own workflow name.
func (s *seeder) dynamicPullRequests() {
	for i, n := range DynamicPullRequests {
		wf := codeql
		wf.name = fmt.Sprintf("CodeQL for pull request #%d", n)
		at := s.now.Add(-time.Duration(2+i) * 24 * time.Hour)
		run := s.run(DynamicRepo, wf, "dynamic", at, "")
		run.branch = fmt.Sprintf("refs/pull/%d/merge", n)
		s.job(run, wf.jobs[0], "success")
		s.finish(run)
	}
}
