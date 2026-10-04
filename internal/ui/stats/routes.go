package stats

// routes lists the dashboard's pages, one line each: pattern, title and
// the page's render function.
var routes = []route{
	{"/stats/{$}", "Overview", (*Server).overview},
	{"/stats/jobs/{id}", "Job", (*Server).job},
	{"/stats/runs", "Runs", (*Server).runs},
	{"/stats/trends", "Trends", (*Server).trends},
	{"/stats/steps", "Steps", (*Server).steps},
}
