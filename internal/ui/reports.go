package ui

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/stats"
)

type filterForm struct {
	Repo  string
	Days  int
	Repos []string
}

func (u *UI) filter(r *http.Request, defaultDays int) (store.Filter, filterForm, error) {
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days <= 0 || days > 3650 {
		days = defaultDays
	}
	f := filterForm{Repo: r.URL.Query().Get("repo"), Days: days}
	f.Repos, err = u.Store.Repositories(r.Context())
	return store.Filter{Repository: f.Repo, Since: time.Now().AddDate(0, 0, -days)}, f, err
}

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

func (u *UI) sizing(w http.ResponseWriter, r *http.Request) {
	f, form, err := u.filter(r, 30)
	if err != nil {
		u.fail(w, err)
		return
	}
	rows, err := u.Store.Sizing(r.Context(), f)
	if err != nil {
		u.fail(w, err)
		return
	}
	u.render(w, "sizing", map[string]any{"Filter": form, "Rows": rows, "GitHubURL": u.GitHubURL})
}
