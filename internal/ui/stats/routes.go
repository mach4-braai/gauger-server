package stats

// routes lists the dashboard's pages, one line each: pattern, title and
// the page's render function.
var routes = []route{
	{"/stats/{$}", "Overview", (*Server).overview},
	{"/stats/jobs/{id}", "Job", (*Server).job},
}
