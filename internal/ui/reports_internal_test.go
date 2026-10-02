package ui

import "testing"

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
		if got := workflowURL("https://github.com", "mach4-braai/gauger", c.path); got != c.want {
			t.Fatalf("workflowURL(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}
