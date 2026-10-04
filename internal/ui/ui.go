// Package ui serves the web UI on the tailnet-only listener.
package ui

import (
	"bytes"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
	"github.com/mach4-braai/gauger-server/internal/ui/stats"
)

//go:embed templates/*.html
var templateFS embed.FS

type UI struct {
	Store  *store.Store
	GitHub *github.Client
	Creds  github.CredentialSource
	Rates  spend.Rates
	// DNSName is the server's MagicDNS name, used in the App manifest.
	DNSName string
	// GitHubURL is the web origin, https://github.com unless testing.
	GitHubURL string
	// Wake is called after a backfill request adds work for the reconciler.
	Wake func()
	// Version identifies the build, shown in the navbar. Empty hides it.
	Version string
	// Now is the dashboard's clock. Nil uses time.Now.
	Now func() time.Time

	pages map[string]*template.Template
}

func (u *UI) Handler() http.Handler {
	u.pages = map[string]*template.Template{}
	for _, name := range []string{"setup", "setup_redirect"} {
		u.pages[name] = template.Must(template.New("layout.html").Funcs(u.funcs()).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", u.home)
	mux.HandleFunc("GET /jobs/{id}", u.job)
	mux.HandleFunc("GET /steps", u.steps)
	mux.HandleFunc("GET /regressions", trendsRedirect)
	mux.HandleFunc("GET /daily", trendsRedirect)
	mux.HandleFunc("GET /sizing", u.sizing)
	mux.HandleFunc("GET /spend", func(w http.ResponseWriter, r *http.Request) {
		to := url.URL{Path: "/stats/spend", RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, to.String(), http.StatusFound)
	})
	mux.HandleFunc("GET /setup", u.setup)
	mux.HandleFunc("POST /setup/manifest", u.setupManifest)
	mux.HandleFunc("GET /setup/callback", u.setupCallback)
	mux.HandleFunc("POST /setup/backfill", u.setupBackfill)
	dashboard := (&stats.Server{
		Store: u.Store, Rates: u.Rates, GitHubURL: u.GitHubURL,
		Version: u.Version, VersionURL: u.versionURL(), Now: u.Now,
	}).Handler()
	mux.Handle("/stats/", dashboard)
	mux.Handle("/static/", dashboard)
	return mux
}

// funcs returns the template funcs, which are bound to this UI.
func (u *UI) funcs() template.FuncMap {
	return template.FuncMap{
		"version":    func() string { return u.Version },
		"versionURL": u.versionURL,
	}
}

// versionURL links a commit-hash Version to its commit, or is empty.
func (u *UI) versionURL() string {
	if !shaRE.MatchString(u.Version) {
		return ""
	}
	return github.CommitURL(u.GitHubURL, "mach4-braai/gauger-server", u.Version)
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func (u *UI) render(w http.ResponseWriter, page string, data any) {
	var buf bytes.Buffer
	if err := u.pages[page].Execute(&buf, data); err != nil {
		slog.Error("render", "page", page, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (u *UI) fail(w http.ResponseWriter, err error) {
	slog.Error("ui query", "err", err)
	http.Error(w, "query failed", http.StatusInternalServerError)
}
