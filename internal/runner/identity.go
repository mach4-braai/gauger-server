// Package runner decodes what gauger sends: job identity, OTLP metrics and
// the fallback artifact.
package runner

import (
	"fmt"
	"strconv"
	"strings"
)

// Identity attributes gauger puts on every record. The same names are the
// keys of the lifecycle request bodies.
const (
	AttrRunID      = "github.run_id"
	AttrRunAttempt = "github.run_attempt"
	AttrCheckRunID = "github.check_run_id"
	AttrRepository = "github.repository"
	AttrWorkflow   = "github.workflow"
	AttrJob        = "github.job"
	AttrRunnerName = "runner.name"
)

func isIdentityKey(k string) bool {
	switch k {
	case AttrRunID, AttrRunAttempt, AttrCheckRunID, AttrRepository, AttrWorkflow, AttrJob, AttrRunnerName:
		return true
	}
	return false
}

// Identity names the job a record belongs to. CheckRunID is 0 when gauger
// could not read it; RunnerName then identifies the job.
type Identity struct {
	RunID      int64
	RunAttempt int
	CheckRunID int64
	Repository string
	Workflow   string
	Job        string
	RunnerName string
}

// ParseIdentity reads an Identity through get, which returns "" for a
// missing key.
func ParseIdentity(get func(string) string) (Identity, error) {
	var id Identity
	var err error
	if id.RunID, err = strconv.ParseInt(get(AttrRunID), 10, 64); err != nil || id.RunID <= 0 {
		return Identity{}, fmt.Errorf("%s must be a positive integer", AttrRunID)
	}
	if id.RunAttempt, err = strconv.Atoi(get(AttrRunAttempt)); err != nil || id.RunAttempt <= 0 {
		return Identity{}, fmt.Errorf("%s must be a positive integer", AttrRunAttempt)
	}
	if raw := get(AttrCheckRunID); raw != "" {
		if id.CheckRunID, err = strconv.ParseInt(raw, 10, 64); err != nil || id.CheckRunID < 0 {
			return Identity{}, fmt.Errorf("%s must be an integer", AttrCheckRunID)
		}
	}
	id.Repository = get(AttrRepository)
	if owner, name, ok := strings.Cut(id.Repository, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Identity{}, fmt.Errorf("%s must be owner/name", AttrRepository)
	}
	id.Workflow = get(AttrWorkflow)
	id.Job = get(AttrJob)
	id.RunnerName = get(AttrRunnerName)
	if id.CheckRunID == 0 && id.RunnerName == "" {
		return Identity{}, fmt.Errorf("need %s or %s", AttrCheckRunID, AttrRunnerName)
	}
	return id, nil
}
