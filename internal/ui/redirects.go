package ui

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/mach4-braai/gauger-server/internal/ui/stats"
)

// redirect sends a retired report URL to its dashboard page with 302,
// keeping the query string.
func redirect(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, withQuery(to, r.URL.RawQuery), http.StatusFound)
	}
}

// redirectJob sends /jobs/{id} to /stats/jobs/{id}.
func redirectJob(w http.ResponseWriter, r *http.Request) {
	to := "/stats/jobs/" + url.PathEscape(r.PathValue("id"))
	http.Redirect(w, r, withQuery(to, r.URL.RawQuery), http.StatusFound)
}

// redirectTrends sends /daily and /regressions to /stats/trends. Their
// days become the nearest range that covers them, and the other
// parameters keep their names.
func redirectTrends(w http.ResponseWriter, r *http.Request) {
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

func withQuery(path, rawQuery string) string {
	if rawQuery == "" {
		return path
	}
	return path + "?" + rawQuery
}
