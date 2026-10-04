package github

import (
	"fmt"
	"strings"
)

// WorkflowURL builds the GitHub URL for a workflow's runs from the path
// stored on its runs, stripping the ".github/workflows/" or "dynamic/"
// prefix GitHub puts on it. Empty path means no link.
func WorkflowURL(ghURL, repo, path string) string {
	if path == "" {
		return ""
	}
	file := strings.TrimPrefix(path, ".github/workflows/")
	file = strings.TrimPrefix(file, "dynamic/")
	return ghURL + "/" + repo + "/actions/workflows/" + file
}

func RunURL(ghURL, repo string, runID int64, attempt int) string {
	return fmt.Sprintf("%s/%s/actions/runs/%d/attempts/%d", ghURL, repo, runID, attempt)
}

func CommitURL(ghURL, repo, sha string) string {
	return fmt.Sprintf("%s/%s/commit/%s", ghURL, repo, sha)
}

func BranchURL(ghURL, repo, branch string) string {
	return fmt.Sprintf("%s/%s/tree/%s", ghURL, repo, branch)
}

// StepURL is the step's log, the job's html_url plus its step anchor.
// Empty html_url means no link.
func StepURL(htmlURL string, number int) string {
	if htmlURL == "" {
		return ""
	}
	return fmt.Sprintf("%s#step:%d:1", htmlURL, number)
}
