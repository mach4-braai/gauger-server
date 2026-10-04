package stats

import "github.com/a-h/templ"

type navItem struct {
	group string
	path  string
	label string
	icon  func() templ.Component
}

// nav is the sidebar, one line per entry, in screen order. Consecutive
// entries with the same group share a heading. Each page defines its icon
// next to its template.
var nav = []navItem{
	{"Usage", "/stats/", "Overview", overviewIcon},
	{"Activity", "/stats/runs", "Runs", runsIcon},
	{"Performance", "/stats/trends", "Trends", trendsIcon},
	{"Performance", "/stats/steps", "Steps", stepsIcon},
}
