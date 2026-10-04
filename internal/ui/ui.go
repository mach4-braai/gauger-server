// Package ui serves the web UI on the tailnet-only listener.
package ui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mach4-braai/gauger-server/internal/github"
	"github.com/mach4-braai/gauger-server/internal/spend"
	"github.com/mach4-braai/gauger-server/internal/store"
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

	pages map[string]*template.Template
}

func (u *UI) Handler() http.Handler {
	u.pages = map[string]*template.Template{}
	for _, name := range []string{"setup", "setup_redirect", "jobs", "job", "steps", "regressions", "daily", "sizing", "spend"} {
		u.pages[name] = template.Must(template.New("layout.html").Funcs(u.funcs()).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html"))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", u.jobs)
	mux.HandleFunc("GET /jobs/{id}", u.job)
	mux.HandleFunc("GET /steps", u.steps)
	mux.HandleFunc("GET /regressions", u.regressions)
	mux.HandleFunc("GET /daily", u.daily)
	mux.HandleFunc("GET /sizing", u.sizing)
	mux.HandleFunc("GET /spend", u.spend)
	mux.HandleFunc("GET /setup", u.setup)
	mux.HandleFunc("POST /setup/manifest", u.setupManifest)
	mux.HandleFunc("GET /setup/callback", u.setupCallback)
	mux.HandleFunc("POST /setup/backfill", u.setupBackfill)
	return mux
}

// funcs returns the shared template funcs plus the ones bound to this UI.
func (u *UI) funcs() template.FuncMap {
	f := maps.Clone(funcs)
	f["version"] = func() string { return u.Version }
	f["versionURL"] = func() string {
		if !shaRE.MatchString(u.Version) {
			return ""
		}
		return commitURL(u.GitHubURL, "mach4-braai/gauger-server", u.Version)
	}
	return f
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

var funcs = template.FuncMap{
	"secs": func(s float64) string { return fmtDuration(time.Duration(s * float64(time.Second))) },
	"span": func(a, b *time.Time) string {
		if a == nil || b == nil {
			return ""
		}
		return fmtDuration(b.Sub(*a))
	},
	"when": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	"day": func(t time.Time) string { return t.UTC().Format("2006-01-02") },
	"gib": func(v *float64) string {
		return optional(v, func(x float64) string { return fmt.Sprintf("%.2f GiB", x/(1<<30)) })
	},
	"pct": func(v *float64) string {
		return optional(v, func(x float64) string { return fmt.Sprintf("%.0f%%", x*100) })
	},
	"ratio": func(num, den *float64) string {
		if num == nil || den == nil || *den == 0 {
			return ""
		}
		return fmt.Sprintf("%.0f%%", *num / *den * 100)
	},
	"cores": func(util, n *float64) string {
		if util == nil || n == nil {
			return ""
		}
		return fmt.Sprintf("%.1f of %.0f", *util**n, *n)
	},
	"num":   func(v *float64) string { return optional(v, func(x float64) string { return fmt.Sprintf("%.0f", x) }) },
	"times": func(a, b float64) string { return fmt.Sprintf("%.2f×", a/b) },
	"usd":   func(v float64) string { return fmt.Sprintf("$%.2f", v) },
	"join":  strings.Join,
	"month": func(t time.Time) string { return t.UTC().Format("2006-01") },
	"visibility": func(private *bool) string {
		switch {
		case private == nil:
			return "(visibility unknown)"
		case !*private:
			return "(public)"
		}
		return ""
	},
	"workflowURL": workflowURL,
	"runURL":      runURL,
	"commitURL":   commitURL,
	"branchURL":   branchURL,
	"stepURL":     stepURL,
}

func optional(v *float64, f func(float64) string) string {
	if v == nil {
		return ""
	}
	return f(*v)
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(math.Mod(d.Seconds(), 60)))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(math.Mod(d.Minutes(), 60)))
	}
}

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
