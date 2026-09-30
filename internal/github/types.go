package github

import "time"

// Job is a workflow job as REST and the workflow_job webhook both return it.
type Job struct {
	ID              int64      `json:"id"`
	RunID           int64      `json:"run_id"`
	RunAttempt      int        `json:"run_attempt"`
	WorkflowName    string     `json:"workflow_name"`
	HeadBranch      string     `json:"head_branch"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	Conclusion      string     `json:"conclusion"`
	Labels          []string   `json:"labels"`
	RunnerName      string     `json:"runner_name"`
	RunnerGroupName string     `json:"runner_group_name"`
	HTMLURL         string     `json:"html_url"`
	CreatedAt       *time.Time `json:"created_at"`
	StartedAt       *time.Time `json:"started_at"`
	CompletedAt     *time.Time `json:"completed_at"`
	Steps           []Step     `json:"steps"`
}

type Step struct {
	Number      int        `json:"number"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// Run is a workflow run attempt.
type Run struct {
	ID           int64      `json:"id"`
	RunAttempt   int        `json:"run_attempt"`
	Name         string     `json:"name"`
	HeadBranch   string     `json:"head_branch"`
	HeadSHA      string     `json:"head_sha"`
	Event        string     `json:"event"`
	Status       string     `json:"status"`
	Conclusion   string     `json:"conclusion"`
	HTMLURL      string     `json:"html_url"`
	CreatedAt    *time.Time `json:"created_at"`
	RunStartedAt *time.Time `json:"run_started_at"`
	UpdatedAt    *time.Time `json:"updated_at"`
	Repository   Repository `json:"repository"`
}

type Repository struct {
	FullName string `json:"full_name"`
	Private  *bool  `json:"private"`
}

type Installation struct {
	ID int64 `json:"id"`
}

type WorkflowRunEvent struct {
	Action       string       `json:"action"`
	WorkflowRun  Run          `json:"workflow_run"`
	Repository   Repository   `json:"repository"`
	Installation Installation `json:"installation"`
}

type WorkflowJobEvent struct {
	Action       string       `json:"action"`
	WorkflowJob  Job          `json:"workflow_job"`
	Repository   Repository   `json:"repository"`
	Installation Installation `json:"installation"`
}

type Artifact struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Expired   bool       `json:"expired"`
	ExpiresAt *time.Time `json:"expires_at"`
}
