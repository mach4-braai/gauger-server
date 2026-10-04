package ui

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/mach4-braai/gauger-server/internal/ui/stats"
)

func (u *UI) home(w http.ResponseWriter, r *http.Request) {
	to := "/stats/runs"
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// job redirects the old job page to the dashboard's, keeping the query.
func (u *UI) job(w http.ResponseWriter, r *http.Request) {
	to := "/stats/jobs/" + url.PathEscape(r.PathValue("id"))
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// steps sends the old slow steps page to the dashboard's Steps page, with
// its query parameters.
func (u *UI) steps(w http.ResponseWriter, r *http.Request) {
	to := "/stats/steps"
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// trendsRedirect sends /daily and /regressions to /stats/trends. Their
// days become the nearest range that covers them, and the other
// parameters keep their names.
func trendsRedirect(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days, err := strconv.Atoi(q.Get("days"))
	if err != nil || days <= 0 {
		days = 14
	}
	to := url.Values{"range": {stats.RangeForDays(days)}}
	for _, key := range []string{"repo", "bucket", "workflow", "job", "ratio", "min"} {
		if v := q.Get(key); v != "" {
			to.Set(key, v)
		}
	}
	http.Redirect(w, r, "/stats/trends?"+to.Encode(), http.StatusFound)
}

// sizing sends the old page to the dashboard's, with its query.
func (u *UI) sizing(w http.ResponseWriter, r *http.Request) {
	to := "/stats/sizing"
	if r.URL.RawQuery != "" {
		to += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, to, http.StatusFound)
}
