package github_test

import (
	"testing"

	"github.com/mach4-braai/gauger-server/internal/github"
)

func TestWorkflowURLStripsPathPrefix(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{".github/workflows/ci.yml", "https://github.com/mach4-braai/gauger/actions/workflows/ci.yml"},
		{"dynamic/dependabot/update-graph", "https://github.com/mach4-braai/gauger/actions/workflows/dependabot/update-graph"},
		{"", ""},
	}
	for _, c := range cases {
		if got := github.WorkflowURL("https://github.com", "mach4-braai/gauger", c.path); got != c.want {
			t.Fatalf("WorkflowURL(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
