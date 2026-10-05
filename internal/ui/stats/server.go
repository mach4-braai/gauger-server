// Package stats serves the dashboard under /stats/: a shell with the
// filters and the sidebar, and one page per route in routes.go.
package stats

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/a-h/templ"
	"github.com/starfederation/datastar-go/datastar"

	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/chart"
	"github.com/mach4-braai/gauger-server/internal/ui/static"
)

// Server holds what the pages read.
type Server struct {
	Store *store.Store
	Rates spend.Rates
	// GitHubURL is the web origin for links to GitHub.
	GitHubURL string
	// Version identifies the build in the sidebar; VersionURL links it.
	Version     string
	VersionURL  string
	FeedbackURL string
	// Now is the clock the filter's window ends at. Nil uses time.Now.
	Now func() time.Time
}

// render builds a page's body from the request and the shell's filter.
// Every page has one. The shell wraps it on a full load, and a Datastar
// request patches it into #page.
type render func(s *Server, r *http.Request, f store.Filter) (templ.Component, error)

type route struct {
	pattern string
	title   string
	render  render
}

// errNotFound from a render answers 404.
var errNotFound = errors.New("not found")

// Handler serves the routes in routes.go under /stats/ and the assets
// under /static/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.Handle("GET "+rt.pattern, gzipped(s.page(rt)))
	}
	mux.Handle("GET /static/", static.Handler())
	return mux
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// page answers a full load with the shell around the page, and a Datastar
// request with patches for #page, #nav and the filter form and a script
// that puts the request's URL in the address bar.
func (s *Server) page(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isDatastar := r.Header.Get("Datastar-Request") == "true"
		stream := r.URL.Query().Get("live") == "1"
		r.URL = canonical(r.URL)
		if stream {
			s.live(w, r, rt)
			return
		}
		f, sel := s.filter(r)
		body, err := rt.render(s, r, f)
		if errors.Is(err, errNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			slog.Error("stats page", "path", r.URL.Path, "err", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		sh := shell{Title: rt.title, Path: r.URL.Path, Sel: sel, Live: liveURL(r.URL), Version: s.Version, VersionURL: s.VersionURL, FeedbackURL: s.FeedbackURL, Body: body}
		if sh.Repos, err = s.Store.Repositories(r.Context()); err == nil {
			sh.Events, err = s.Store.Events(r.Context())
		}
		if err != nil {
			slog.Error("stats shell", "err", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		if isDatastar {
			to, _ := json.Marshal(r.URL.RequestURI())
			sse := datastar.NewSSE(w, r)
			err := errors.Join(
				sse.PatchElementTempl(pageBody(rt.title, body)),
				sse.PatchElementTempl(navList(r.URL.Path, sel)),
				sse.PatchElementTempl(filterBar(sh)),
				sse.PatchElementTempl(liveStream(sh.Live)),
				sse.ExecuteScript("history.replaceState(null, '', "+string(to)+")"),
			)
			if err != nil {
				slog.Error("stats patch", "path", r.URL.Path, "err", err)
			}
			return
		}
		var buf bytes.Buffer
		if err := layout(sh).Render(r.Context(), &buf); err != nil {
			slog.Error("stats render", "path", r.URL.Path, "err", err)
			http.Error(w, "render failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(buf.Bytes())
	})
}

// canonical drops empty parameters and the signals Datastar adds to a
// request, so pages and the address bar see only the page's state.
func canonical(u *url.URL) *url.URL {
	q := u.Query()
	q.Del("datastar")
	q.Del("live")
	for k, vs := range q {
		vs = slices.DeleteFunc(vs, func(v string) bool { return v == "" })
		if len(vs) == 0 {
			q.Del(k)
		} else {
			q[k] = vs
		}
	}
	c := *u
	c.RawQuery = q.Encode()
	return &c
}

// window is one choice of the range picker.
type window struct {
	key, label, prose string
	span              time.Duration
}

var windows = []window{
	{"24h", "24h", "the last 24 hours", 24 * time.Hour},
	{"7d", "7d", "the last 7 days", 7 * 24 * time.Hour},
	{"30d", "30d", "the last 30 days", 30 * 24 * time.Hour},
	{"90d", "90d", "the last 90 days", 90 * 24 * time.Hour},
	{"all", "All", "all time", 0},
}

const defaultRange = "7d"

// selection is the shell's filter as the query has it.
type selection struct {
	Range, Repo, Event string
}

func (sel selection) window() window {
	for _, w := range windows {
		if w.key == sel.Range {
			return w
		}
	}
	return windows[1]
}

// query is the selection as a query string for links between pages,
// empty when everything is at its default.
func (sel selection) query() string {
	q := url.Values{}
	if sel.Range != defaultRange {
		q.Set("range", sel.Range)
	}
	if sel.Repo != "" {
		q.Set("repo", sel.Repo)
	}
	if sel.Event != "" {
		q.Set("event", sel.Event)
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// filter reads range, repo and event from the query. The window ends now.
func (s *Server) filter(r *http.Request) (store.Filter, selection) {
	q := r.URL.Query()
	sel := selection{Range: defaultRange, Repo: q.Get("repo"), Event: q.Get("event")}
	for _, w := range windows {
		if w.key == q.Get("range") {
			sel.Range = w.key
		}
	}
	now := s.now().UTC()
	f := store.Filter{Repository: sel.Repo, Event: sel.Event, Until: now}
	if span := sel.window().span; span > 0 {
		f.Since = now.Add(-span)
	}
	return f, sel
}

// step is the time chart bucket for f: an hour over a day or less, else a
// day.
func step(f store.Filter) chart.Step {
	if !f.Since.IsZero() && f.Until.Sub(f.Since) <= 24*time.Hour {
		return chart.Hour
	}
	return chart.Day
}

// axis lists f's buckets. Over all time it starts at first, the earliest
// bucket with data.
func axis(f store.Filter, st chart.Step, first time.Time) []time.Time {
	since := f.Since
	if since.IsZero() {
		since = first
	}
	return chart.Buckets(since, f.Until, st)
}

// describe says what f covers, for a page's description: "the last 7
// days in acme/api on push".
func describe(f store.Filter) string {
	d := "all time"
	if !f.Since.IsZero() {
		d = "the window from " + f.Since.Format("Jan 2 15:04") + " UTC"
		for _, w := range windows {
			if w.span == f.Until.Sub(f.Since) {
				d = w.prose
			}
		}
	}
	if f.Repository != "" {
		d += " in " + f.Repository
	}
	if f.Event != "" {
		d += " on " + f.Event
	}
	return d
}

// applyFilters is the Datastar expression that reloads the page in place
// from the filter form. Inputs of the form use it for events other than
// change, such as a debounced input.
const applyFilters = "@get(document.getElementById('" + chart.Form + "').action, {contentType: 'form', selector: '#" + chart.Form + "'})"
